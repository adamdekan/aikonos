// broker/internal/db/llm_usage_events.go
// Repository for per-call LLM usage events (llm_usage_events, migration 045 —
// ). The analytics twin of
// llm_spend_counters (spend_caps.go), which remains the authoritative cap
// meter. Insert is tenant-scoped via withConn; PruneBefore is the one
// deliberate cross-tenant exception, noted on the method.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// UsageEvent is one billable LLM call, mirroring the llm_usage_events columns.
// ts and id are assigned by the DB. UserGroups is an FGA membership snapshot at
// emit time — a reporting dimension only, never an authorization input.
// Quantity/Unit (migration 047) carry the billable amount for a non-per_mtok
// pricing unit (e.g. Quantity=12, Unit="per_page"); both stay 0/"" for
// token-billed calls.
type UsageEvent struct {
	TenantID   string
	UserID     string
	AgentID    string
	RunID      string
	SessionID  string
	Source     string
	Provider   string
	Model      string
	TokensIn   int64
	TokensOut  int64
	CacheRead  int64
	CacheWrite int64
	CostMicros int64
	Quantity   float64
	Unit       string
	UserGroups []string
}

// UsageEventRepo persists llm_usage_events rows.
type UsageEventRepo struct {
	pool   *pgxpool.Pool
	logger *zap.Logger
}

func NewUsageEventRepo(pool *pgxpool.Pool, logger *zap.Logger) *UsageEventRepo {
	return &UsageEventRepo{pool: pool, logger: logger}
}

// Insert writes one usage event. ts defaults to NOW() — the emit follows the
// call by milliseconds, so insert time is the call time.
func (r *UsageEventRepo) Insert(ctx context.Context, ev UsageEvent) error {
	return withConnErr(ctx, r.pool, ev.TenantID, func(conn *pgxpool.Conn) error {
		groups := ev.UserGroups
		if groups == nil {
			// A nil slice would map to SQL NULL, violating the NOT NULL column.
			groups = []string{}
		}
		_, err := conn.Exec(ctx, `
		INSERT INTO llm_usage_events (
		    tenant_id, user_id, agent_id, run_id, session_id, source,
		    provider, model, tokens_in, tokens_out, cache_read, cache_write,
		    cost_micros, user_groups, quantity, unit)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
			ev.TenantID, ev.UserID, ev.AgentID, ev.RunID, ev.SessionID, ev.Source,
			ev.Provider, ev.Model, ev.TokensIn, ev.TokensOut, ev.CacheRead, ev.CacheWrite,
			ev.CostMicros, groups, ev.Quantity, ev.Unit,
		)
		if err != nil {
			return fmt.Errorf("insert llm_usage_event: %w", err)
		}
		return nil
	})
}

// SessionUsageRow is one (provider, model) rollup of a single session's calls.
type SessionUsageRow struct {
	Provider   string
	Model      string
	TokensIn   int64
	TokensOut  int64
	CacheRead  int64
	CacheWrite int64
	CostMicros int64
	Calls      int64
}

// SessionTotals rolls one session's billable calls up by (provider, model),
// most expensive first so a caller rendering a single "primary model" label can
// take rows[0] without re-sorting.
//
// Scoped to userID as well as tenant. RLS already confines the query to the
// tenant, but a session belongs to the one user who ran it and this backs a
// per-user surface, so the user predicate is what keeps it from becoming a
// cross-user read for anyone who can guess a session id. The caller passes an
// identity it derived from the request context, never a client-supplied field.
//
// An empty sessionID returns no rows rather than every unattributed event:
// session_id is "" on paths that have no session (external invoke, and
// parent-side calls made outside a session), and lumping those together under
// one caller would be wrong in both directions.
func (r *UsageEventRepo) SessionTotals(ctx context.Context, tenantID, userID, sessionID string) ([]SessionUsageRow, error) {
	if sessionID == "" {
		return nil, nil
	}
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) ([]SessionUsageRow, error) {
		rows, err := conn.Query(ctx, `
		SELECT provider,
		       model,
		       COALESCE(SUM(tokens_in), 0)   AS tokens_in,
		       COALESCE(SUM(tokens_out), 0)  AS tokens_out,
		       COALESCE(SUM(cache_read), 0)  AS cache_read,
		       COALESCE(SUM(cache_write), 0) AS cache_write,
		       COALESCE(SUM(cost_micros), 0) AS cost_micros,
		       COUNT(*)                      AS calls
		  FROM llm_usage_events
		 WHERE tenant_id = $1 AND user_id = $2 AND session_id = $3
		 GROUP BY provider, model
		 ORDER BY cost_micros DESC, tokens_out DESC, model ASC`,
			tenantID, userID, sessionID)
		if err != nil {
			return nil, fmt.Errorf("query session usage: %w", err)
		}
		defer rows.Close()

		out := []SessionUsageRow{}
		for rows.Next() {
			var row SessionUsageRow
			if err := rows.Scan(
				&row.Provider, &row.Model,
				&row.TokensIn, &row.TokensOut, &row.CacheRead, &row.CacheWrite,
				&row.CostMicros, &row.Calls,
			); err != nil {
				return nil, fmt.Errorf("scan session usage: %w", err)
			}
			out = append(out, row)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterate session usage: %w", err)
		}
		return out, nil
	})
}

// PruneBefore deletes every event with ts strictly before cutoff and returns
// the number removed — the retention sweep main.go's daily ticker drives.
//
// Same deliberate cross-tenant exception as agent_baselines.go's
// PruneWindowsBefore: retention is one sweep across all tenants, so there is no
// tenant to scope withTenant to and under the non-superuser `aikonos_app` role
// RLS would filter the DELETE to zero rows. The sweep therefore goes through
// the SECURITY DEFINER function llm_usage_prune_events (migration 045).
func (r *UsageEventRepo) PruneBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	return withConnUnscoped(ctx, r.pool, func(conn *pgxpool.Conn) (int64, error) {
		var removed int64
		if err := conn.QueryRow(ctx,
			`SELECT llm_usage_prune_events($1)`,
			cutoff,
		).Scan(&removed); err != nil {
			return 0, fmt.Errorf("prune llm_usage_events: %w", err)
		}
		return removed, nil
	})
}
