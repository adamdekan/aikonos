// broker/internal/db/agent_baselines.go
// Repository for automated agent behavioral baseline learning
//. Two tables:
// agent_behavior_windows (raw rolling per-window observations, flushed by the
// Recorder, read/pruned by the Learner) and agent_baselines (the materialized
// learned envelope, read by the Detector). Every tenant-scoped query sets
// app.current_tenant for RLS via withTenant — see the two exceptions noted on
// DistinctAgentsWithWindows and PruneWindowsBefore below.
package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// WindowDelta is one (agent, tool, window) observation to add to the running
// total — the unit the Recorder's in-memory buffer flushes in a batch.
// Invocations/CostUnits are deltas, not absolute totals: UpsertWindows adds
// them to whatever is already stored for the key.
type WindowDelta struct {
	AgentID     string
	ToolID      string
	WindowStart time.Time
	Invocations int64
	CostUnits   int64
}

// WindowRow is a persisted agent_behavior_windows row, as read by the Learner.
type WindowRow struct {
	TenantID    string
	AgentID     string
	ToolID      string
	WindowStart time.Time
	Invocations int64
	CostUnits   int64
}

// AgentRef identifies one (tenant, agent) pair with recent activity — the
// Learner's iteration unit.
type AgentRef struct {
	TenantID string
	AgentID  string
}

// Baseline is the materialized learned behavior envelope for one agent.
type Baseline struct {
	TenantID      string
	AgentID       string
	ToolSet       []string
	RpmP95        float64
	CostP95       float64
	SampleWindows int
	FirstSeen     time.Time
	ComputedAt    time.Time
}

// AgentBaselineRepo handles all persistence for agent behavioral baselines.
type AgentBaselineRepo struct {
	pool   *pgxpool.Pool
	logger *zap.Logger
}

func NewAgentBaselineRepo(pool *pgxpool.Pool, logger *zap.Logger) *AgentBaselineRepo {
	return &AgentBaselineRepo{pool: pool, logger: logger}
}

// UpsertWindows batch-upserts a tenant's window deltas, additive on conflict
// (ON CONFLICT ... DO UPDATE SET invocations = invocations + excluded, same
// for cost_units) so an at-least-once flush never double-counts across
// retries beyond the intended add. One call per tenant per flush — mirrors
// InsertPlanSteps's pgx.Batch idiom.
func (r *AgentBaselineRepo) UpsertWindows(ctx context.Context, tenant string, deltas []WindowDelta) error {
	if len(deltas) == 0 {
		return nil
	}

	return withConnErr(ctx, r.pool, tenant, func(conn *pgxpool.Conn) error {
		batch := &pgx.Batch{}
		for _, d := range deltas {
			batch.Queue(`
			INSERT INTO agent_behavior_windows (
				tenant_id, agent_id, tool_id, window_start, invocations, cost_units
			) VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (tenant_id, agent_id, tool_id, window_start) DO UPDATE SET
				invocations = agent_behavior_windows.invocations + excluded.invocations,
				cost_units = agent_behavior_windows.cost_units + excluded.cost_units`,
				tenant, d.AgentID, d.ToolID, d.WindowStart, d.Invocations, d.CostUnits,
			)
		}

		br := conn.SendBatch(ctx, batch)
		defer br.Close()
		for range deltas {
			if _, err := br.Exec(); err != nil {
				return fmt.Errorf("upsert window: %w", err)
			}
		}
		return nil
	})
}

// ListRecentWindows returns every row for the agent's last sinceWindows
// distinct window_start values, oldest first — the input the Learner
// aggregates into a Baseline.
func (r *AgentBaselineRepo) ListRecentWindows(ctx context.Context, tenant, agent string, sinceWindows int) ([]WindowRow, error) {
	return withConn(ctx, r.pool, tenant, func(conn *pgxpool.Conn) ([]WindowRow, error) {
		rows, err := conn.Query(ctx, `
		SELECT tenant_id, agent_id, tool_id, window_start, invocations, cost_units
		FROM agent_behavior_windows
		WHERE tenant_id = $1 AND agent_id = $2
		  AND window_start IN (
		      SELECT DISTINCT window_start FROM agent_behavior_windows
		      WHERE tenant_id = $1 AND agent_id = $2
		      ORDER BY window_start DESC LIMIT $3
		  )
		ORDER BY window_start ASC, tool_id ASC`,
			tenant, agent, sinceWindows,
		)
		if err != nil {
			return nil, fmt.Errorf("list recent windows: %w", err)
		}
		defer rows.Close()

		result, err := pgx.CollectRows(rows, pgx.RowToStructByName[WindowRow])
		if err != nil {
			return nil, fmt.Errorf("scan windows: %w", err)
		}
		return result, nil
	})
}

// DistinctAgentsWithWindows returns every (tenant, agent) pair with at least
// one window at or after since — the Learner's cross-tenant iteration seam.
//
// Deliberate cross-tenant exception to the withTenant-first rule: the Learner
// runs as a single background process that must recompute baselines for every
// tenant's agents, not one tenant at a time, so there is no single tenant to
// scope this query to. The broker connects as the non-superuser role
// `aikonos_app`, so RLS enforces here too and a direct SELECT would return zero
// rows; the cross-tenant read goes through the SECURITY DEFINER function
// baseline_distinct_agents (migration 033), which runs with owner rights and is
// the only sanctioned RLS bypass for this method.
func (r *AgentBaselineRepo) DistinctAgentsWithWindows(ctx context.Context, since time.Time) ([]AgentRef, error) {
	return withConnUnscoped(ctx, r.pool, func(conn *pgxpool.Conn) ([]AgentRef, error) {
		rows, err := conn.Query(ctx,
			`SELECT tenant_id, agent_id FROM baseline_distinct_agents($1)`,
			since,
		)
		if err != nil {
			return nil, fmt.Errorf("distinct agents with windows: %w", err)
		}
		defer rows.Close()

		result, err := pgx.CollectRows(rows, pgx.RowToStructByName[AgentRef])
		if err != nil {
			return nil, fmt.Errorf("scan agent refs: %w", err)
		}
		return result, nil
	})
}

// UpsertBaseline replaces the learned envelope for one agent.
func (r *AgentBaselineRepo) UpsertBaseline(ctx context.Context, tenant string, b Baseline) error {
	return withConnErr(ctx, r.pool, tenant, func(conn *pgxpool.Conn) error {
		toolSet := b.ToolSet
		if toolSet == nil {
			toolSet = []string{}
		}
		toolSetJSON, err := json.Marshal(toolSet)
		if err != nil {
			return fmt.Errorf("marshal tool_set: %w", err)
		}

		_, err = conn.Exec(ctx, `
		INSERT INTO agent_baselines (
			tenant_id, agent_id, tool_set, rpm_p95, cost_p95,
			sample_windows, first_seen, computed_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (tenant_id, agent_id) DO UPDATE SET
			tool_set = excluded.tool_set,
			rpm_p95 = excluded.rpm_p95,
			cost_p95 = excluded.cost_p95,
			sample_windows = excluded.sample_windows,
			first_seen = excluded.first_seen,
			computed_at = excluded.computed_at`,
			tenant, b.AgentID, toolSetJSON, b.RpmP95, b.CostP95,
			b.SampleWindows, b.FirstSeen, b.ComputedAt,
		)
		if err != nil {
			return fmt.Errorf("upsert baseline: %w", err)
		}
		return nil
	})
}

// GetBaseline fetches the learned envelope for one agent. Returns an error
// matching db.ErrNotFound (errors.Is) if none exists yet (learning phase).
func (r *AgentBaselineRepo) GetBaseline(ctx context.Context, tenant, agent string) (*Baseline, error) {
	return withConn(ctx, r.pool, tenant, func(conn *pgxpool.Conn) (*Baseline, error) {
		var (
			b           Baseline
			toolSetJSON []byte
		)
		err := conn.QueryRow(ctx, `
		SELECT tenant_id, agent_id, tool_set, rpm_p95, cost_p95,
		       sample_windows, first_seen, computed_at
		FROM agent_baselines
		WHERE tenant_id = $1 AND agent_id = $2`,
			tenant, agent,
		).Scan(&b.TenantID, &b.AgentID, &toolSetJSON, &b.RpmP95, &b.CostP95,
			&b.SampleWindows, &b.FirstSeen, &b.ComputedAt)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("get baseline: %w", err)
		}

		if err := json.Unmarshal(toolSetJSON, &b.ToolSet); err != nil {
			return nil, fmt.Errorf("unmarshal tool_set: %w", err)
		}
		return &b, nil
	})
}

// PruneWindowsBefore deletes every agent_behavior_windows row with
// window_start strictly before cutoff (rows at or after cutoff are kept) and
// returns the number of rows removed — the Learner's retention sweep.
//
// Same deliberate cross-tenant exception as DistinctAgentsWithWindows:
// retention is a single cross-tenant sweep, not a per-tenant operation, so
// there is no tenant to scope withTenant to. Under the non-superuser
// `aikonos_app` role RLS would filter the DELETE to zero rows, so the sweep goes
// through the SECURITY DEFINER function baseline_prune_windows (migration 033),
// which runs with owner rights — the only sanctioned RLS bypass for this method.
func (r *AgentBaselineRepo) PruneWindowsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	return withConnUnscoped(ctx, r.pool, func(conn *pgxpool.Conn) (int64, error) {
		var removed int64
		if err := conn.QueryRow(ctx,
			`SELECT baseline_prune_windows($1)`,
			cutoff,
		).Scan(&removed); err != nil {
			return 0, fmt.Errorf("prune windows: %w", err)
		}
		return removed, nil
	})
}
