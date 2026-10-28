// broker/internal/db/agents.go
// Repository for named per-tenant agent configs (agents table). Every query
// sets app.current_tenant for RLS. No raw SQL outside this package.
package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// ErrAgentNotFound is returned when Get/Update/Delete targets a row that does
// not exist in the caller's tenant.
var ErrAgentNotFound = errors.New("agent not found")

type Agent struct {
	ID                uuid.UUID
	TenantID          uuid.UUID
	Name              string
	LLMModel          string
	ApprovalMode      string
	Skills            []string
	McpServers        []string
	AllowedProviders  []string
	PreferredProvider string
	Soul              string
	GatewayEnabled    bool
	ServiceAgent      bool // AG-17: may run subject-less (external tk_ key, no user)
	CreatedBy         string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	RevokedBefore     *time.Time // AG-06 revocation epoch: credentials issued before it are void
	IsDefault         bool       // AG-01: the tenant default agent (EnsureDefault creates it)
	Status            string     // AG-03: staged | active | suspended | retired; only active runs
	StatusReason      string     // AG-03: reason recorded with the last status change
}

// AG-03 agent lifecycle states (agents.status, migration 056).
const (
	AgentStaged    = "staged"
	AgentActive    = "active"
	AgentSuspended = "suspended"
	AgentRetired   = "retired"
)

type AgentRepo struct {
	pool   *pgxpool.Pool
	logger *zap.Logger
}

func NewAgentRepo(pool *pgxpool.Pool, logger *zap.Logger) *AgentRepo {
	return &AgentRepo{pool: pool, logger: logger}
}

const agentCols = `id, tenant_id, name, llm_model, approval_mode, skills, mcp_servers, allowed_providers, preferred_provider, soul, gateway_enabled, service_agent, created_by, created_at, updated_at, revoked_before, is_default, status, status_reason`

func scanAgent(row interface {
	Scan(dest ...any) error
}) (*Agent, error) {
	var a Agent
	var skillsJSON, mcpJSON, allowedProvidersJSON []byte
	if err := row.Scan(&a.ID, &a.TenantID, &a.Name, &a.LLMModel, &a.ApprovalMode,
		&skillsJSON, &mcpJSON, &allowedProvidersJSON, &a.PreferredProvider,
		&a.Soul, &a.GatewayEnabled, &a.ServiceAgent, &a.CreatedBy, &a.CreatedAt, &a.UpdatedAt, &a.RevokedBefore, &a.IsDefault, &a.Status, &a.StatusReason); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(skillsJSON, &a.Skills); err != nil {
		return nil, fmt.Errorf("unmarshal skills: %w", err)
	}
	if err := json.Unmarshal(mcpJSON, &a.McpServers); err != nil {
		return nil, fmt.Errorf("unmarshal mcp_servers: %w", err)
	}
	if err := json.Unmarshal(allowedProvidersJSON, &a.AllowedProviders); err != nil {
		return nil, fmt.Errorf("unmarshal allowed_providers: %w", err)
	}
	if a.Skills == nil {
		a.Skills = []string{}
	}
	if a.McpServers == nil {
		a.McpServers = []string{}
	}
	if a.AllowedProviders == nil {
		a.AllowedProviders = []string{}
	}
	return &a, nil
}

func (r *AgentRepo) Create(ctx context.Context, a *Agent) error {
	return withConnErr(ctx, r.pool, a.TenantID.String(), func(conn *pgxpool.Conn) error {
		if a.AllowedProviders == nil {
			a.AllowedProviders = []string{}
		}
		skillsJSON, err := json.Marshal(a.Skills)
		if err != nil {
			return fmt.Errorf("marshal skills: %w", err)
		}
		mcpJSON, err := json.Marshal(a.McpServers)
		if err != nil {
			return fmt.Errorf("marshal mcp_servers: %w", err)
		}
		allowedProvidersJSON, err := json.Marshal(a.AllowedProviders)
		if err != nil {
			return fmt.Errorf("marshal allowed_providers: %w", err)
		}
		_, err = conn.Exec(ctx, `
		INSERT INTO agents (id, tenant_id, name, llm_model, approval_mode, skills, mcp_servers, allowed_providers, preferred_provider, gateway_enabled, service_agent, created_by, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, COALESCE(NULLIF($13, ''), 'staged'))`,
			a.ID, a.TenantID, a.Name, a.LLMModel, a.ApprovalMode, skillsJSON, mcpJSON, allowedProvidersJSON, a.PreferredProvider, a.GatewayEnabled, a.ServiceAgent, a.CreatedBy, a.Status,
		)
		if err != nil {
			return fmt.Errorf("insert agent: %w", err)
		}
		return nil
	})
}

func (r *AgentRepo) List(ctx context.Context, tenantID string) ([]*Agent, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) ([]*Agent, error) {
		rows, err := conn.Query(ctx, `SELECT `+agentCols+` FROM agents ORDER BY name`)
		if err != nil {
			return nil, fmt.Errorf("list agents: %w", err)
		}
		defer rows.Close()
		var out []*Agent
		for rows.Next() {
			a, err := scanAgent(rows)
			if err != nil {
				return nil, err
			}
			out = append(out, a)
		}
		return out, rows.Err()
	})
}

func (r *AgentRepo) Get(ctx context.Context, tenantID, id string) (*Agent, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (*Agent, error) {
		row := conn.QueryRow(ctx, `SELECT `+agentCols+` FROM agents WHERE id = $1 AND tenant_id = $2`, id, tenantID)
		a, err := scanAgent(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAgentNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("get agent: %w", err)
		}
		return a, nil
	})
}

func (r *AgentRepo) Update(ctx context.Context, a *Agent) error {
	return withConnErr(ctx, r.pool, a.TenantID.String(), func(conn *pgxpool.Conn) error {
		if a.AllowedProviders == nil {
			a.AllowedProviders = []string{}
		}
		skillsJSON, err := json.Marshal(a.Skills)
		if err != nil {
			return fmt.Errorf("marshal skills: %w", err)
		}
		mcpJSON, err := json.Marshal(a.McpServers)
		if err != nil {
			return fmt.Errorf("marshal mcp_servers: %w", err)
		}
		allowedProvidersJSON, err := json.Marshal(a.AllowedProviders)
		if err != nil {
			return fmt.Errorf("marshal allowed_providers: %w", err)
		}
		tag, err := conn.Exec(ctx, `
		UPDATE agents
		SET name=$1, llm_model=$2, approval_mode=$3, skills=$4, mcp_servers=$5, allowed_providers=$6, preferred_provider=$7, gateway_enabled=$8, service_agent=$9, updated_at=NOW()
		WHERE id=$10 AND tenant_id=$11`,
			a.Name, a.LLMModel, a.ApprovalMode, skillsJSON, mcpJSON, allowedProvidersJSON, a.PreferredProvider, a.GatewayEnabled, a.ServiceAgent, a.ID, a.TenantID,
		)
		if err != nil {
			return fmt.Errorf("update agent: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrAgentNotFound
		}
		return nil
	})
}

func (r *AgentRepo) Delete(ctx context.Context, tenantID, id string) error {
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		tag, err := conn.Exec(ctx, `DELETE FROM agents WHERE id = $1 AND tenant_id = $2 AND NOT is_default`, id, tenantID)
		if err != nil {
			return fmt.Errorf("delete agent: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrAgentNotFound
		}
		return nil
	})
}

func (r *AgentRepo) SetSoul(ctx context.Context, tenantID, id, soul string) error {
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		tag, err := conn.Exec(ctx,
			`UPDATE agents SET soul=$1, updated_at=NOW() WHERE id=$2 AND tenant_id=$3`,
			soul, id, tenantID,
		)
		if err != nil {
			return fmt.Errorf("set soul: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrAgentNotFound
		}
		return nil
	})
}

// DefaultAgentName is the name the tenant default agent is created with.
const DefaultAgentName = "Default agent"

// EnsureDefault returns the tenant default agent (AG-01), creating it when the
// tenant has none. created reports whether this call inserted it. Idempotent
// and race-safe: the partial unique index agents_one_default decides.
func (r *AgentRepo) EnsureDefault(ctx context.Context, tenantID string) (*Agent, bool, error) {
	var created bool
	a, err := withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (*Agent, error) {
		tag, err := conn.Exec(ctx, `
		INSERT INTO agents (tenant_id, name, created_by, is_default, status)
		VALUES ($1, $2, '', TRUE, 'active')
		ON CONFLICT (tenant_id) WHERE is_default DO NOTHING`, tenantID, DefaultAgentName)
		if err != nil {
			return nil, fmt.Errorf("insert default agent: %w", err)
		}
		created = tag.RowsAffected() == 1
		row := conn.QueryRow(ctx, `SELECT `+agentCols+` FROM agents WHERE tenant_id = $1 AND is_default`, tenantID)
		a, err := scanAgent(row)
		if err != nil {
			return nil, fmt.Errorf("get default agent: %w", err)
		}
		return a, nil
	})
	return a, created, err
}

// UserDefault returns the user's default-agent setting, "" when unset.
func (r *AgentRepo) UserDefault(ctx context.Context, tenantID, userID string) (string, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (string, error) {
		var id uuid.UUID
		err := conn.QueryRow(ctx,
			`SELECT agent_id FROM user_default_agents WHERE tenant_id = $1 AND user_id = $2`,
			tenantID, userID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("get user default agent: %w", err)
		}
		return id.String(), nil
	})
}

// SetUserDefault stores the user's default-agent setting; "" clears it.
func (r *AgentRepo) SetUserDefault(ctx context.Context, tenantID, userID, agentID string) error {
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		var err error
		if agentID == "" {
			_, err = conn.Exec(ctx,
				`DELETE FROM user_default_agents WHERE tenant_id = $1 AND user_id = $2`, tenantID, userID)
		} else {
			_, err = conn.Exec(ctx, `
			INSERT INTO user_default_agents (tenant_id, user_id, agent_id) VALUES ($1, $2, $3)
			ON CONFLICT (tenant_id, user_id) DO UPDATE SET agent_id = EXCLUDED.agent_id, updated_at = NOW()`,
				tenantID, userID, agentID)
		}
		if err != nil {
			return fmt.Errorf("set user default agent: %w", err)
		}
		return nil
	})
}
