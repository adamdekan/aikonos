// broker/internal/provisioning/provisioner.go
// JIT provisioner: on first verified north call, matches the caller's email
// against provisioning rules and writes FGA group-membership tuples.
package provisioning

import (
	"context"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/auth"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// IsDuplicateTuple reports whether an FGA WriteTuple error is a tolerated
// already_exists condition. The FGA client surfaces this as a string match
// because the OpenFGA error type is not exported through our seam.
func IsDuplicateTuple(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already_exists")
}

// RuleStore is the DB seam consumed by the Provisioner.
type RuleStore interface {
	ListRules(ctx context.Context, tenantID string) ([]db.ProvisioningRule, error)
	ClaimProvisioning(ctx context.Context, tenantID, subject, email, name string) (bool, error)
}

// TupleWriter is the FGA write seam (one tuple at a time for easy per-tuple
// error handling, matching the provisioner's group-by-group loop).
type TupleWriter interface {
	WriteTuple(ctx context.Context, user, relation, object string) error
}

// AuditEmitter is the subset of audit.Emitter used here.
type AuditEmitter interface {
	Emit(ctx context.Context, event *auditv1.AuditEvent) error
}

// Provisioner applies JIT provisioning rules to new users on first north call.
type Provisioner struct {
	store  RuleStore
	fga    TupleWriter // nil → FGA disabled, Seen is a no-op
	audit  AuditEmitter
	logger *zap.Logger

	mu      sync.Mutex
	seenSet map[string]struct{} // key = tenantID+"/"+subject; process-lifetime
}

// NewProvisioner constructs a Provisioner. Passing nil for fga disables
// provisioning (Seen becomes a no-op), which matches the FGA-disabled
// dev stub path.
func NewProvisioner(store RuleStore, fga TupleWriter, audit AuditEmitter, logger *zap.Logger) *Provisioner {
	return &Provisioner{
		store:   store,
		fga:     fga,
		audit:   audit,
		logger:  logger,
		seenSet: make(map[string]struct{}),
	}
}

// Seen fires the provisioning flow for id in a background goroutine (5 s
// timeout). It never blocks the caller. Safe to call from every north RPC.
func (p *Provisioner) Seen(ctx context.Context, tenantID string, id *auth.Identity) {
	if p.fga == nil {
		return // FGA disabled — provisioning would silently no-op; skip entirely.
	}
	if id == nil || id.Subject == "" || strings.HasPrefix(id.Subject, "svc-") || id.Email == "" {
		return
	}

	key := tenantID + "/" + id.Subject
	p.mu.Lock()
	_, already := p.seenSet[key]
	if !already {
		p.seenSet[key] = struct{}{}
	}
	p.mu.Unlock()
	if already {
		return
	}

	// Snapshot for the goroutine — id fields are plain strings, safe to copy.
	subject := id.Subject
	email := id.Email
	name := id.Name

	go func() {
		gCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p.provision(gCtx, tenantID, subject, email, name)
	}()
}

func (p *Provisioner) removeSeen(tenantID, subject string) {
	key := tenantID + "/" + subject
	p.mu.Lock()
	delete(p.seenSet, key)
	p.mu.Unlock()
}

func (p *Provisioner) provision(ctx context.Context, tenantID, subject, email, name string) {
	// Order: ListRules → MatchRules → ClaimProvisioning → write tuples → audit.
	// ListRules and ClaimProvisioning failures clear the seen-set entry so the
	// next call retries the full flow; that guards against transient DB errors
	// consuming the claim permanently.
	rules, err := p.store.ListRules(ctx, tenantID)
	if err != nil {
		p.logger.Warn("provisioning: ListRules failed",
			zap.String("tenant", tenantID), zap.String("subject", subject), zap.Error(err))
		p.removeSeen(tenantID, subject)
		return
	}

	groups := MatchRules(email, rules)

	claimed, err := p.store.ClaimProvisioning(ctx, tenantID, subject, email, name)
	if err != nil {
		p.logger.Warn("provisioning: ClaimProvisioning failed",
			zap.String("tenant", tenantID), zap.String("subject", subject), zap.Error(err))
		p.removeSeen(tenantID, subject)
		return
	}
	if !claimed {
		return // already provisioned by this or another replica; keep seen-set entry
	}

	// Claim consumed. Write tuples only when there are matching groups; audit
	// is skipped for empty-match (nothing to record).
	if len(groups) == 0 {
		return
	}

	userRef := "user:" + subject
	for _, g := range groups {
		if err := p.fga.WriteTuple(ctx, userRef, "member", "group:"+g); err != nil {
			// Tolerate duplicate-tuple errors — the tuple may already exist from
			// a manual assignment via the Roles tab or a prior crashed run.
			// fga.write() absorbs write_failed_due_to_invalid_input internally;
			// only already_exists can escape to here.
			if IsDuplicateTuple(err) {
				p.logger.Debug("provisioning: tuple already exists (tolerated)",
					zap.String("subject", subject), zap.String("group", g))
				continue
			}
			p.logger.Warn("provisioning: WriteTuple failed",
				zap.String("subject", subject), zap.String("group", g), zap.Error(err))
			continue // best-effort: keep writing other groups
		}
	}

	p.emitAudit(ctx, tenantID, subject, groups)
}

func (p *Provisioner) emitAudit(ctx context.Context, tenantID, subject string, groups []string) {
	if p.audit == nil {
		return
	}
	ctxStruct, _ := structpb.NewStruct(map[string]any{
		"subject": subject,
		"groups":  strings.Join(groups, ","),
	})
	ev := &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TenantId:    tenantID,
		OccurredAt:  timestamppb.Now(),
		ActorUserId: subject,
		EventType:   "user.provisioned",
		ResourceRef: "user:" + subject,
		Decision:    auditv1.PolicyDecision_ALLOW,
		Context:     ctxStruct,
	}
	if err := p.audit.Emit(ctx, ev); err != nil {
		p.logger.Warn("provisioning: audit emit failed", zap.Error(err))
	}
}
