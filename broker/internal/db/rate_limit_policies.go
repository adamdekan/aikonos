// broker/internal/db/rate_limit_policies.go
// Repository for per-tenant rate limit policies (rate_limit_policies table).
// All queries set app.current_tenant for RLS. No raw SQL outside this package.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// RateLimitPolicy is one tenant-scoped rate limit row.
// NULL db values (unconstrained) are represented as nil pointers.
// rpm_limit=0 / tpm_limit=0 means deny-all; nil means unconstrained.
type RateLimitPolicy struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	AgentID   *uuid.UUID // nil = applies to all agents
	Provider  string     // "" = applies to all providers
	RPMLimit  *int32     // nil = unconstrained; 0 = deny-all
	TPMLimit  *int32     // nil = unconstrained; 0 = deny-all
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// RateLimitPolicyRepo persists rate_limit_policies rows. Every query wraps its
// connection in withTenant for RLS.
type RateLimitPolicyRepo struct {
	pool   *pgxpool.Pool
	logger *zap.Logger
}

// NewRateLimitPolicyRepo constructs the repo.
func NewRateLimitPolicyRepo(pool *pgxpool.Pool, logger *zap.Logger) *RateLimitPolicyRepo {
	return &RateLimitPolicyRepo{pool: pool, logger: logger}
}

// List returns all rate limit policies for the given tenant, ordered by created_at.
func (r *RateLimitPolicyRepo) List(ctx context.Context, tenantID string) ([]RateLimitPolicy, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) ([]RateLimitPolicy, error) {
		rows, err := conn.Query(ctx, `
		SELECT id, tenant_id, agent_id, provider, rpm_limit, tpm_limit,
		       created_by, created_at, updated_at
		  FROM rate_limit_policies
		 WHERE tenant_id = $1
		 ORDER BY created_at`, tenantID)
		if err != nil {
			return nil, fmt.Errorf("list rate_limit_policies: %w", err)
		}
		defer rows.Close()
		var out []RateLimitPolicy
		for rows.Next() {
			var p RateLimitPolicy
			if err := rows.Scan(
				&p.ID, &p.TenantID, &p.AgentID, &p.Provider,
				&p.RPMLimit, &p.TPMLimit,
				&p.CreatedBy, &p.CreatedAt, &p.UpdatedAt,
			); err != nil {
				return nil, fmt.Errorf("scan rate_limit_policy: %w", err)
			}
			out = append(out, p)
		}
		return out, rows.Err()
	})
}

// ListAll returns every tenant's rate limit policies, ordered by created_at.
//
// For the broker's one-shot startup preload only: the in-process limiter holds a
// single flat policy set spanning all tenants, and at startup there is no tenant
// to scope an RLS-bounded read to. Reads through the migration 044 SECURITY
// DEFINER carve-out, the same sanctioned bypass 033/034 use for their
// legitimately cross-tenant sweeps. Every other caller must use List, which
// stays tenant-scoped and RLS-enforced.
//
// The tenant passed to withConn is only there to satisfy the connection helper's
// GUC set; the function body ignores app.current_tenant by design.
func (r *RateLimitPolicyRepo) ListAll(ctx context.Context) ([]RateLimitPolicy, error) {
	return withConn(ctx, r.pool, "", func(conn *pgxpool.Conn) ([]RateLimitPolicy, error) {
		rows, err := conn.Query(ctx, `SELECT * FROM rate_limit_policies_all()`)
		if err != nil {
			return nil, fmt.Errorf("list all rate_limit_policies: %w", err)
		}
		defer rows.Close()
		var out []RateLimitPolicy
		for rows.Next() {
			var p RateLimitPolicy
			if err := rows.Scan(
				&p.ID, &p.TenantID, &p.AgentID, &p.Provider,
				&p.RPMLimit, &p.TPMLimit,
				&p.CreatedBy, &p.CreatedAt, &p.UpdatedAt,
			); err != nil {
				return nil, fmt.Errorf("scan rate_limit_policy: %w", err)
			}
			out = append(out, p)
		}
		return out, rows.Err()
	})
}

// Upsert inserts or updates a rate limit policy identified by the scope
// (tenant_id, agent_id, provider). Returns the persisted row.
func (r *RateLimitPolicyRepo) Upsert(ctx context.Context, tenantID string, p RateLimitPolicy) (*RateLimitPolicy, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (*RateLimitPolicy, error) {
		var out RateLimitPolicy
		err := conn.QueryRow(ctx, `
		INSERT INTO rate_limit_policies
		    (tenant_id, agent_id, provider, rpm_limit, tpm_limit, created_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (tenant_id, COALESCE(agent_id::text,''), COALESCE(provider,''))
		DO UPDATE SET
		    rpm_limit  = EXCLUDED.rpm_limit,
		    tpm_limit  = EXCLUDED.tpm_limit,
		    created_by = EXCLUDED.created_by,
		    updated_at = NOW()
		RETURNING id, tenant_id, agent_id, provider, rpm_limit, tpm_limit,
		          created_by, created_at, updated_at`,
			tenantID, p.AgentID, p.Provider, p.RPMLimit, p.TPMLimit, p.CreatedBy,
		).Scan(
			&out.ID, &out.TenantID, &out.AgentID, &out.Provider,
			&out.RPMLimit, &out.TPMLimit,
			&out.CreatedBy, &out.CreatedAt, &out.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("upsert rate_limit_policy: %w", err)
		}
		return &out, nil
	})
}

// Delete removes a rate limit policy by id within the tenant.
// No error is returned when the row does not exist (idempotent).
func (r *RateLimitPolicyRepo) Delete(ctx context.Context, tenantID, id string) error {
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		// AND tenant_id = $2: defense-in-depth against BYPASSRLS roles.
		_, err := conn.Exec(ctx,
			`DELETE FROM rate_limit_policies WHERE id = $1 AND tenant_id = $2`,
			id, tenantID,
		)
		if err != nil {
			return fmt.Errorf("delete rate_limit_policy: %w", err)
		}
		return nil
	})
}
