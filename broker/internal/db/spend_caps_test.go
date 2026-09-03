package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ── Pure-logic tests (no Postgres required) ───────────────────────────────────

func TestSpendPeriodStart(t *testing.T) {
	cases := []struct {
		in   time.Time
		want time.Time
	}{
		{time.Date(2026, 7, 24, 15, 30, 0, 0, time.UTC), time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC), time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)},
		// A non-UTC input must still normalize to the UTC month start.
		{time.Date(2026, 7, 1, 1, 0, 0, 0, time.FixedZone("est", -5*3600)), time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		got := SpendPeriodStart(c.in)
		if !got.Equal(c.want) {
			t.Errorf("SpendPeriodStart(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestNewSpendCapRepo(t *testing.T) {
	if NewSpendCapRepo(nil, nil) == nil {
		t.Fatal("NewSpendCapRepo returned nil")
	}
}

func TestNewSpendCounterRepo(t *testing.T) {
	if NewSpendCounterRepo(nil, nil) == nil {
		t.Fatal("NewSpendCounterRepo returned nil")
	}
}

// ── DB-backed tests (require TEST_DATABASE_URL; skip when absent) ─────────────
//
// Mirrors the openTestPool skip pattern in workflows_test.go — no live
// Postgres is required for `go test` to pass; these tests only exercise the
// repo layer when TEST_DATABASE_URL is set (migration 042 applied).

// TestSpendCap_CRUDRoundTrip proves create/list/upsert-overwrite/delete all
// round-trip through the one-cap-per-(tenant,scope,subject) constraint.
func TestSpendCap_CRUDRoundTrip(t *testing.T) {
	pool := openTestPool(t)
	repo := NewSpendCapRepo(pool, zap.NewNop())
	ctx := context.Background()
	tenant := uuid.New().String()

	created, err := repo.Upsert(ctx, tenant, SpendCap{
		Scope:     SpendCapScopeOrg,
		SubjectID: "",
		CapMicros: 1_000_000,
		CreatedBy: "admin@example.com",
	})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if created.CapMicros != 1_000_000 {
		t.Fatalf("created CapMicros = %d, want 1000000", created.CapMicros)
	}

	list, err := repo.List(ctx, tenant)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("List after create: got %+v", list)
	}

	// Upsert-overwrite: same scope key, new cap value — must update in place,
	// not create a second row (the whole point of ON CONFLICT).
	updated, err := repo.Upsert(ctx, tenant, SpendCap{
		Scope:     SpendCapScopeOrg,
		SubjectID: "",
		CapMicros: 2_000_000,
		CreatedBy: "admin@example.com",
	})
	if err != nil {
		t.Fatalf("Upsert overwrite: %v", err)
	}
	if updated.ID != created.ID {
		t.Fatalf("overwrite created a new row: original id %s, new id %s", created.ID, updated.ID)
	}
	if updated.CapMicros != 2_000_000 {
		t.Fatalf("updated CapMicros = %d, want 2000000", updated.CapMicros)
	}

	list, err = repo.List(ctx, tenant)
	if err != nil {
		t.Fatalf("List after overwrite: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected exactly 1 row after upsert-overwrite, got %d", len(list))
	}

	if err := repo.Delete(ctx, tenant, created.ID.String()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	list, err = repo.List(ctx, tenant)
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected 0 rows after delete, got %d", len(list))
	}

	// Delete of a non-existent id is idempotent, not an error.
	if err := repo.Delete(ctx, tenant, uuid.New().String()); err != nil {
		t.Fatalf("Delete of missing id: %v", err)
	}
}

// TestSpendCap_ScopesCoexist proves org/user/agent caps for distinct subjects
// don't collide under the (tenant, scope, subject_id) unique index.
func TestSpendCap_ScopesCoexist(t *testing.T) {
	pool := openTestPool(t)
	repo := NewSpendCapRepo(pool, zap.NewNop())
	ctx := context.Background()
	tenant := uuid.New().String()

	if _, err := repo.Upsert(ctx, tenant, SpendCap{Scope: SpendCapScopeOrg, SubjectID: "", CapMicros: 1}); err != nil {
		t.Fatalf("upsert org: %v", err)
	}
	if _, err := repo.Upsert(ctx, tenant, SpendCap{Scope: SpendCapScopeUser, SubjectID: "user-1", CapMicros: 2}); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	if _, err := repo.Upsert(ctx, tenant, SpendCap{Scope: SpendCapScopeAgent, SubjectID: "agent-1", CapMicros: 3}); err != nil {
		t.Fatalf("upsert agent: %v", err)
	}

	list, err := repo.List(ctx, tenant)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("expected 3 distinct caps, got %d: %+v", len(list), list)
	}
}

// TestSpendCap_TenantIsolation proves a second tenant's caps never appear in
// the first tenant's List — RLS enforces via withTenant.
func TestSpendCap_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	repo := NewSpendCapRepo(pool, zap.NewNop())
	ctx := context.Background()
	tenantA := uuid.New().String()
	tenantB := uuid.New().String()

	if _, err := repo.Upsert(ctx, tenantA, SpendCap{Scope: SpendCapScopeOrg, CapMicros: 111}); err != nil {
		t.Fatalf("upsert tenant A: %v", err)
	}
	if _, err := repo.Upsert(ctx, tenantB, SpendCap{Scope: SpendCapScopeOrg, CapMicros: 222}); err != nil {
		t.Fatalf("upsert tenant B: %v", err)
	}

	listA, err := repo.List(ctx, tenantA)
	if err != nil {
		t.Fatalf("List tenant A: %v", err)
	}
	if len(listA) != 1 || listA[0].CapMicros != 111 {
		t.Fatalf("tenant A isolation broken: got %+v", listA)
	}

	listB, err := repo.List(ctx, tenantB)
	if err != nil {
		t.Fatalf("List tenant B: %v", err)
	}
	if len(listB) != 1 || listB[0].CapMicros != 222 {
		t.Fatalf("tenant B isolation broken: got %+v", listB)
	}
}

// TestSpendCounter_AccumulateTwiceSums proves the ON CONFLICT clause adds to
// the existing counters rather than overwriting.
func TestSpendCounter_AccumulateTwiceSums(t *testing.T) {
	pool := openTestPool(t)
	repo := NewSpendCounterRepo(pool, zap.NewNop())
	ctx := context.Background()
	tenant := uuid.New().String()
	period := SpendPeriodStart(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))

	if err := repo.Accumulate(ctx, tenant, "user-1", "agent-1", period, 100, 10, 20); err != nil {
		t.Fatalf("first Accumulate: %v", err)
	}
	if err := repo.Accumulate(ctx, tenant, "user-1", "agent-1", period, 50, 5, 8); err != nil {
		t.Fatalf("second Accumulate: %v", err)
	}

	total, err := repo.SubjectTotal(ctx, tenant, SpendCapScopeUser, "user-1", period)
	if err != nil {
		t.Fatalf("SubjectTotal: %v", err)
	}
	if total != 150 {
		t.Fatalf("expected summed cost 150, got %d", total)
	}
}

// TestSpendCounter_Rollups proves org/user/agent rollups correctly cross
// multiple users and agents in the same tenant+period.
func TestSpendCounter_Rollups(t *testing.T) {
	pool := openTestPool(t)
	repo := NewSpendCounterRepo(pool, zap.NewNop())
	ctx := context.Background()
	tenant := uuid.New().String()
	period := SpendPeriodStart(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))

	// user-1 spends via agent-a and agent-b; user-2 spends only via agent-a.
	if err := repo.Accumulate(ctx, tenant, "user-1", "agent-a", period, 100, 1, 1); err != nil {
		t.Fatalf("accumulate 1: %v", err)
	}
	if err := repo.Accumulate(ctx, tenant, "user-1", "agent-b", period, 200, 1, 1); err != nil {
		t.Fatalf("accumulate 2: %v", err)
	}
	if err := repo.Accumulate(ctx, tenant, "user-2", "agent-a", period, 300, 1, 1); err != nil {
		t.Fatalf("accumulate 3: %v", err)
	}
	// A different period must not leak into this period's rollups.
	otherPeriod := SpendPeriodStart(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if err := repo.Accumulate(ctx, tenant, "user-1", "agent-a", otherPeriod, 9999, 1, 1); err != nil {
		t.Fatalf("accumulate other period: %v", err)
	}

	orgTotal, err := repo.OrgTotal(ctx, tenant, period)
	if err != nil {
		t.Fatalf("OrgTotal: %v", err)
	}
	if orgTotal != 600 {
		t.Fatalf("OrgTotal = %d, want 600", orgTotal)
	}

	userTotals, err := repo.UserTotals(ctx, tenant, period)
	if err != nil {
		t.Fatalf("UserTotals: %v", err)
	}
	byUser := map[string]int64{}
	for _, s := range userTotals {
		byUser[s.SubjectID] = s.CostMicros
	}
	if byUser["user-1"] != 300 {
		t.Errorf("user-1 total = %d, want 300 (100+200 across agents)", byUser["user-1"])
	}
	if byUser["user-2"] != 300 {
		t.Errorf("user-2 total = %d, want 300", byUser["user-2"])
	}

	agentTotals, err := repo.AgentTotals(ctx, tenant, period)
	if err != nil {
		t.Fatalf("AgentTotals: %v", err)
	}
	byAgent := map[string]int64{}
	for _, s := range agentTotals {
		byAgent[s.SubjectID] = s.CostMicros
	}
	if byAgent["agent-a"] != 400 {
		t.Errorf("agent-a total = %d, want 400 (100 from user-1 + 300 from user-2)", byAgent["agent-a"])
	}
	if byAgent["agent-b"] != 200 {
		t.Errorf("agent-b total = %d, want 200", byAgent["agent-b"])
	}

	orgSubjectTotal, err := repo.SubjectTotal(ctx, tenant, SpendCapScopeOrg, "", period)
	if err != nil {
		t.Fatalf("SubjectTotal(org): %v", err)
	}
	if orgSubjectTotal != 600 {
		t.Errorf("SubjectTotal(org) = %d, want 600", orgSubjectTotal)
	}

	userSubjectTotal, err := repo.SubjectTotal(ctx, tenant, SpendCapScopeUser, "user-2", period)
	if err != nil {
		t.Fatalf("SubjectTotal(user-2): %v", err)
	}
	if userSubjectTotal != 300 {
		t.Errorf("SubjectTotal(user-2) = %d, want 300", userSubjectTotal)
	}

	agentSubjectTotal, err := repo.SubjectTotal(ctx, tenant, SpendCapScopeAgent, "agent-b", period)
	if err != nil {
		t.Fatalf("SubjectTotal(agent-b): %v", err)
	}
	if agentSubjectTotal != 200 {
		t.Errorf("SubjectTotal(agent-b) = %d, want 200", agentSubjectTotal)
	}
}

// TestSpendCounter_TenantIsolation proves a second tenant's counters never
// leak into the first tenant's rollups.
func TestSpendCounter_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	repo := NewSpendCounterRepo(pool, zap.NewNop())
	ctx := context.Background()
	tenantA := uuid.New().String()
	tenantB := uuid.New().String()
	period := SpendPeriodStart(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))

	if err := repo.Accumulate(ctx, tenantA, "user-1", "agent-1", period, 111, 1, 1); err != nil {
		t.Fatalf("accumulate tenant A: %v", err)
	}
	if err := repo.Accumulate(ctx, tenantB, "user-1", "agent-1", period, 222, 1, 1); err != nil {
		t.Fatalf("accumulate tenant B: %v", err)
	}

	totalA, err := repo.OrgTotal(ctx, tenantA, period)
	if err != nil {
		t.Fatalf("OrgTotal tenant A: %v", err)
	}
	if totalA != 111 {
		t.Fatalf("tenant A isolation broken: got %d, want 111", totalA)
	}

	totalB, err := repo.OrgTotal(ctx, tenantB, period)
	if err != nil {
		t.Fatalf("OrgTotal tenant B: %v", err)
	}
	if totalB != 222 {
		t.Fatalf("tenant B isolation broken: got %d, want 222", totalB)
	}
}
