package broker

// decision_evidence.go — GetDecisionEvidence RPC: exports one audit event with
// what an auditor needs to re-check it offline (docs/15-decision-replay.md).

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// EvidenceKind identifies the exported evidence format.
const EvidenceKind = "aikonos.decision-evidence/v1"

// evidenceReader is the audit-store surface GetDecisionEvidence needs.
// *audit.Reader satisfies it; Deps.AuditReader is asserted to it so the
// narrower ReaderIface fakes elsewhere stay unchanged.
type evidenceReader interface {
	GetEvent(ctx context.Context, tenant, eventID string) (*auditv1.AuditEvent, []byte, bool, error)
	GetPolicyBundle(ctx context.Context, revision string) ([]byte, bool, error)
	VerifyEventSignature(ctx context.Context, e *auditv1.AuditEvent) audit.SignatureStatus
}

// decisionEvidence is the exported document.
type decisionEvidence struct {
	Kind       string           `json:"kind"`
	ExportedAt string           `json:"exported_at"`
	TenantID   string           `json:"tenant_id"`
	Event      json.RawMessage  `json:"event"`      // exactly as stored
	EventHash  string           `json:"event_hash"` // what the next event in the chain records as prior_event_hash
	Signature  string           `json:"signature"`  // valid | invalid | unsigned | unverified
	Policies   []policyEvidence `json:"policies"`   // every policy revision the decision names
}

// policyEvidence is one archived policy snapshot the decision names.
type policyEvidence struct {
	Revision string          `json:"revision"`
	Verified bool            `json:"verified"` // the snapshot's files hash to Revision
	Error    string          `json:"error,omitempty"`
	Snapshot json.RawMessage `json:"snapshot,omitempty"` // exactly as archived
}

func (s *BrokerService) GetDecisionEvidence(ctx context.Context, req *brokerv1.GetDecisionEvidenceRequest) (*brokerv1.GetDecisionEvidenceResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.admin.get_decision_evidence")
	defer span.End()

	// Tenant comes only from the verified caller identity — never from the request.
	tenant, user, err := s.callerIdentity(ctx, "", "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	if req.GetEventId() == "" {
		return nil, status.Error(codes.InvalidArgument, "event_id is required")
	}
	reader, ok := s.deps.AuditReader.(evidenceReader)
	if !ok || s.deps.AuditReader == nil {
		return nil, status.Error(codes.FailedPrecondition, "the audit store is not configured")
	}

	event, stored, _, err := reader.GetEvent(ctx, tenant, req.GetEventId())
	switch {
	case errors.Is(err, audit.ErrNotFound):
		return nil, status.Errorf(codes.NotFound, "event %s not found", req.GetEventId())
	case err != nil:
		s.deps.Logger.Warn("GetDecisionEvidence: reading the event failed", zap.String("event_id", req.GetEventId()), zap.Error(err))
		return nil, status.Error(codes.Internal, "failed to read the audit event")
	}

	ev := decisionEvidence{
		Kind:       EvidenceKind,
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		TenantID:   tenant,
		Event:      json.RawMessage(stored),
		EventHash:  audit.EventHash(event),
		Signature:  string(reader.VerifyEventSignature(ctx, event)),
		Policies:   []policyEvidence{},
	}
	for _, rev := range eventRevisions(event) {
		pe := policyEvidence{Revision: rev}
		snap, _, err := reader.GetPolicyBundle(ctx, rev)
		switch {
		case err != nil:
			pe.Error = "snapshot unavailable: " + err.Error()
		default:
			pe.Snapshot = json.RawMessage(snap)
			if got, verr := policy.VerifySnapshot(snap); verr != nil {
				pe.Error = verr.Error()
			} else {
				pe.Verified = got == rev
			}
		}
		ev.Policies = append(ev.Policies, pe)
	}

	out, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to encode the evidence")
	}
	s.emitEvidenceExportedAudit(ctx, tenant, user, req.GetEventId())
	return &brokerv1.GetDecisionEvidenceResponse{EvidenceJson: out}, nil
}

// eventRevisions lists the distinct policy revisions an event's decision
// record names: the core OPA evaluation's and any extra gate's.
func eventRevisions(e *auditv1.AuditEvent) []string {
	seen := map[string]bool{}
	opa := e.GetContext().GetFields()["opa"].GetStructValue()
	if rev := opa.GetFields()["revision"].GetStringValue(); rev != "" {
		seen[rev] = true
	}
	for _, g := range opa.GetFields()["gates"].GetListValue().GetValues() {
		if rev := g.GetStructValue().GetFields()["revision"].GetStringValue(); rev != "" {
			seen[rev] = true
		}
	}
	revs := make([]string, 0, len(seen))
	for r := range seen {
		revs = append(revs, r)
	}
	sort.Strings(revs)
	return revs
}

// emitEvidenceExportedAudit records the export itself (fire-and-forget).
func (s *BrokerService) emitEvidenceExportedAudit(ctx context.Context, tenant, actor, eventID string) {
	if s.deps.Audit == nil {
		return
	}
	ctxStruct, _ := structpb.NewStruct(map[string]any{"event_id": eventID})
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     trace.SpanContextFromContext(ctx).TraceID().String(),
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: actor,
		EventType:   "aikonos.broker.decision.evidence_exported",
		ResourceRef: "aikonos:audit:" + eventID,
		Decision:    auditv1.PolicyDecision_ALLOW,
		Context:     ctxStruct,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, "aikonos.broker.decision.evidence_exported")
	}
}
