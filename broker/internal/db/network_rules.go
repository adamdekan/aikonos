// broker/internal/db/network_rules.go
// Repository for the agent web access-list (network_rules). Every query sets
// app.current_tenant for RLS. No raw SQL outside this package.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

type NetworkScopeKind string

const (
	NetworkScopeTenant NetworkScopeKind = "TENANT"
	NetworkScopeGroup  NetworkScopeKind = "GROUP"
	NetworkScopeUser   NetworkScopeKind = "USER"
)

type NetworkAction string

const (
	NetworkAllow NetworkAction = "ALLOW"
	NetworkDeny  NetworkAction = "DENY"
	NetworkAsk   NetworkAction = "ASK"
)

type NetworkRule struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	ScopeKind   NetworkScopeKind
	ScopeValue  string
	Action      NetworkAction
	HostPattern string
	Note        string
	CreatedBy   string
	CreatedAt   time.Time
}

type NetworkRuleRepo struct {
	pool   *pgxpool.Pool
	logger *zap.Logger
}

func NewNetworkRuleRepo(pool *pgxpool.Pool, logger *zap.Logger) *NetworkRuleRepo {
	return &NetworkRuleRepo{pool: pool, logger: logger}
}

const networkRuleCols = `id, tenant_id, scope_kind, scope_value, action, host_pattern, note, created_by, created_at`

func scanNetworkRule(row interface {
	Scan(dest ...any) error
}) (*NetworkRule, error) {
	var r NetworkRule
	if err := row.Scan(&r.ID, &r.TenantID, &r.ScopeKind, &r.ScopeValue, &r.Action,
		&r.HostPattern, &r.Note, &r.CreatedBy, &r.CreatedAt); err != nil {
		return nil, err
	}
	return &r, nil
}

func (r *NetworkRuleRepo) List(ctx context.Context, tenantID string) ([]*NetworkRule, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) ([]*NetworkRule, error) {
		rows, err := conn.Query(ctx, `SELECT `+networkRuleCols+` FROM network_rules ORDER BY scope_kind, host_pattern`)
		if err != nil {
			return nil, fmt.Errorf("list network_rules: %w", err)
		}
		defer rows.Close()
		var out []*NetworkRule
		for rows.Next() {
			rule, err := scanNetworkRule(rows)
			if err != nil {
				return nil, err
			}
			out = append(out, rule)
		}
		return out, rows.Err()
	})
}

func (r *NetworkRuleRepo) Add(ctx context.Context, rule *NetworkRule) error {
	return withConnErr(ctx, r.pool, rule.TenantID.String(), func(conn *pgxpool.Conn) error {
		_, err := conn.Exec(ctx, `
		INSERT INTO network_rules (id, tenant_id, scope_kind, scope_value, action, host_pattern, note, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			rule.ID, rule.TenantID, rule.ScopeKind, rule.ScopeValue, rule.Action, rule.HostPattern, rule.Note, rule.CreatedBy,
		)
		if err != nil {
			return fmt.Errorf("insert network_rule: %w", err)
		}
		return nil
	})
}

func (r *NetworkRuleRepo) Delete(ctx context.Context, tenantID, id string) error {
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		tag, err := conn.Exec(ctx, `DELETE FROM network_rules WHERE id = $1`, id)
		if err != nil {
			return fmt.Errorf("delete network_rule: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("network_rule %s not found", id)
		}
		return nil
	})
}
