package broker

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ── fakeSpendCapStore — injectable SpendCap repo for handler/enforcement tests ─

type fakeSpendCapStore struct {
	mu   sync.Mutex
	caps map[string]db.SpendCap // "tenant|scope|subject" → row
}

func newFakeSpendCapStore() *fakeSpendCapStore {
	return &fakeSpendCapStore{caps: map[string]db.SpendCap{}}
}

func spendCapFakeKey(tenant string, scope db.SpendCapScope, subject string) string {
	return tenant + "|" + scope + "|" + subject
}

func (f *fakeSpendCapStore) List(_ context.Context, tenant string) ([]db.SpendCap, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []db.SpendCap
	for k, c := range f.caps {
		if strings.HasPrefix(k, tenant+"|") {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeSpendCapStore) Upsert(_ context.Context, tenant string, c db.SpendCap) (*db.SpendCap, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := spendCapFakeKey(tenant, c.Scope, c.SubjectID)
	if existing, ok := f.caps[key]; ok {
		c.ID = existing.ID
	} else {
		c.ID = uuid.New()
	}
	f.caps[key] = c
	out := c
	return &out, nil
}

func (f *fakeSpendCapStore) Delete(_ context.Context, tenant, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, c := range f.caps {
		if strings.HasPrefix(k, tenant+"|") && c.ID.String() == id {
			delete(f.caps, k)
		}
	}
	return nil
}

func (f *fakeSpendCapStore) Get(_ context.Context, tenant string, scope db.SpendCapScope, subjectID string) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.caps[spendCapFakeKey(tenant, scope, subjectID)]
	if !ok {
		return 0, false, nil
	}
	return c.CapMicros, true, nil
}

// ── fakeSpendCounterStore — injectable SpendCounter repo ───────────────────────

type fakeSpendCounterStore struct {
	mu              sync.Mutex
	orgTotal        int64
	userTotals      map[string]int64
	agentTotals     map[string]int64
	accumulateCalls []fakeAccumulateCall
	// accumulateErr simulates a DB failure on the durable spend write; the call
	// is still recorded so a test can assert it was attempted.
	accumulateErr error
}

type fakeAccumulateCall struct {
	tenant, userID, agentID string
	costMicros              int64
	tokensIn, tokensOut     int64
}

func newFakeSpendCounterStore() *fakeSpendCounterStore {
	return &fakeSpendCounterStore{userTotals: map[string]int64{}, agentTotals: map[string]int64{}}
}

func (f *fakeSpendCounterStore) Accumulate(_ context.Context, tenant, userID, agentID string, _ time.Time, costMicros, tokensIn, tokensOut int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accumulateCalls = append(f.accumulateCalls, fakeAccumulateCall{tenant, userID, agentID, costMicros, tokensIn, tokensOut})
	return f.accumulateErr
}

func (f *fakeSpendCounterStore) OrgTotal(_ context.Context, _ string, _ time.Time) (int64, error) {
	return f.orgTotal, nil
}

func (f *fakeSpendCounterStore) UserTotals(_ context.Context, _ string, _ time.Time) ([]db.SubjectSpend, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]db.SubjectSpend, 0, len(f.userTotals))
	for id, c := range f.userTotals {
		out = append(out, db.SubjectSpend{SubjectID: id, CostMicros: c})
	}
	return out, nil
}

func (f *fakeSpendCounterStore) AgentTotals(_ context.Context, _ string, _ time.Time) ([]db.SubjectSpend, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]db.SubjectSpend, 0, len(f.agentTotals))
	for id, c := range f.agentTotals {
		out = append(out, db.SubjectSpend{SubjectID: id, CostMicros: c})
	}
	return out, nil
}

func (f *fakeSpendCounterStore) SubjectTotal(_ context.Context, _ string, scope db.SpendCapScope, subjectID string, _ time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch scope {
	case db.SpendCapScopeOrg:
		return f.orgTotal, nil
	case db.SpendCapScopeUser:
		return f.userTotals[subjectID], nil
	case db.SpendCapScopeAgent:
		return f.agentTotals[subjectID], nil
	}
	return 0, nil
}

// waitFor (fire-and-forget goroutine poll) is defined in stream_test.go.

// ── enforcement: checkSpendCaps / CheckRateLimit spend gate ────────────────────

func newSouthSpendSvc(t *testing.T, caps *fakeSpendCapStore, counters *fakeSpendCounterStore) (*SandboxService, *capturingAuditSink) {
	t.Helper()
	sink := &capturingAuditSink{}
	deps := Deps{
		Logger:          zap.NewNop(),
		Audit:           sink,
		GatewaySpiffeID: testGateway,
		TenantID:        testTenant,
		SpendCaps:       caps,
		SpendCounters:   counters,
	}
	return NewSandboxService(deps), sink
}

func TestCheckRateLimit_OrgSpendOverCapDenied(t *testing.T) {
	counters := newFakeSpendCounterStore()
	counters.orgTotal = 1_000_000
	caps := newFakeSpendCapStore()
	_, _ = caps.Upsert(context.Background(), testTenant, db.SpendCap{Scope: db.SpendCapScopeOrg, CapMicros: 1_000_000})
	svc, _ := newSouthSpendSvc(t, caps, counters)

	resp, err := svc.CheckRateLimit(gatewayCtx(testGateway), &brokerv1.CheckRateLimitRequest{TenantId: testTenant, Provider: "openrouter"})
	if err != nil {
		t.Fatalf("CheckRateLimit: %v", err)
	}
	if resp.Allowed {
		t.Fatal("expected denial: org spend at cap")
	}
	if resp.LimitType != "spend_org" {
		t.Fatalf("limit_type = %q, want spend_org", resp.LimitType)
	}
}

func TestCheckRateLimit_UserSpendOverCapDenied(t *testing.T) {
	counters := newFakeSpendCounterStore()
	counters.userTotals["alice"] = 500_001
	caps := newFakeSpendCapStore()
	_, _ = caps.Upsert(context.Background(), testTenant, db.SpendCap{Scope: db.SpendCapScopeUser, SubjectID: "alice", CapMicros: 500_000})
	svc, _ := newSouthSpendSvc(t, caps, counters)

	resp, err := svc.CheckRateLimit(gatewayCtx(testGateway), &brokerv1.CheckRateLimitRequest{TenantId: testTenant, Provider: "openrouter", UserId: "alice"})
	if err != nil {
		t.Fatalf("CheckRateLimit: %v", err)
	}
	if resp.Allowed {
		t.Fatal("expected denial: user over cap")
	}
	if resp.LimitType != "spend_user" {
		t.Fatalf("limit_type = %q, want spend_user", resp.LimitType)
	}
}

func TestCheckRateLimit_AgentSpendOverCapDenied(t *testing.T) {
	counters := newFakeSpendCounterStore()
	counters.agentTotals["agent-1"] = 999_999
	caps := newFakeSpendCapStore()
	_, _ = caps.Upsert(context.Background(), testTenant, db.SpendCap{Scope: db.SpendCapScopeAgent, SubjectID: "agent-1", CapMicros: 900_000})
	svc, _ := newSouthSpendSvc(t, caps, counters)

	resp, err := svc.CheckRateLimit(gatewayCtx(testGateway), &brokerv1.CheckRateLimitRequest{TenantId: testTenant, Provider: "openrouter", AgentId: "agent-1"})
	if err != nil {
		t.Fatalf("CheckRateLimit: %v", err)
	}
	if resp.Allowed {
		t.Fatal("expected denial: agent over cap")
	}
	if resp.LimitType != "spend_agent" {
		t.Fatalf("limit_type = %q, want spend_agent", resp.LimitType)
	}
}

func TestCheckRateLimit_UnderCapAllowed(t *testing.T) {
	counters := newFakeSpendCounterStore()
	counters.orgTotal = 100
	counters.userTotals["alice"] = 100
	caps := newFakeSpendCapStore()
	_, _ = caps.Upsert(context.Background(), testTenant, db.SpendCap{Scope: db.SpendCapScopeOrg, CapMicros: 1_000_000})
	_, _ = caps.Upsert(context.Background(), testTenant, db.SpendCap{Scope: db.SpendCapScopeUser, SubjectID: "alice", CapMicros: 1_000_000})
	svc, _ := newSouthSpendSvc(t, caps, counters)

	resp, err := svc.CheckRateLimit(gatewayCtx(testGateway), &brokerv1.CheckRateLimitRequest{TenantId: testTenant, Provider: "openrouter", UserId: "alice"})
	if err != nil {
		t.Fatalf("CheckRateLimit: %v", err)
	}
	if !resp.Allowed {
		t.Fatalf("expected allow under cap, got denied: %s", resp.LimitType)
	}
}

// TestCheckRateLimit_UnsetCapNeverDenies proves a subject with spend but no
// configured cap is never denied (0 cap_micros must not be treated as
// deny-all — hasCap must gate the comparison, not a bare cap>0 check).
func TestCheckRateLimit_UnsetCapNeverDenies(t *testing.T) {
	counters := newFakeSpendCounterStore()
	counters.userTotals["alice"] = 999_999_999
	caps := newFakeSpendCapStore() // no caps configured at all
	svc, _ := newSouthSpendSvc(t, caps, counters)

	resp, err := svc.CheckRateLimit(gatewayCtx(testGateway), &brokerv1.CheckRateLimitRequest{TenantId: testTenant, Provider: "openrouter", UserId: "alice"})
	if err != nil {
		t.Fatalf("CheckRateLimit: %v", err)
	}
	if !resp.Allowed {
		t.Fatal("no cap configured must never deny")
	}
}

// TestCheckRateLimit_SpendDenialEmitsAudit proves a spend-cap denial fires the
// same rate-limit-enforced audit event RPM/TPM denials use, carrying the
// spend limit_type.
func TestCheckRateLimit_SpendDenialEmitsAudit(t *testing.T) {
	counters := newFakeSpendCounterStore()
	counters.orgTotal = 5_000_000
	caps := newFakeSpendCapStore()
	_, _ = caps.Upsert(context.Background(), testTenant, db.SpendCap{Scope: db.SpendCapScopeOrg, CapMicros: 1_000_000})
	svc, sink := newSouthSpendSvc(t, caps, counters)

	_, err := svc.CheckRateLimit(gatewayCtx(testGateway), &brokerv1.CheckRateLimitRequest{TenantId: testTenant, Provider: "openrouter"})
	if err != nil {
		t.Fatalf("CheckRateLimit: %v", err)
	}
	// emitCheckRateLimitAudit runs in a detached goroutine — poll for it.
	waitFor(t, func() bool { return sink.count() > 0 })
	ev := sink.last()
	if ev.EventType != "aikonos.broker.rate_limit.enforced" {
		t.Errorf("event_type = %q, want aikonos.broker.rate_limit.enforced", ev.EventType)
	}
}

// ── SpendCache invalidation ─────────────────────────────────────────────────

func TestSpendCache_MutationInvalidatesTenant(t *testing.T) {
	c := NewSpendRollupCache()
	key := spendCacheKey(testTenant, db.SpendCapScopeOrg, "")
	c.set(key, spendCacheEntry{capMicros: 100, hasCap: true, spendMicros: 50})
	if _, ok := c.get(key); !ok {
		t.Fatal("expected cache hit before invalidation")
	}
	c.invalidateTenant(testTenant)
	if _, ok := c.get(key); ok {
		t.Fatal("expected cache miss after invalidateTenant")
	}
}

// ── CRUD RPCs: admin gate ────────────────────────────────────────────────────

func testSpendAdminDeps(t *testing.T, fgaURL string, caps *fakeSpendCapStore, counters *fakeSpendCounterStore) Deps {
	t.Helper()
	sink := &capturingAuditSink{}
	cfg := policy.Config{OPAEndpoint: "http://unused", OpenFGAEndpoint: fgaURL}
	if fgaURL != "" {
		cfg.OpenFGAStoreID = "store-1"
	}
	eng, err := policy.NewEngine(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return Deps{
		Logger:        zap.NewNop(),
		Audit:         sink,
		Policy:        eng,
		TenantID:      testTenantUUID,
		SpendCaps:     caps,
		SpendCounters: counters,
	}
}

func TestListSpendCaps_NonAdminDenied(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testSpendAdminDeps(t, srv.URL, newFakeSpendCapStore(), newFakeSpendCounterStore()))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.ListSpendCaps(ctx, &brokerv1.ListSpendCapsRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin ListSpendCaps: want PermissionDenied, got %v", err)
	}
}

func TestSetSpendCap_NonAdminDenied(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testSpendAdminDeps(t, srv.URL, newFakeSpendCapStore(), newFakeSpendCounterStore()))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.SetSpendCap(ctx, &brokerv1.SetSpendCapRequest{Scope: "org", CapMicros: 1000})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin SetSpendCap: want PermissionDenied, got %v", err)
	}
}

func TestDeleteSpendCap_NonAdminDenied(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testSpendAdminDeps(t, srv.URL, newFakeSpendCapStore(), newFakeSpendCounterStore()))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.DeleteSpendCap(ctx, &brokerv1.DeleteSpendCapRequest{Id: uuid.New().String()})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin DeleteSpendCap: want PermissionDenied, got %v", err)
	}
}

func TestGetSpendSummary_NonAdminDenied(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testSpendAdminDeps(t, srv.URL, newFakeSpendCapStore(), newFakeSpendCounterStore()))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.GetSpendSummary(ctx, &brokerv1.GetSpendSummaryRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin GetSpendSummary: want PermissionDenied, got %v", err)
	}
}

// ── CRUD RPCs: validation + mutation audit ──────────────────────────────────

func TestSetSpendCap_ValidatesScope(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testSpendAdminDeps(t, srv.URL, newFakeSpendCapStore(), newFakeSpendCounterStore()))
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")

	cases := []struct {
		name    string
		req     *brokerv1.SetSpendCapRequest
		wantErr codes.Code
	}{
		{"org with subject_id rejected", &brokerv1.SetSpendCapRequest{Scope: "org", SubjectId: "x", CapMicros: 1}, codes.InvalidArgument},
		{"user without subject_id rejected", &brokerv1.SetSpendCapRequest{Scope: "user", CapMicros: 1}, codes.InvalidArgument},
		{"agent without subject_id rejected", &brokerv1.SetSpendCapRequest{Scope: "agent", CapMicros: 1}, codes.InvalidArgument},
		{"unknown scope rejected", &brokerv1.SetSpendCapRequest{Scope: "bogus", CapMicros: 1}, codes.InvalidArgument},
		{"zero cap rejected", &brokerv1.SetSpendCapRequest{Scope: "org", CapMicros: 0}, codes.InvalidArgument},
		{"negative cap rejected", &brokerv1.SetSpendCapRequest{Scope: "org", CapMicros: -1}, codes.InvalidArgument},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.SetSpendCap(ctx, c.req)
			if status.Code(err) != c.wantErr {
				t.Fatalf("%s: want %v, got %v", c.name, c.wantErr, err)
			}
		})
	}
}

func TestSetSpendCap_ValidRequestUpsertsAndAudits(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	caps := newFakeSpendCapStore()
	deps := testSpendAdminDeps(t, srv.URL, caps, newFakeSpendCounterStore())
	svc := NewBrokerService(deps)
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")

	resp, err := svc.SetSpendCap(ctx, &brokerv1.SetSpendCapRequest{Scope: "user", SubjectId: "alice", CapMicros: 5_000_000})
	if err != nil {
		t.Fatalf("SetSpendCap: %v", err)
	}
	if resp.Id == "" {
		t.Fatal("expected non-empty id")
	}

	capMicros, ok, err := caps.Get(context.Background(), testTenantUUID, db.SpendCapScopeUser, "alice")
	if err != nil || !ok || capMicros != 5_000_000 {
		t.Fatalf("cap not persisted: capMicros=%d ok=%v err=%v", capMicros, ok, err)
	}

	sink := deps.Audit.(*capturingAuditSink)
	if sink.count() == 0 {
		t.Fatal("expected an audit event on SetSpendCap")
	}
	if sink.last().EventType != "spend_cap.set" {
		t.Errorf("event_type = %q, want spend_cap.set", sink.last().EventType)
	}
}

func TestDeleteSpendCap_RemovesRowAndAudits(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	caps := newFakeSpendCapStore()
	saved, _ := caps.Upsert(context.Background(), testTenantUUID, db.SpendCap{Scope: db.SpendCapScopeOrg, CapMicros: 10})
	deps := testSpendAdminDeps(t, srv.URL, caps, newFakeSpendCounterStore())
	svc := NewBrokerService(deps)
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")

	_, err := svc.DeleteSpendCap(ctx, &brokerv1.DeleteSpendCapRequest{Id: saved.ID.String()})
	if err != nil {
		t.Fatalf("DeleteSpendCap: %v", err)
	}
	if _, ok, _ := caps.Get(context.Background(), testTenantUUID, db.SpendCapScopeOrg, ""); ok {
		t.Fatal("cap still present after delete")
	}
	sink := deps.Audit.(*capturingAuditSink)
	if sink.last().EventType != "spend_cap.delete" {
		t.Errorf("event_type = %q, want spend_cap.delete", sink.last().EventType)
	}
}

// ── GetSpendSummary ──────────────────────────────────────────────────────────

func TestGetSpendSummary_ReturnsOrgUserAgentRows(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	caps := newFakeSpendCapStore()
	_, _ = caps.Upsert(context.Background(), testTenantUUID, db.SpendCap{Scope: db.SpendCapScopeOrg, CapMicros: 1_000_000})
	_, _ = caps.Upsert(context.Background(), testTenantUUID, db.SpendCap{Scope: db.SpendCapScopeUser, SubjectID: "alice", CapMicros: 200_000})
	counters := newFakeSpendCounterStore()
	counters.orgTotal = 400_000
	counters.userTotals["alice"] = 150_000
	counters.agentTotals["agent-1"] = 50_000
	svc := NewBrokerService(testSpendAdminDeps(t, srv.URL, caps, counters))
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")

	resp, err := svc.GetSpendSummary(ctx, &brokerv1.GetSpendSummaryRequest{})
	if err != nil {
		t.Fatalf("GetSpendSummary: %v", err)
	}
	if resp.OrgSpendMicros != 400_000 || resp.OrgCapMicros != 1_000_000 {
		t.Errorf("org row wrong: spend=%d cap=%d", resp.OrgSpendMicros, resp.OrgCapMicros)
	}
	if len(resp.Users) != 1 || resp.Users[0].UserId != "alice" || resp.Users[0].SpendMicros != 150_000 || resp.Users[0].CapMicros != 200_000 {
		t.Errorf("user rows wrong: %+v", resp.Users)
	}
	if len(resp.Agents) != 1 || resp.Agents[0].AgentId != "agent-1" || resp.Agents[0].SpendMicros != 50_000 || resp.Agents[0].CapMicros != 0 {
		t.Errorf("agent rows wrong (agent-1 has no cap set, want cap_micros=0): %+v", resp.Agents)
	}
}

// TestGetSpendSummary_ZeroSpendCapStillAppears proves a configured user/agent
// cap with zero spend this period still surfaces a row — UserTotals/AgentTotals
// only return subjects with usage rows, so a cap-only subject must be unioned
// in separately or it silently vanishes from the dashboard.
func TestGetSpendSummary_ZeroSpendCapStillAppears(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	caps := newFakeSpendCapStore()
	_, _ = caps.Upsert(context.Background(), testTenantUUID, db.SpendCap{Scope: db.SpendCapScopeUser, SubjectID: "bob", CapMicros: 300_000})
	_, _ = caps.Upsert(context.Background(), testTenantUUID, db.SpendCap{Scope: db.SpendCapScopeAgent, SubjectID: "agent-2", CapMicros: 400_000})
	counters := newFakeSpendCounterStore() // no spend recorded for bob or agent-2
	svc := NewBrokerService(testSpendAdminDeps(t, srv.URL, caps, counters))
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")

	resp, err := svc.GetSpendSummary(ctx, &brokerv1.GetSpendSummaryRequest{})
	if err != nil {
		t.Fatalf("GetSpendSummary: %v", err)
	}
	if len(resp.Users) != 1 || resp.Users[0].UserId != "bob" || resp.Users[0].SpendMicros != 0 || resp.Users[0].CapMicros != 300_000 {
		t.Errorf("expected zero-spend capped user row for bob: %+v", resp.Users)
	}
	if len(resp.Agents) != 1 || resp.Agents[0].AgentId != "agent-2" || resp.Agents[0].SpendMicros != 0 || resp.Agents[0].CapMicros != 400_000 {
		t.Errorf("expected zero-spend capped agent row for agent-2: %+v", resp.Agents)
	}
}

