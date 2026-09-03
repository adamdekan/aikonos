package broker

// spend_caps.go — admin CRUD + dashboard summary for monthly LLM spend caps
//, plus the small TTL cache the CheckRateLimit
// spend-cap gate (rate_limits.go) reads through.

import (
	"context"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// SpendCapStore is the minimal db.SpendCapRepo surface the CRUD RPCs and the
// enforcement gate need. *db.SpendCapRepo satisfies this; tests inject a fake.
type SpendCapStore interface {
	List(ctx context.Context, tenant string) ([]db.SpendCap, error)
	Upsert(ctx context.Context, tenant string, c db.SpendCap) (*db.SpendCap, error)
	Delete(ctx context.Context, tenant, id string) error
	Get(ctx context.Context, tenant string, scope db.SpendCapScope, subjectID string) (int64, bool, error)
}

// SpendCounterStore is the minimal db.SpendCounterRepo surface EmitLlmUsage's
// accumulate, the enforcement gate's rollup reads, and GetSpendSummary need.
// *db.SpendCounterRepo satisfies this; tests inject a fake.
type SpendCounterStore interface {
	Accumulate(ctx context.Context, tenant, userID, agentID string, periodStart time.Time, costMicros, tokensIn, tokensOut int64) error
	OrgTotal(ctx context.Context, tenant string, periodStart time.Time) (int64, error)
	UserTotals(ctx context.Context, tenant string, periodStart time.Time) ([]db.SubjectSpend, error)
	AgentTotals(ctx context.Context, tenant string, periodStart time.Time) ([]db.SubjectSpend, error)
	SubjectTotal(ctx context.Context, tenant string, scope db.SpendCapScope, subjectID string, periodStart time.Time) (int64, error)
}

// ── Rollup cache ─────────────────────────────────────────────────────────────

const spendCacheTTL = 30 * time.Second

type spendCacheEntry struct {
	capMicros   int64
	hasCap      bool
	spendMicros int64
	expiresAt   time.Time
}

// spendRollupCache is a small in-process TTL cache over the (cap, current-
// period spend) pair CheckRateLimit needs per (tenant, scope, subject) —
// avoids a DB round trip on every LLM call. Sound only because deployment is
// single-broker (singleton.go's advisory lock); a multi-replica deployment
// would need a shared cache instead.
// ponytail: entries are never pruned on expiry, only overwritten on next
// access — ceiling is O(distinct tenant|scope|subject keys seen since
// process start). Fine at current scale; add a periodic sweep if that grows
// unbounded (e.g. per-agent caps across many short-lived agents).
type spendRollupCache struct {
	mu      sync.Mutex
	entries map[string]spendCacheEntry
}

// NewSpendRollupCache constructs the cache for Deps.SpendCache. Exported so
// main.go (a different package) can wire it without needing to name the
// unexported spendRollupCache type.
func NewSpendRollupCache() *spendRollupCache {
	return &spendRollupCache{entries: map[string]spendCacheEntry{}}
}

func spendCacheKey(tenant string, scope db.SpendCapScope, subject string) string {
	return tenant + "|" + scope + "|" + subject
}

func (c *spendRollupCache) get(key string) (spendCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || time.Now().After(e.expiresAt) {
		return spendCacheEntry{}, false
	}
	return e, true
}

func (c *spendRollupCache) set(key string, e spendCacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]spendCacheEntry{}
	}
	e.expiresAt = time.Now().Add(spendCacheTTL)
	c.entries[key] = e
}

// invalidateTenant drops every cached entry for tenant — called on any
// spend-cap mutation so a lowered/removed cap takes effect immediately
// instead of waiting out the TTL.
func (c *spendRollupCache) invalidateTenant(tenant string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prefix := tenant + "|"
	for k := range c.entries {
		if strings.HasPrefix(k, prefix) {
			delete(c.entries, k)
		}
	}
}

// ── Enforcement (south, called from rate_limits.go's CheckRateLimit) ───────────

// checkSpendCaps evaluates org, then user (if userID set), then agent (if
// agentID set) monthly spend caps, denying on the first cap whose current-
// period spend is at or over it. Unset caps never deny. Nil repos disable
// the check entirely (spend caps not configured).
func (d Deps) checkSpendCaps(ctx context.Context, tenant, userID, agentID string) (allowed bool, limitType string) {
	if d.SpendCaps == nil || d.SpendCounters == nil {
		return true, ""
	}
	periodStart := db.SpendPeriodStart(time.Now())
	if d.spendCapBreached(ctx, tenant, db.SpendCapScopeOrg, "", periodStart) {
		return false, "spend_org"
	}
	if userID != "" && d.spendCapBreached(ctx, tenant, db.SpendCapScopeUser, userID, periodStart) {
		return false, "spend_user"
	}
	if agentID != "" && d.spendCapBreached(ctx, tenant, db.SpendCapScopeAgent, agentID, periodStart) {
		return false, "spend_agent"
	}
	return true, ""
}

func (d Deps) spendCapBreached(ctx context.Context, tenant string, scope db.SpendCapScope, subject string, periodStart time.Time) bool {
	capMicros, spendMicros, hasCap := d.spendRollup(ctx, tenant, scope, subject, periodStart)
	if !hasCap {
		return false
	}
	return spendMicros >= capMicros
}

// spendRollup returns (cap, spend, hasCap) for (tenant, scope, subject),
// serving from the TTL cache when fresh and populating it on a miss.
func (d Deps) spendRollup(ctx context.Context, tenant string, scope db.SpendCapScope, subject string, periodStart time.Time) (int64, int64, bool) {
	key := spendCacheKey(tenant, scope, subject)
	if d.SpendCache != nil {
		if e, ok := d.SpendCache.get(key); ok {
			return e.capMicros, e.spendMicros, e.hasCap
		}
	}
	capMicros, hasCap, err := d.SpendCaps.Get(ctx, tenant, scope, subject)
	if err != nil {
		d.Logger.Warn("spendRollup: cap lookup failed", zap.Error(err))
		return 0, 0, false
	}
	spendMicros, err := d.SpendCounters.SubjectTotal(ctx, tenant, scope, subject, periodStart)
	if err != nil {
		d.Logger.Warn("spendRollup: spend lookup failed", zap.Error(err))
		return 0, 0, false
	}
	if d.SpendCache != nil {
		d.SpendCache.set(key, spendCacheEntry{capMicros: capMicros, hasCap: hasCap, spendMicros: spendMicros})
	}
	return capMicros, spendMicros, hasCap
}

// ── North admin RPCs ─────────────────────────────────────────────────────────

func (s *BrokerService) ListSpendCaps(ctx context.Context, req *brokerv1.ListSpendCapsRequest) (*brokerv1.ListSpendCapsResponse, error) {
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	if s.deps.SpendCaps == nil {
		return &brokerv1.ListSpendCapsResponse{}, nil
	}
	rows, err := s.deps.SpendCaps.List(ctx, tenant)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list spend caps: %v", err)
	}
	out := make([]*brokerv1.SpendCap, 0, len(rows))
	for _, r := range rows {
		out = append(out, spendCapToProto(r))
	}
	return &brokerv1.ListSpendCapsResponse{Caps: out}, nil
}

func (s *BrokerService) SetSpendCap(ctx context.Context, req *brokerv1.SetSpendCapRequest) (*brokerv1.SetSpendCapResponse, error) {
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	if s.deps.SpendCaps == nil {
		return nil, status.Error(codes.FailedPrecondition, "spend caps not configured")
	}
	if err := validateSpendCapScope(req.Scope, req.SubjectId); err != nil {
		return nil, err
	}
	if req.CapMicros <= 0 {
		return nil, status.Error(codes.InvalidArgument, "cap_micros must be a positive integer")
	}

	saved, err := s.deps.SpendCaps.Upsert(ctx, tenant, db.SpendCap{
		Scope:     req.Scope,
		SubjectID: req.SubjectId,
		CapMicros: req.CapMicros,
		CreatedBy: user,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "upsert spend cap: %v", err)
	}
	if s.deps.SpendCache != nil {
		s.deps.SpendCache.invalidateTenant(tenant)
	}

	s.emitSpendCapAudit(ctx, tenant, user, "spend_cap.set", map[string]any{
		"scope":      req.Scope,
		"subject_id": req.SubjectId,
		"cap_micros": req.CapMicros,
	})
	return &brokerv1.SetSpendCapResponse{Id: saved.ID.String()}, nil
}

func (s *BrokerService) DeleteSpendCap(ctx context.Context, req *brokerv1.DeleteSpendCapRequest) (*brokerv1.DeleteSpendCapResponse, error) {
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	if s.deps.SpendCaps == nil {
		return nil, status.Error(codes.FailedPrecondition, "spend caps not configured")
	}
	if req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "id required")
	}
	if err := s.deps.SpendCaps.Delete(ctx, tenant, req.Id); err != nil {
		return nil, status.Errorf(codes.Internal, "delete spend cap: %v", err)
	}
	if s.deps.SpendCache != nil {
		s.deps.SpendCache.invalidateTenant(tenant)
	}

	s.emitSpendCapAudit(ctx, tenant, user, "spend_cap.delete", map[string]any{"id": req.Id})
	return &brokerv1.DeleteSpendCapResponse{}, nil
}

func (s *BrokerService) GetSpendSummary(ctx context.Context, req *brokerv1.GetSpendSummaryRequest) (*brokerv1.GetSpendSummaryResponse, error) {
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	if s.deps.SpendCounters == nil {
		return &brokerv1.GetSpendSummaryResponse{}, nil
	}

	periodStart := db.SpendPeriodStart(time.Now())
	orgSpend, err := s.deps.SpendCounters.OrgTotal(ctx, tenant, periodStart)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "org spend total: %v", err)
	}

	// caps keyed "scope|subject_id" — populates the per-user/agent cap_micros
	// column (0 = unset), and the org cap separately.
	caps := map[string]int64{}
	var orgCap int64
	var capRows []db.SpendCap
	if s.deps.SpendCaps != nil {
		rows, err := s.deps.SpendCaps.List(ctx, tenant)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list spend caps: %v", err)
		}
		capRows = rows
		for _, c := range rows {
			if c.Scope == db.SpendCapScopeOrg {
				orgCap = c.CapMicros
				continue
			}
			caps[c.Scope+"|"+c.SubjectID] = c.CapMicros
		}
	}

	userTotals, err := s.deps.SpendCounters.UserTotals(ctx, tenant, periodStart)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "user spend totals: %v", err)
	}
	userRows := make([]*brokerv1.SpendSummaryUserRow, 0, len(userTotals))
	seenUsers := make(map[string]bool, len(userTotals))
	for _, u := range userTotals {
		seenUsers[u.SubjectID] = true
		userRows = append(userRows, &brokerv1.SpendSummaryUserRow{
			UserId:      u.SubjectID,
			SpendMicros: u.CostMicros,
			CapMicros:   caps[db.SpendCapScopeUser+"|"+u.SubjectID],
		})
	}

	agentTotals, err := s.deps.SpendCounters.AgentTotals(ctx, tenant, periodStart)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "agent spend totals: %v", err)
	}
	agentRows := make([]*brokerv1.SpendSummaryAgentRow, 0, len(agentTotals))
	seenAgents := make(map[string]bool, len(agentTotals))
	for _, a := range agentTotals {
		seenAgents[a.SubjectID] = true
		agentRows = append(agentRows, &brokerv1.SpendSummaryAgentRow{
			AgentId:     a.SubjectID,
			SpendMicros: a.CostMicros,
			CapMicros:   caps[db.SpendCapScopeAgent+"|"+a.SubjectID],
		})
	}

	// A configured user/agent cap with no spend this period has no totals row
	// (UserTotals/AgentTotals only return subjects with usage) — union in a
	// spend-0 row for every such cap so it doesn't silently vanish from the
	// dashboard.
	for _, c := range capRows {
		switch c.Scope {
		case db.SpendCapScopeUser:
			if !seenUsers[c.SubjectID] {
				userRows = append(userRows, &brokerv1.SpendSummaryUserRow{UserId: c.SubjectID, CapMicros: c.CapMicros})
			}
		case db.SpendCapScopeAgent:
			if !seenAgents[c.SubjectID] {
				agentRows = append(agentRows, &brokerv1.SpendSummaryAgentRow{AgentId: c.SubjectID, CapMicros: c.CapMicros})
			}
		}
	}

	return &brokerv1.GetSpendSummaryResponse{
		OrgSpendMicros: orgSpend,
		OrgCapMicros:   orgCap,
		Users:          userRows,
		Agents:         agentRows,
	}, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func validateSpendCapScope(scope, subjectID string) error {
	switch scope {
	case db.SpendCapScopeOrg:
		if subjectID != "" {
			return status.Error(codes.InvalidArgument, "subject_id must be empty for scope=org")
		}
	case db.SpendCapScopeUser, db.SpendCapScopeAgent:
		if subjectID == "" {
			return status.Errorf(codes.InvalidArgument, "subject_id required for scope=%s", scope)
		}
	default:
		return status.Errorf(codes.InvalidArgument, "scope must be one of org|user|agent, got %q", scope)
	}
	return nil
}

func spendCapToProto(c db.SpendCap) *brokerv1.SpendCap {
	return &brokerv1.SpendCap{
		Id:        c.ID.String(),
		TenantId:  c.TenantID.String(),
		Scope:     c.Scope,
		SubjectId: c.SubjectID,
		CapMicros: c.CapMicros,
		CreatedBy: c.CreatedBy,
	}
}

// emitSpendCapAudit fires an ALLOW audit event for spend-cap CRUD mutations.
func (s *BrokerService) emitSpendCapAudit(ctx context.Context, tenant, actor, eventType string, fields map[string]any) {
	if s.deps.Audit == nil {
		return
	}
	ctxStruct, _ := structpb.NewStruct(fields)
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: actor,
		EventType:   eventType,
		Decision:    auditv1.PolicyDecision_ALLOW,
		Context:     ctxStruct,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, eventType)
	}
}
