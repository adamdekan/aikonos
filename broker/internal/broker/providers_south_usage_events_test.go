package broker

// Per-call LLM usage events:
// recordLlmUsage writes one analytics row per billable call carrying the same
// cost the durable counter accumulates, an insert failure never fails the RPC,
// and the FGA group snapshot is TTL-cached, prefix-stripped, and fail-open.

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ── fakeUsageEventStore — injectable UsageEvent repo ─────────────────────────

type fakeUsageEventStore struct {
	mu     sync.Mutex
	events []db.UsageEvent
	// insertErr simulates a DB failure; the call is still recorded so a test can
	// assert it was attempted.
	insertErr error
	// totalsErr simulates a DB failure on the SessionTotals read path.
	totalsErr error
}

func (f *fakeUsageEventStore) Insert(_ context.Context, ev db.UsageEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	return f.insertErr
}

// SessionTotals rolls the recorded events up the same way the real SQL does, so
// a handler test can assert against events it inserted through Insert rather
// than maintaining a second fixture that could disagree with it.
func (f *fakeUsageEventStore) SessionTotals(_ context.Context, tenantID, userID, sessionID string) ([]db.SessionUsageRow, error) {
	if f.totalsErr != nil {
		return nil, f.totalsErr
	}
	if sessionID == "" {
		return nil, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	byKey := map[string]*db.SessionUsageRow{}
	order := []string{}
	for _, ev := range f.events {
		if ev.TenantID != tenantID || ev.UserID != userID || ev.SessionID != sessionID {
			continue
		}
		key := ev.Provider + "\x00" + ev.Model
		row, ok := byKey[key]
		if !ok {
			byKey[key] = &db.SessionUsageRow{Provider: ev.Provider, Model: ev.Model}
			row = byKey[key]
			order = append(order, key)
		}
		row.TokensIn += ev.TokensIn
		row.TokensOut += ev.TokensOut
		row.CacheRead += ev.CacheRead
		row.CacheWrite += ev.CacheWrite
		row.CostMicros += ev.CostMicros
		row.Calls++
	}

	out := make([]db.SessionUsageRow, 0, len(order))
	for _, key := range order {
		out = append(out, *byKey[key])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CostMicros > out[j].CostMicros })
	return out, nil
}

func (f *fakeUsageEventStore) all() []db.UsageEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]db.UsageEvent(nil), f.events...)
}

// newUsageEventDeps builds Deps wired for the event path only — no Providers, so
// cost comes straight off req.Cost.
func newUsageEventDeps(events *fakeUsageEventStore, counters *fakeSpendCounterStore) Deps {
	return Deps{Logger: zap.NewNop(), SpendCounters: counters, UsageEvents: events}
}

// ── event insert ─────────────────────────────────────────────────────────────

// The event and the counter must agree on cost: both read the one
// costMicrosFor result recordLlmUsage computes.
func TestRecordLlmUsage_WritesEventWithSameCostAsCounter(t *testing.T) {
	events := &fakeUsageEventStore{}
	counters := newFakeSpendCounterStore()
	deps := newUsageEventDeps(events, counters)

	deps.recordLlmUsage(context.Background(), &brokerv1.EmitLlmUsageRequest{
		TenantId: testTenant, UserId: "alice", AgentId: "agent-1",
		RunId: "run-7", SessionId: "sess-9", Source: "chat",
		Provider: "openrouter", Model: "some-future-model",
		TokensIn: 10, TokensOut: 20, CacheRead: 5, CacheWrite: 3, Cost: 0.5,
	})

	got := events.all()
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	want := db.UsageEvent{
		TenantID: testTenant, UserID: "alice", AgentID: "agent-1",
		RunID: "run-7", SessionID: "sess-9", Source: "chat",
		Provider: "openrouter", Model: "some-future-model",
		TokensIn: 10, TokensOut: 20, CacheRead: 5, CacheWrite: 3,
		CostMicros: 500_000, // 0.5 USD
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("event = %+v, want %+v", got[0], want)
	}

	counters.mu.Lock()
	defer counters.mu.Unlock()
	if len(counters.accumulateCalls) != 1 {
		t.Fatalf("want 1 accumulate call, got %d", len(counters.accumulateCalls))
	}
	if counters.accumulateCalls[0].costMicros != got[0].CostMicros {
		t.Errorf("counter cost %d != event cost %d — the two must never disagree",
			counters.accumulateCalls[0].costMicros, got[0].CostMicros)
	}
}

// Most providers return no cost, so the server prices the call itself. The
// metrics counter must be handed that resolved cost, not the absent req.Cost —
// otherwise llm_cost_total is never incremented into existence while the DB row
// carries real spend.
func TestRecordLlmUsage_MetricsCostMatchesResolvedCostWhenRequestHasNone(t *testing.T) {
	store := newFakeLlmProviderStore()
	store.rows[testTenant+"/openrouter"] = fakeLlmProviderRow{
		id: "openrouter", name: "OR", endpoint: "https://x.test", api: "openai-completions",
		models: []db.Model{{ID: "gpt-4o", Mode: "chat", Pricing: &db.ModelPricing{
			Unit: db.PricingUnitPerMTok, InMicros: 3_000_000, OutMicros: 15_000_000,
		}}},
	}
	rec := &fakeUsageRecorder{}
	events := &fakeUsageEventStore{}
	deps := Deps{Logger: zap.NewNop(), Providers: store, UsageMetrics: rec, UsageEvents: events}

	deps.recordLlmUsage(context.Background(), &brokerv1.EmitLlmUsageRequest{
		TenantId: testTenant, UserId: "alice", Provider: "openrouter", Model: "gpt-4o",
		TokensIn: 1_000_000, TokensOut: 500_000, // 3.00 + 7.50 USD
	})

	got, ok := rec.last()
	if !ok {
		t.Fatal("expected RecordUsage to be called")
	}
	if got.Cost != 10.5 {
		t.Errorf("metrics cost = %v USD, want 10.5 — the server-resolved cost, not req.Cost", got.Cost)
	}
	ev := events.all()
	if len(ev) != 1 {
		t.Fatalf("want 1 event, got %d", len(ev))
	}
	if int64(got.Cost*microUSDPerUSD) != ev[0].CostMicros {
		t.Errorf("metrics cost %v USD != event cost %d micros — the two must never disagree",
			got.Cost, ev[0].CostMicros)
	}
}

// EmitLlmUsage is fire-and-forget: an analytics insert failure is logged and
// swallowed, never returned, and never blocks the durable spend write.
func TestEmitLlmUsage_EventInsertErrorDoesNotFailRPC(t *testing.T) {
	events := &fakeUsageEventStore{insertErr: errors.New("db down")}
	counters := newFakeSpendCounterStore()
	deps := newUsageEventDeps(events, counters)
	deps.GatewaySpiffeID = testGateway
	deps.TenantID = testTenant
	svc := NewSandboxService(deps)

	resp, err := svc.EmitLlmUsage(gatewayCtx(testGateway), &brokerv1.EmitLlmUsageRequest{
		TenantId: testTenant, UserId: "alice", Provider: "openrouter", Model: "m",
		TokensIn: 10, TokensOut: 20, Cost: 0.002,
	})
	if err != nil {
		t.Fatalf("an event-insert failure must not fail the RPC, got %v", err)
	}
	if resp == nil {
		t.Fatal("want a response even when the event insert failed")
	}
	if len(events.all()) != 1 {
		t.Error("the insert must still have been attempted")
	}
	counters.mu.Lock()
	defer counters.mu.Unlock()
	if len(counters.accumulateCalls) != 1 {
		t.Error("the durable counter write must be unaffected by the event failure")
	}
}

// A nil store leaves the rest of the accounting path untouched.
func TestRecordLlmUsage_NilEventStoreIsNoOp(t *testing.T) {
	counters := newFakeSpendCounterStore()
	deps := Deps{Logger: zap.NewNop(), SpendCounters: counters}

	deps.recordLlmUsage(context.Background(), &brokerv1.EmitLlmUsageRequest{
		TenantId: testTenant, Provider: "openrouter", Cost: 0.001,
	})

	counters.mu.Lock()
	defer counters.mu.Unlock()
	if len(counters.accumulateCalls) != 1 {
		t.Fatalf("want the counter still written, got %d calls", len(counters.accumulateCalls))
	}
}

// ── group snapshot ───────────────────────────────────────────────────────────

// newGroupDeps builds Deps with a real policy engine pointed at the fake FGA.
func newGroupDeps(t *testing.T, fgaURL string) Deps {
	t.Helper()
	eng, err := policy.NewEngine(context.Background(), policy.Config{
		OPAEndpoint:     "http://unused",
		OpenFGAEndpoint: fgaURL,
		OpenFGAStoreID:  "store-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return Deps{Logger: zap.NewNop(), Policy: eng}
}

// Group ids reach us with or without the "group:" type prefix depending on the
// caller; bare, sorted ids are what gets stored.
func TestUsageUserGroups_StripsPrefixAndSorts(t *testing.T) {
	f := &fakeFGA{listObjectsResult: []string{"group:sales", "engineering", "group:"}}
	srv := f.server(t)
	defer srv.Close()
	deps := newGroupDeps(t, srv.URL)

	got := deps.usageUserGroups(context.Background(), testTenant, "alice-"+uuid.NewString())
	want := []string{"engineering", "sales"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("groups = %v, want %v", got, want)
	}
}

// A second call inside the TTL must not hit OpenFGA — the emit path runs on
// every model turn.
func TestUsageUserGroups_CachesWithinTTL(t *testing.T) {
	f := &fakeFGA{listObjectsResult: []string{"group:eng"}}
	srv := f.server(t)
	defer srv.Close()
	deps := newGroupDeps(t, srv.URL)
	user := "alice-" + uuid.NewString()

	first := deps.usageUserGroups(context.Background(), testTenant, user)
	second := deps.usageUserGroups(context.Background(), testTenant, user)

	if !reflect.DeepEqual(first, second) {
		t.Errorf("cached result diverged: %v vs %v", first, second)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listObjectsCalls != 1 {
		t.Errorf("list-objects called %d times, want 1 (second call must be cached)", f.listObjectsCalls)
	}
}

// An expired entry is refetched, not served stale forever.
func TestUsageUserGroups_RefetchesAfterTTL(t *testing.T) {
	f := &fakeFGA{listObjectsResult: []string{"group:fresh"}}
	srv := f.server(t)
	defer srv.Close()
	deps := newGroupDeps(t, srv.URL)
	user := "alice-" + uuid.NewString()

	// Seed an already-expired entry rather than sleeping out the 5-minute TTL.
	usageGroupCache.Store(testTenant+"|"+user, usageGroupEntry{
		groups:    []string{"stale"},
		expiresAt: time.Now().Add(-time.Minute),
	})

	got := deps.usageUserGroups(context.Background(), testTenant, user)
	if !reflect.DeepEqual(got, []string{"fresh"}) {
		t.Errorf("groups = %v, want [fresh] — an expired entry must be refetched", got)
	}
}

// The snapshot is a reporting dimension, never a gate: an FGA outage costs the
// group column, not the event.
func TestUsageUserGroups_FGAErrorFailsOpenToEmpty(t *testing.T) {
	f := &fakeFGA{listObjectsErr: true}
	srv := f.server(t)
	defer srv.Close()
	deps := newGroupDeps(t, srv.URL)

	if got := deps.usageUserGroups(context.Background(), testTenant, "alice-"+uuid.NewString()); len(got) != 0 {
		t.Errorf("groups = %v, want empty on FGA error", got)
	}
}

// No policy engine (FGA disabled) and no user (scheduled/agent-only call) both
// resolve to no groups without a lookup.
func TestUsageUserGroups_NilPolicyOrEmptyUser(t *testing.T) {
	if got := (Deps{Logger: zap.NewNop()}).usageUserGroups(context.Background(), testTenant, "alice"); got != nil {
		t.Errorf("nil Policy: groups = %v, want nil", got)
	}
	f := &fakeFGA{listObjectsResult: []string{"group:eng"}}
	srv := f.server(t)
	defer srv.Close()
	deps := newGroupDeps(t, srv.URL)
	if got := deps.usageUserGroups(context.Background(), testTenant, ""); got != nil {
		t.Errorf("empty user: groups = %v, want nil", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listObjectsCalls != 0 {
		t.Errorf("empty user must not query FGA, got %d calls", f.listObjectsCalls)
	}
}

// The recorded event carries the resolved groups.
func TestRecordLlmUsage_EventCarriesGroupSnapshot(t *testing.T) {
	f := &fakeFGA{listObjectsResult: []string{"group:eng", "group:sales"}}
	srv := f.server(t)
	defer srv.Close()
	events := &fakeUsageEventStore{}
	deps := newGroupDeps(t, srv.URL)
	deps.UsageEvents = events

	user := "alice-" + uuid.NewString()
	deps.recordLlmUsage(context.Background(), &brokerv1.EmitLlmUsageRequest{
		TenantId: testTenant, UserId: user, Provider: "openrouter", Cost: 0.001,
	})

	got := events.all()
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	if !reflect.DeepEqual(got[0].UserGroups, []string{"eng", "sales"}) {
		t.Errorf("user_groups = %v, want [eng sales]", got[0].UserGroups)
	}
}
