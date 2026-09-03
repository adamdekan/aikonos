// broker/internal/db/spend_caps.go
// Repositories for monthly LLM spend caps (spend_caps) and the durable
// per-period spend accumulator (llm_spend_counters). .
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// SpendCapScope is one of "org", "user", "agent".
type SpendCapScope = string

const (
	SpendCapScopeOrg   SpendCapScope = "org"
	SpendCapScopeUser  SpendCapScope = "user"
	SpendCapScopeAgent SpendCapScope = "agent"
)

// SpendCap is one admin-set monthly budget row. SubjectID is "" for scope=org.
type SpendCap struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	Scope     SpendCapScope
	SubjectID string
	CapMicros int64
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SpendCapRepo persists spend_caps rows. Every query wraps its connection in
// withTenant for RLS.
type SpendCapRepo struct {
	pool   *pgxpool.Pool
	logger *zap.Logger
}

func NewSpendCapRepo(pool *pgxpool.Pool, logger *zap.Logger) *SpendCapRepo {
	return &SpendCapRepo{pool: pool, logger: logger}
}

// List returns all spend caps for the given tenant, ordered by created_at.
func (r *SpendCapRepo) List(ctx context.Context, tenantID string) ([]SpendCap, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) ([]SpendCap, error) {
		rows, err := conn.Query(ctx, `
		SELECT id, tenant_id, scope, subject_id, cap_micros, created_by, created_at, updated_at
		  FROM spend_caps
		 WHERE tenant_id = $1
		 ORDER BY created_at`, tenantID)
		if err != nil {
			return nil, fmt.Errorf("list spend_caps: %w", err)
		}
		defer rows.Close()
		var out []SpendCap
		for rows.Next() {
			var c SpendCap
			if err := rows.Scan(
				&c.ID, &c.TenantID, &c.Scope, &c.SubjectID, &c.CapMicros,
				&c.CreatedBy, &c.CreatedAt, &c.UpdatedAt,
			); err != nil {
				return nil, fmt.Errorf("scan spend_cap: %w", err)
			}
			out = append(out, c)
		}
		return out, rows.Err()
	})
}

// Upsert inserts or updates a spend cap identified by the scope
// (tenant_id, scope, subject_id). Returns the persisted row.
func (r *SpendCapRepo) Upsert(ctx context.Context, tenantID string, c SpendCap) (*SpendCap, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (*SpendCap, error) {
		var out SpendCap
		err := conn.QueryRow(ctx, `
		INSERT INTO spend_caps (tenant_id, scope, subject_id, cap_micros, created_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, scope, subject_id)
		DO UPDATE SET
		    cap_micros = EXCLUDED.cap_micros,
		    created_by = EXCLUDED.created_by,
		    updated_at = NOW()
		RETURNING id, tenant_id, scope, subject_id, cap_micros, created_by, created_at, updated_at`,
			tenantID, c.Scope, c.SubjectID, c.CapMicros, c.CreatedBy,
		).Scan(
			&out.ID, &out.TenantID, &out.Scope, &out.SubjectID, &out.CapMicros,
			&out.CreatedBy, &out.CreatedAt, &out.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("upsert spend_cap: %w", err)
		}
		return &out, nil
	})
}

// Get returns the cap_micros for (tenant, scope, subjectID) and whether a cap
// row exists — the enforcement path's single-row read (List is for the admin
// CRUD/summary views, which need every row).
func (r *SpendCapRepo) Get(ctx context.Context, tenantID string, scope SpendCapScope, subjectID string) (int64, bool, error) {
	return withConn2(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (int64, bool, error) {
		var capMicros int64
		err := conn.QueryRow(ctx, `
		SELECT cap_micros FROM spend_caps
		 WHERE tenant_id = $1 AND scope = $2 AND subject_id = $3`,
			tenantID, scope, subjectID,
		).Scan(&capMicros)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return 0, false, nil
			}
			return 0, false, fmt.Errorf("get spend_cap: %w", err)
		}
		return capMicros, true, nil
	})
}

// Delete removes a spend cap by id within the tenant. No error is returned
// when the row does not exist (idempotent).
func (r *SpendCapRepo) Delete(ctx context.Context, tenantID, id string) error {
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		// AND tenant_id = $2: defense-in-depth against BYPASSRLS roles.
		_, err := conn.Exec(ctx,
			`DELETE FROM spend_caps WHERE id = $1 AND tenant_id = $2`,
			id, tenantID,
		)
		if err != nil {
			return fmt.Errorf("delete spend_cap: %w", err)
		}
		return nil
	})
}

// SpendPeriodStart returns the UTC calendar-month start for t — the sole
// period granularity spend-caps supports (no proration/custom periods).
func SpendPeriodStart(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// SubjectSpend is one grouped rollup row (per-user or per-agent totals).
type SubjectSpend struct {
	SubjectID  string
	CostMicros int64
	TokensIn   int64
	TokensOut  int64
}

// SpendCounterRepo persists llm_spend_counters rows: the durable per-period
// accumulator EmitLlmUsage writes to, and the rollup reads the enforcement
// path and dashboard need. Every query wraps its connection in withTenant.
type SpendCounterRepo struct {
	pool   *pgxpool.Pool
	logger *zap.Logger
}

func NewSpendCounterRepo(pool *pgxpool.Pool, logger *zap.Logger) *SpendCounterRepo {
	return &SpendCounterRepo{pool: pool, logger: logger}
}

// Accumulate adds costMicros/tokensIn/tokensOut onto the (tenant, user, agent,
// periodStart) counter row, creating it if absent. userID/agentID may be ""
// (e.g. an agentless personal chat call has agentID="").
func (r *SpendCounterRepo) Accumulate(ctx context.Context, tenantID, userID, agentID string, periodStart time.Time, costMicros, tokensIn, tokensOut int64) error {
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		_, err := conn.Exec(ctx, `
		INSERT INTO llm_spend_counters (tenant_id, user_id, agent_id, period_start, cost_micros, tokens_in, tokens_out)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (tenant_id, user_id, agent_id, period_start)
		DO UPDATE SET
		    cost_micros = llm_spend_counters.cost_micros + EXCLUDED.cost_micros,
		    tokens_in   = llm_spend_counters.tokens_in + EXCLUDED.tokens_in,
		    tokens_out  = llm_spend_counters.tokens_out + EXCLUDED.tokens_out,
		    updated_at  = NOW()`,
			tenantID, userID, agentID, periodStart, costMicros, tokensIn, tokensOut,
		)
		if err != nil {
			return fmt.Errorf("accumulate llm_spend_counters: %w", err)
		}
		return nil
	})
}

// OrgTotal returns the tenant-wide spend for periodStart (sum across every
// user/agent).
func (r *SpendCounterRepo) OrgTotal(ctx context.Context, tenantID string, periodStart time.Time) (int64, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (int64, error) {
		var total int64
		err := conn.QueryRow(ctx, `
		SELECT COALESCE(SUM(cost_micros), 0) FROM llm_spend_counters
		 WHERE tenant_id = $1 AND period_start = $2`,
			tenantID, periodStart,
		).Scan(&total)
		if err != nil {
			return 0, fmt.Errorf("org spend total: %w", err)
		}
		return total, nil
	})
}

// UserTotals returns per-user spend rollups (summed across agents) for
// periodStart, ordered by user_id.
func (r *SpendCounterRepo) UserTotals(ctx context.Context, tenantID string, periodStart time.Time) ([]SubjectSpend, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) ([]SubjectSpend, error) {
		rows, err := conn.Query(ctx, `
		SELECT user_id, SUM(cost_micros), SUM(tokens_in), SUM(tokens_out)
		  FROM llm_spend_counters
		 WHERE tenant_id = $1 AND period_start = $2 AND user_id <> ''
		 GROUP BY user_id
		 ORDER BY user_id`,
			tenantID, periodStart,
		)
		if err != nil {
			return nil, fmt.Errorf("user spend totals: %w", err)
		}
		defer rows.Close()
		var out []SubjectSpend
		for rows.Next() {
			var s SubjectSpend
			if err := rows.Scan(&s.SubjectID, &s.CostMicros, &s.TokensIn, &s.TokensOut); err != nil {
				return nil, fmt.Errorf("scan user spend total: %w", err)
			}
			out = append(out, s)
		}
		return out, rows.Err()
	})
}

// AgentTotals returns per-agent spend rollups (summed across users) for
// periodStart, ordered by agent_id. Rows with no agent attribution (agent_id
// = "") are excluded.
func (r *SpendCounterRepo) AgentTotals(ctx context.Context, tenantID string, periodStart time.Time) ([]SubjectSpend, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) ([]SubjectSpend, error) {
		rows, err := conn.Query(ctx, `
		SELECT agent_id, SUM(cost_micros), SUM(tokens_in), SUM(tokens_out)
		  FROM llm_spend_counters
		 WHERE tenant_id = $1 AND period_start = $2 AND agent_id <> ''
		 GROUP BY agent_id
		 ORDER BY agent_id`,
			tenantID, periodStart,
		)
		if err != nil {
			return nil, fmt.Errorf("agent spend totals: %w", err)
		}
		defer rows.Close()
		var out []SubjectSpend
		for rows.Next() {
			var s SubjectSpend
			if err := rows.Scan(&s.SubjectID, &s.CostMicros, &s.TokensIn, &s.TokensOut); err != nil {
				return nil, fmt.Errorf("scan agent spend total: %w", err)
			}
			out = append(out, s)
		}
		return out, rows.Err()
	})
}

// SubjectTotal is the single-subject read the enforcement path uses: for
// scope=org it's the tenant-wide total (subjectID ignored); for scope=user/
// agent it's that subject's total summed across the other dimension.
func (r *SpendCounterRepo) SubjectTotal(ctx context.Context, tenantID string, scope SpendCapScope, subjectID string, periodStart time.Time) (int64, error) {
	if scope == SpendCapScopeOrg {
		return r.OrgTotal(ctx, tenantID, periodStart)
	}
	col := "user_id"
	if scope == SpendCapScopeAgent {
		col = "agent_id"
	}
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (int64, error) {
		var total int64
		err := conn.QueryRow(ctx, `
		SELECT COALESCE(SUM(cost_micros), 0) FROM llm_spend_counters
		 WHERE tenant_id = $1 AND `+col+` = $2 AND period_start = $3`,
			tenantID, subjectID, periodStart,
		).Scan(&total)
		if err != nil {
			return 0, fmt.Errorf("subject spend total: %w", err)
		}
		return total, nil
	})
}
