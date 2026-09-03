// broker/internal/db/provisioning.go
// Repository for email-based JIT user provisioning rules.
// All queries set app.current_tenant (RLS). No raw SQL outside this package.
package db

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// ProvisioningRule is one email→groups mapping for a tenant.
// Groups holds bare group names (no "group:" prefix); the applier adds it.
type ProvisioningRule struct {
	ID        uuid.UUID
	Matcher   string   // lowercase exact or "*@domain"
	Groups    []string // stored as JSONB
	CreatedBy string
	CreatedAt time.Time
}

// ProvisioningRepo persists provisioning_rules and handles the atomic
// first-seen claim on user_directory.
type ProvisioningRepo struct {
	pool   *pgxpool.Pool
	logger *zap.Logger
}

// NewProvisioningRepo constructs the repo. Nil pool is accepted; queries fail
// at call time (mirrors the other repos in this package).
func NewProvisioningRepo(pool *pgxpool.Pool, logger *zap.Logger) *ProvisioningRepo {
	return &ProvisioningRepo{pool: pool, logger: logger}
}

// ListRules returns all provisioning rules for tenantID ordered by matcher.
func (r *ProvisioningRepo) ListRules(ctx context.Context, tenantID string) ([]ProvisioningRule, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) ([]ProvisioningRule, error) {
		rows, err := conn.Query(ctx,
			`SELECT id, matcher, groups, created_by, created_at
		   FROM provisioning_rules
		  ORDER BY matcher`)
		if err != nil {
			return nil, fmt.Errorf("list provisioning_rules: %w", err)
		}
		defer rows.Close()
		var out []ProvisioningRule
		for rows.Next() {
			var pr ProvisioningRule
			var groupsJSON []byte
			if err := rows.Scan(&pr.ID, &pr.Matcher, &groupsJSON, &pr.CreatedBy, &pr.CreatedAt); err != nil {
				return nil, err
			}
			if err := json.Unmarshal(groupsJSON, &pr.Groups); err != nil {
				return nil, fmt.Errorf("decode groups JSON: %w", err)
			}
			out = append(out, pr)
		}
		return out, rows.Err()
	})
}

// AddRule inserts a provisioning rule. A UNIQUE(tenant_id, matcher) violation
// is returned as a descriptive error rather than a raw pgx error.
func (r *ProvisioningRepo) AddRule(ctx context.Context, tenantID string, rule ProvisioningRule) error {
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		groupsJSON, err := json.Marshal(rule.Groups)
		if err != nil {
			return fmt.Errorf("marshal groups: %w", err)
		}
		tenantUUID, err := uuid.Parse(tenantID)
		if err != nil {
			return fmt.Errorf("invalid tenant_id: %w", err)
		}
		_, execErr := conn.Exec(ctx,
			`INSERT INTO provisioning_rules (tenant_id, id, matcher, groups, created_by)
		 VALUES ($1, $2, $3, $4, $5)`,
			tenantUUID, rule.ID, rule.Matcher, groupsJSON, rule.CreatedBy,
		)
		if execErr != nil {
			// Map the unique-constraint violation to a friendly message.
			if strings.Contains(execErr.Error(), "duplicate") || strings.Contains(execErr.Error(), "unique") {
				return fmt.Errorf("provisioning rule for matcher %q already exists", rule.Matcher)
			}
			return fmt.Errorf("insert provisioning_rule: %w", execErr)
		}
		return nil
	})
}

// DeleteRule removes the rule with the given id for the tenant.
func (r *ProvisioningRepo) DeleteRule(ctx context.Context, tenantID, id string) error {
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		tenantUUID, err := uuid.Parse(tenantID)
		if err != nil {
			return fmt.Errorf("invalid tenant_id: %w", err)
		}
		ruleID, err := uuid.Parse(id)
		if err != nil {
			return fmt.Errorf("invalid rule id: %w", err)
		}
		// AND tenant_id = $2: defense-in-depth against BYPASSRLS roles (mirrors
		// network_rules and roles repos which also bind the tenant in the predicate).
		tag, err := conn.Exec(ctx,
			`DELETE FROM provisioning_rules WHERE id = $1 AND tenant_id = $2`,
			ruleID, tenantUUID)
		if err != nil {
			return fmt.Errorf("delete provisioning_rule: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("provisioning_rule %s not found", id)
		}
		return nil
	})
}

// ClaimProvisioning atomically marks subject as provisioned for the first time.
// Returns claimed=true when this call wins the race (provisioned_at was NULL).
// The single statement is safe across broker replicas: the WHERE clause ensures
// only one writer wins.
func (r *ProvisioningRepo) ClaimProvisioning(ctx context.Context, tenantID, subject, email, name string) (claimed bool, err error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (bool, error) {
		tenantUUID, err := uuid.Parse(tenantID)
		if err != nil {
			return false, fmt.Errorf("invalid tenant_id: %w", err)
		}

		rows, err := conn.Query(ctx, `
		INSERT INTO user_directory (tenant_id, subject, email, name, last_seen, provisioned_at)
		VALUES ($1, $2, $3, $4, NOW(), NOW())
		ON CONFLICT (tenant_id, subject) DO UPDATE
		  SET provisioned_at = NOW(),
		      email = CASE WHEN EXCLUDED.email <> '' THEN EXCLUDED.email ELSE user_directory.email END,
		      last_seen = NOW()
		  WHERE user_directory.provisioned_at IS NULL
		RETURNING subject`,
			tenantUUID, subject, email, name,
		)
		if err != nil {
			return false, fmt.Errorf("claim provisioning: %w", err)
		}
		defer rows.Close()
		if rows.Next() {
			return true, nil
		}
		return false, rows.Err()
	})
}

// SubjectsByEmail returns the FGA subject strings for all users in
// user_directory whose email matches (case-insensitive exact match).
// Service principal subjects (svc- prefix) are excluded.
func (r *ProvisioningRepo) SubjectsByEmail(ctx context.Context, tenantID, email string) ([]string, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) ([]string, error) {
		rows, err := conn.Query(ctx,
			`SELECT subject FROM user_directory
		  WHERE LOWER(email) = LOWER($1)
		    AND subject NOT LIKE 'svc-%'`,
			email,
		)
		if err != nil {
			return nil, fmt.Errorf("subjects_by_email: %w", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return nil, err
			}
			out = append(out, s)
		}
		return out, rows.Err()
	})
}
