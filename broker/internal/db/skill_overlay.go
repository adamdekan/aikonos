// broker/internal/db/skill_overlay.go
// Repository for the tenant-scoped skill overlay.
// All queries set app.current_tenant for RLS. No raw SQL outside this package.
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// ErrSkillOverlayNotFound is returned by Delete when no row exists for the
// given (tenant, tool_id) pair. Callers use errors.Is to map to codes.NotFound.
var ErrSkillOverlayNotFound = errors.New("skill_overlay not found")

// SkillOverlay is one tenant's annotation/curation record for a single tool.
type SkillOverlay struct {
	ToolID          string
	ExecutorKind    string // "builtin" | "mcp"; derived at write, not user-set
	CapabilityScope string // derived (I3); stored for display
	EffectClass     string // authoritative stored class (I4)
	DisplayName     string
	Description     string
	Enabled         bool
	CreatedBy       string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// SkillOverlayRepo persists skill_overlay rows. Every query wraps its
// connection in withTenant for RLS.
type SkillOverlayRepo struct {
	pool   *pgxpool.Pool
	logger *zap.Logger
}

// NewSkillOverlayRepo constructs the repo. Nil pool is accepted; queries fail
// at call time (mirrors the other repos in this package).
func NewSkillOverlayRepo(pool *pgxpool.Pool, logger *zap.Logger) *SkillOverlayRepo {
	return &SkillOverlayRepo{pool: pool, logger: logger}
}

// Upsert inserts or updates the overlay row for (tenant, row.ToolID).
// updated_at is always refreshed on conflict.
func (r *SkillOverlayRepo) Upsert(ctx context.Context, tenant string, row SkillOverlay) error {
	return withConnErr(ctx, r.pool, tenant, func(conn *pgxpool.Conn) error {
		_, err := conn.Exec(ctx, `
		INSERT INTO skill_overlay
		    (tenant_id, tool_id, executor_kind, capability_scope, effect_class,
		     display_name, description, enabled, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (tenant_id, tool_id) DO UPDATE
		    SET executor_kind    = EXCLUDED.executor_kind,
		        capability_scope = EXCLUDED.capability_scope,
		        effect_class     = EXCLUDED.effect_class,
		        display_name     = EXCLUDED.display_name,
		        description      = EXCLUDED.description,
		        enabled          = EXCLUDED.enabled,
		        updated_at       = NOW()`,
			tenant, row.ToolID, row.ExecutorKind, row.CapabilityScope, row.EffectClass,
			row.DisplayName, row.Description, row.Enabled, row.CreatedBy,
		)
		if err != nil {
			return fmt.Errorf("upsert skill_overlay: %w", err)
		}
		return nil
	})
}

// Delete removes the overlay row for (tenant, toolID).
func (r *SkillOverlayRepo) Delete(ctx context.Context, tenant, toolID string) error {
	return withConnErr(ctx, r.pool, tenant, func(conn *pgxpool.Conn) error {
		// AND tenant_id = $2: defense-in-depth against BYPASSRLS roles (mirrors
		// network_rules, provisioning, and mcp_connections repos).
		tag, err := conn.Exec(ctx,
			`DELETE FROM skill_overlay WHERE tool_id = $1 AND tenant_id = $2`,
			toolID, tenant,
		)
		if err != nil {
			return fmt.Errorf("delete skill_overlay: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("skill_overlay %q: %w", toolID, ErrSkillOverlayNotFound)
		}
		return nil
	})
}

// Get returns the overlay row for (tenant, toolID), or nil if absent.
func (r *SkillOverlayRepo) Get(ctx context.Context, tenant, toolID string) (*SkillOverlay, error) {
	return withConn(ctx, r.pool, tenant, func(conn *pgxpool.Conn) (*SkillOverlay, error) {
		row := conn.QueryRow(ctx, `
		SELECT tool_id, executor_kind, capability_scope, effect_class,
		       display_name, description, enabled, created_by, created_at, updated_at
		  FROM skill_overlay
		 WHERE tool_id = $1 AND tenant_id = $2`,
			toolID, tenant,
		)
		var s SkillOverlay
		if err := row.Scan(
			&s.ToolID, &s.ExecutorKind, &s.CapabilityScope, &s.EffectClass,
			&s.DisplayName, &s.Description, &s.Enabled, &s.CreatedBy,
			&s.CreatedAt, &s.UpdatedAt,
		); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, nil
			}
			return nil, fmt.Errorf("get skill_overlay %q: %w", toolID, err)
		}
		return &s, nil
	})
}

// ListByTenant returns all overlay rows for tenant ordered by tool_id.
func (r *SkillOverlayRepo) ListByTenant(ctx context.Context, tenant string) ([]SkillOverlay, error) {
	return withConn(ctx, r.pool, tenant, func(conn *pgxpool.Conn) ([]SkillOverlay, error) {
		rows, err := conn.Query(ctx, `
		SELECT tool_id, executor_kind, capability_scope, effect_class,
		       display_name, description, enabled, created_by, created_at, updated_at
		  FROM skill_overlay
		 WHERE tenant_id = $1
		 ORDER BY tool_id`, tenant)
		if err != nil {
			return nil, fmt.Errorf("list skill_overlay: %w", err)
		}
		defer rows.Close()
		var out []SkillOverlay
		for rows.Next() {
			var s SkillOverlay
			if err := rows.Scan(
				&s.ToolID, &s.ExecutorKind, &s.CapabilityScope, &s.EffectClass,
				&s.DisplayName, &s.Description, &s.Enabled, &s.CreatedBy,
				&s.CreatedAt, &s.UpdatedAt,
			); err != nil {
				return nil, err
			}
			out = append(out, s)
		}
		return out, rows.Err()
	})
}
