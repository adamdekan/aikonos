// broker/internal/db/agent_skill.go
// Repository for tenant-scoped agent-skill bundles (SKILL.md uploads).
// All queries set app.current_tenant for RLS. No raw SQL outside this package.
package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// ErrAgentSkillNotFound is returned by Get, GetByName, and Delete when no row
// exists for the given (tenant, id/name). Callers use errors.Is to map to
// codes.NotFound.
var ErrAgentSkillNotFound = errors.New("agent_skill not found")

// AgentSkill is one tenant's stored SKILL.md bundle.
type AgentSkill struct {
	TenantID               uuid.UUID
	ID                     uuid.UUID
	Name                   string
	Description            string
	Body                   string
	AllowedTools           []byte // raw JSONB; callers unmarshal as []string
	ContextFork            bool
	DisableModelInvocation bool
	Extras                 []byte // raw JSONB
	Keywords               []byte // raw JSONB; callers unmarshal as []string
	CreatedBy              string
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// NormalizeKeywords applies the write-time keyword contract (spec:
// auto-skill-loading.md CP1): lowercase, trim, drop empties, dedup
// case-insensitively while preserving first-seen order. Called by admin
// upsert paths before persisting — the gateway matcher relies on keywords
// already being in this canonical form.
func NormalizeKeywords(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, kw := range in {
		norm := strings.ToLower(strings.TrimSpace(kw))
		if norm == "" || seen[norm] {
			continue
		}
		seen[norm] = true
		out = append(out, norm)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// AgentSkillRepo persists agent_skill rows. Every query wraps its connection
// in withTenant for RLS.
type AgentSkillRepo struct {
	pool   *pgxpool.Pool
	logger *zap.Logger
}

// NewAgentSkillRepo constructs the repo. Nil pool is accepted; queries fail at
// call time (mirrors the other repos in this package).
func NewAgentSkillRepo(pool *pgxpool.Pool, logger *zap.Logger) *AgentSkillRepo {
	return &AgentSkillRepo{pool: pool, logger: logger}
}

// Upsert inserts or updates the bundle row for (tenant, row.ID).
// updated_at is always refreshed on conflict.
func (r *AgentSkillRepo) Upsert(ctx context.Context, tenant string, row AgentSkill) error {
	return withConnErr(ctx, r.pool, tenant, func(conn *pgxpool.Conn) error {
		allowedTools := row.AllowedTools
		if allowedTools == nil {
			allowedTools = []byte("[]")
		}
		extras := row.Extras
		if extras == nil {
			extras = []byte("{}")
		}
		keywords := row.Keywords
		if keywords == nil {
			keywords = []byte("[]")
		}
		_, err := conn.Exec(ctx, `
		INSERT INTO agent_skill
		    (tenant_id, id, name, description, body, allowed_tools,
		     context_fork, disable_model_invocation, extras, keywords, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (tenant_id, id) DO UPDATE
		    SET name                     = EXCLUDED.name,
		        description              = EXCLUDED.description,
		        body                     = EXCLUDED.body,
		        allowed_tools            = EXCLUDED.allowed_tools,
		        context_fork             = EXCLUDED.context_fork,
		        disable_model_invocation = EXCLUDED.disable_model_invocation,
		        extras                   = EXCLUDED.extras,
		        keywords                 = EXCLUDED.keywords,
		        updated_at               = NOW()`,
			tenant, row.ID, row.Name, row.Description, row.Body, allowedTools,
			row.ContextFork, row.DisableModelInvocation, extras, keywords, row.CreatedBy,
		)
		if err != nil {
			return fmt.Errorf("upsert agent_skill: %w", err)
		}
		return nil
	})
}

// Get returns the bundle row for (tenant, id), or ErrAgentSkillNotFound if absent.
func (r *AgentSkillRepo) Get(ctx context.Context, tenant string, id uuid.UUID) (*AgentSkill, error) {
	return withConn(ctx, r.pool, tenant, func(conn *pgxpool.Conn) (*AgentSkill, error) {
		row := conn.QueryRow(ctx, `
		SELECT tenant_id, id, name, description, body, allowed_tools,
		       context_fork, disable_model_invocation, extras, keywords,
		       created_by, created_at, updated_at
		  FROM agent_skill
		 WHERE id = $1 AND tenant_id = $2`,
			id, tenant,
		)
		var s AgentSkill
		if err := row.Scan(
			&s.TenantID, &s.ID, &s.Name, &s.Description, &s.Body, &s.AllowedTools,
			&s.ContextFork, &s.DisableModelInvocation, &s.Extras, &s.Keywords,
			&s.CreatedBy, &s.CreatedAt, &s.UpdatedAt,
		); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("agent_skill %s: %w", id, ErrAgentSkillNotFound)
			}
			return nil, fmt.Errorf("get agent_skill %s: %w", id, err)
		}
		return &s, nil
	})
}

// GetByName returns the bundle row for (tenant, name), or ErrAgentSkillNotFound
// if absent.
func (r *AgentSkillRepo) GetByName(ctx context.Context, tenant, name string) (*AgentSkill, error) {
	return withConn(ctx, r.pool, tenant, func(conn *pgxpool.Conn) (*AgentSkill, error) {
		row := conn.QueryRow(ctx, `
		SELECT tenant_id, id, name, description, body, allowed_tools,
		       context_fork, disable_model_invocation, extras, keywords,
		       created_by, created_at, updated_at
		  FROM agent_skill
		 WHERE name = $1 AND tenant_id = $2`,
			name, tenant,
		)
		var s AgentSkill
		if err := row.Scan(
			&s.TenantID, &s.ID, &s.Name, &s.Description, &s.Body, &s.AllowedTools,
			&s.ContextFork, &s.DisableModelInvocation, &s.Extras, &s.Keywords,
			&s.CreatedBy, &s.CreatedAt, &s.UpdatedAt,
		); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("agent_skill %q: %w", name, ErrAgentSkillNotFound)
			}
			return nil, fmt.Errorf("get agent_skill by name %q: %w", name, err)
		}
		return &s, nil
	})
}

// ListByTenant returns all bundle rows for tenant ordered by name.
func (r *AgentSkillRepo) ListByTenant(ctx context.Context, tenant string) ([]AgentSkill, error) {
	return withConn(ctx, r.pool, tenant, func(conn *pgxpool.Conn) ([]AgentSkill, error) {
		rows, err := conn.Query(ctx, `
		SELECT tenant_id, id, name, description, body, allowed_tools,
		       context_fork, disable_model_invocation, extras, keywords,
		       created_by, created_at, updated_at
		  FROM agent_skill
		 WHERE tenant_id = $1
		 ORDER BY name`, tenant)
		if err != nil {
			return nil, fmt.Errorf("list agent_skill: %w", err)
		}
		defer rows.Close()
		var out []AgentSkill
		for rows.Next() {
			var s AgentSkill
			if err := rows.Scan(
				&s.TenantID, &s.ID, &s.Name, &s.Description, &s.Body, &s.AllowedTools,
				&s.ContextFork, &s.DisableModelInvocation, &s.Extras, &s.Keywords,
				&s.CreatedBy, &s.CreatedAt, &s.UpdatedAt,
			); err != nil {
				return nil, err
			}
			out = append(out, s)
		}
		return out, rows.Err()
	})
}

// Delete removes the bundle row for (tenant, id).
// Returns ErrAgentSkillNotFound if no row exists.
func (r *AgentSkillRepo) Delete(ctx context.Context, tenant string, id uuid.UUID) error {
	return withConnErr(ctx, r.pool, tenant, func(conn *pgxpool.Conn) error {
		// AND tenant_id = $2: defense-in-depth against BYPASSRLS roles (mirrors
		// network_rules, provisioning, and mcp_connections repos).
		tag, err := conn.Exec(ctx,
			`DELETE FROM agent_skill WHERE id = $1 AND tenant_id = $2`,
			id, tenant,
		)
		if err != nil {
			return fmt.Errorf("delete agent_skill: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("agent_skill %s: %w", id, ErrAgentSkillNotFound)
		}
		return nil
	})
}
