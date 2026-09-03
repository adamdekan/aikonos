package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ── DB-backed tests (require TEST_DATABASE_URL; skip when absent) ─────────────
//
// Mirrors the openTestPool skip pattern in workflows_test.go — no live
// Postgres is required for `go test` to pass; these tests only exercise the
// repo layer when TEST_DATABASE_URL is set (migration 032 applied).

// TestAgentBaselineRepo_UpsertWindows_Additive proves migration 032's
// ON CONFLICT clause adds to the existing counters rather than overwriting —
// two upserts of the same (tenant, agent, tool, window) key must sum.
func TestAgentBaselineRepo_UpsertWindows_Additive(t *testing.T) {
	pool := openTestPool(t)
	repo := NewAgentBaselineRepo(pool, zap.NewNop())
	ctx := context.Background()

	tenant := uuid.New().String()
	agent := uuid.New().String()
	windowStart := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)

	delta := WindowDelta{
		AgentID:     agent,
		ToolID:      "web.fetch",
		WindowStart: windowStart,
		Invocations: 3,
		CostUnits:   10,
	}

	if err := repo.UpsertWindows(ctx, tenant, []WindowDelta{delta}); err != nil {
		t.Fatalf("first UpsertWindows: %v", err)
	}
	if err := repo.UpsertWindows(ctx, tenant, []WindowDelta{delta}); err != nil {
		t.Fatalf("second UpsertWindows: %v", err)
	}

	rows, err := repo.ListRecentWindows(ctx, tenant, agent, 10)
	if err != nil {
		t.Fatalf("ListRecentWindows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row after two upserts of the same key, got %d", len(rows))
	}
	if rows[0].Invocations != 6 {
		t.Errorf("expected additive invocations = 6, got %d", rows[0].Invocations)
	}
	if rows[0].CostUnits != 20 {
		t.Errorf("expected additive cost_units = 20, got %d", rows[0].CostUnits)
	}
}

// TestAgentBaselineRepo_TenantIsolation proves a second tenant's windows are
// never returned by ListRecentWindows scoped to the first tenant, even for
// the same agent id — RLS + the WHERE tenant_id = $1 filter must isolate.
func TestAgentBaselineRepo_TenantIsolation(t *testing.T) {
	pool := openTestPool(t)
	repo := NewAgentBaselineRepo(pool, zap.NewNop())
	ctx := context.Background()

	tenantA := uuid.New().String()
	tenantB := uuid.New().String()
	agent := uuid.New().String()
	windowStart := time.Date(2026, 7, 4, 13, 0, 0, 0, time.UTC)

	deltaA := WindowDelta{AgentID: agent, ToolID: "doc.write", WindowStart: windowStart, Invocations: 5, CostUnits: 1}
	deltaB := WindowDelta{AgentID: agent, ToolID: "doc.write", WindowStart: windowStart, Invocations: 99, CostUnits: 99}

	if err := repo.UpsertWindows(ctx, tenantA, []WindowDelta{deltaA}); err != nil {
		t.Fatalf("upsert tenant A: %v", err)
	}
	if err := repo.UpsertWindows(ctx, tenantB, []WindowDelta{deltaB}); err != nil {
		t.Fatalf("upsert tenant B: %v", err)
	}

	rowsA, err := repo.ListRecentWindows(ctx, tenantA, agent, 10)
	if err != nil {
		t.Fatalf("ListRecentWindows tenant A: %v", err)
	}
	if len(rowsA) != 1 || rowsA[0].Invocations != 5 {
		t.Fatalf("tenant A isolation broken: got rows %+v", rowsA)
	}

	rowsB, err := repo.ListRecentWindows(ctx, tenantB, agent, 10)
	if err != nil {
		t.Fatalf("ListRecentWindows tenant B: %v", err)
	}
	if len(rowsB) != 1 || rowsB[0].Invocations != 99 {
		t.Fatalf("tenant B isolation broken: got rows %+v", rowsB)
	}
}

// TestAgentBaselineRepo_PruneWindowsBefore_Boundary proves the cutoff is
// strict: a window strictly before cutoff is deleted, a window exactly at or
// after cutoff is kept.
func TestAgentBaselineRepo_PruneWindowsBefore_Boundary(t *testing.T) {
	pool := openTestPool(t)
	repo := NewAgentBaselineRepo(pool, zap.NewNop())
	ctx := context.Background()

	tenant := uuid.New().String()
	agent := uuid.New().String()
	cutoff := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	older := WindowDelta{AgentID: agent, ToolID: "email.draft", WindowStart: cutoff.Add(-time.Hour), Invocations: 1, CostUnits: 1}
	atCutoff := WindowDelta{AgentID: agent, ToolID: "email.draft", WindowStart: cutoff, Invocations: 1, CostUnits: 1}
	newer := WindowDelta{AgentID: agent, ToolID: "email.draft", WindowStart: cutoff.Add(time.Hour), Invocations: 1, CostUnits: 1}

	if err := repo.UpsertWindows(ctx, tenant, []WindowDelta{older, atCutoff, newer}); err != nil {
		t.Fatalf("upsert windows: %v", err)
	}

	if _, err := repo.PruneWindowsBefore(ctx, cutoff); err != nil {
		t.Fatalf("PruneWindowsBefore: %v", err)
	}

	rows, err := repo.ListRecentWindows(ctx, tenant, agent, 10)
	if err != nil {
		t.Fatalf("ListRecentWindows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows to survive prune (at cutoff + newer), got %d: %+v", len(rows), rows)
	}
	for _, row := range rows {
		if row.WindowStart.Before(cutoff) {
			t.Errorf("row with window_start %v strictly before cutoff %v survived prune", row.WindowStart, cutoff)
		}
	}
}
