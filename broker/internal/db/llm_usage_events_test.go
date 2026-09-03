package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

func TestNewUsageEventRepo(t *testing.T) {
	if NewUsageEventRepo(nil, nil) == nil {
		t.Fatal("NewUsageEventRepo returned nil")
	}
}

// ── DB-backed tests (require TEST_DATABASE_URL; skip when absent) ─────────────
//
// Same openTestPool skip pattern as workflows_test.go/spend_caps_test.go — no
// live Postgres is needed for `go test` to pass; these exercise the repo only
// when TEST_DATABASE_URL is set (migration 045 applied).

// readEvents returns the tenant's events oldest-first, with the DB-assigned ts.
func readEvents(t *testing.T, pool *pgxpool.Pool, tenant string) ([]UsageEvent, []time.Time) {
	t.Helper()
	type row struct {
		ev UsageEvent
		ts time.Time
	}
	rowsOut, err := withConn(context.Background(), pool, tenant, func(conn *pgxpool.Conn) ([]row, error) {
		rs, err := conn.Query(context.Background(), `
		SELECT ts, tenant_id, user_id, agent_id, run_id, session_id, source, provider, model,
		       tokens_in, tokens_out, cache_read, cache_write, cost_micros, user_groups,
		       quantity, unit
		  FROM llm_usage_events WHERE tenant_id = $1 ORDER BY id`, tenant)
		if err != nil {
			return nil, err
		}
		defer rs.Close()
		var out []row
		for rs.Next() {
			var r row
			if err := rs.Scan(&r.ts, &r.ev.TenantID, &r.ev.UserID, &r.ev.AgentID, &r.ev.RunID,
				&r.ev.SessionID, &r.ev.Source, &r.ev.Provider, &r.ev.Model, &r.ev.TokensIn,
				&r.ev.TokensOut, &r.ev.CacheRead, &r.ev.CacheWrite, &r.ev.CostMicros,
				&r.ev.UserGroups, &r.ev.Quantity, &r.ev.Unit); err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		return out, rs.Err()
	})
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	evs := make([]UsageEvent, 0, len(rowsOut))
	tss := make([]time.Time, 0, len(rowsOut))
	for _, r := range rowsOut {
		evs = append(evs, r.ev)
		tss = append(tss, r.ts)
	}
	return evs, tss
}

// Insert round-trips every column (including the TEXT[] group snapshot and a nil
// one), and PruneBefore deletes strictly older rows only.
func TestUsageEvent_InsertAndPruneRoundTrip(t *testing.T) {
	pool := openTestPool(t)
	repo := NewUsageEventRepo(pool, zap.NewNop())
	ctx := context.Background()
	tenant := uuid.New().String()

	want := UsageEvent{
		TenantID: tenant, UserID: "alice", AgentID: "agent-1",
		RunID: "run-7", SessionID: "sess-9", Source: "chat",
		Provider: "openrouter", Model: "some-future-model",
		TokensIn: 10, TokensOut: 20, CacheRead: 5, CacheWrite: 3,
		CostMicros: 1234, UserGroups: []string{"eng", "sales"},
		Quantity: 12, Unit: "per_page",
	}
	if err := repo.Insert(ctx, want); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// A nil UserGroups must not violate the NOT NULL column, and an
	// unpopulated Quantity/Unit (the existing token-only sender shape) must
	// round-trip as 0/"" rather than failing the insert.
	if err := repo.Insert(ctx, UsageEvent{TenantID: tenant, Provider: "p"}); err != nil {
		t.Fatalf("Insert with nil UserGroups: %v", err)
	}

	got, tss := readEvents(t, pool, tenant)
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d", len(got))
	}
	if got[0].UserID != want.UserID || got[0].AgentID != want.AgentID || got[0].RunID != want.RunID ||
		got[0].SessionID != want.SessionID || got[0].Source != want.Source ||
		got[0].Provider != want.Provider || got[0].Model != want.Model ||
		got[0].TokensIn != want.TokensIn || got[0].TokensOut != want.TokensOut ||
		got[0].CacheRead != want.CacheRead || got[0].CacheWrite != want.CacheWrite ||
		got[0].CostMicros != want.CostMicros ||
		got[0].Quantity != want.Quantity || got[0].Unit != want.Unit {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got[0], want)
	}
	if len(got[0].UserGroups) != 2 || got[0].UserGroups[0] != "eng" || got[0].UserGroups[1] != "sales" {
		t.Errorf("user_groups = %v, want [eng sales]", got[0].UserGroups)
	}
	if len(got[1].UserGroups) != 0 {
		t.Errorf("nil UserGroups stored as %v, want empty array", got[1].UserGroups)
	}
	if got[1].Quantity != 0 || got[1].Unit != "" {
		t.Errorf("token-only sender quantity/unit = %v/%q, want 0/\"\"", got[1].Quantity, got[1].Unit)
	}
	// ts defaults to NOW(); anything far off means the column default is wrong.
	if tss[0].IsZero() || time.Since(tss[0]) > time.Hour {
		t.Errorf("ts = %v, want ~now", tss[0])
	}

	// Both rows are fresh, so a past cutoff must leave them alone.
	removed, err := repo.PruneBefore(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("PruneBefore: %v", err)
	}
	if removed != 0 {
		t.Errorf("prune removed %d rows, want 0 — rows newer than the cutoff must survive", removed)
	}
	if evs, _ := readEvents(t, pool, tenant); len(evs) != 2 {
		t.Fatalf("%d rows survived the no-op sweep, want 2", len(evs))
	}

	// A future cutoff removes them (>= 2: the sweep is cross-tenant by design).
	removed, err = repo.PruneBefore(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("PruneBefore (future cutoff): %v", err)
	}
	if removed < 2 {
		t.Errorf("prune removed %d rows, want >= 2", removed)
	}
	if evs, _ := readEvents(t, pool, tenant); len(evs) != 0 {
		t.Errorf("%d rows survived the sweep, want 0", len(evs))
	}
}
