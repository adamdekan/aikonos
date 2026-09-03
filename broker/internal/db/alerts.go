// broker/internal/db/alerts.go
// Repository for the persistent alert history table. Every query sets
// app.current_tenant for RLS. No raw SQL outside this package.
package db

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/alerting"
)

// AlertRepo reads and writes the alerts table.
// It satisfies alerting.AlertRepo so the Engine can persist fired alerts.
type AlertRepo struct {
	pool   *pgxpool.Pool
	logger *zap.Logger
}

// NewAlertRepo constructs an AlertRepo backed by pool.
func NewAlertRepo(pool *pgxpool.Pool, logger *zap.Logger) *AlertRepo {
	return &AlertRepo{pool: pool, logger: logger}
}

// Insert persists a fired alert. Caller supplies the UUIDv7 alert_id.
func (r *AlertRepo) Insert(ctx context.Context, a alerting.StoredAlert) error {
	tenantUUID, err := uuid.Parse(a.TenantID)
	if err != nil {
		return fmt.Errorf("insert alert: invalid tenant_id %q: %w", a.TenantID, err)
	}

	summary, err := json.Marshal(a.Summary)
	if err != nil {
		return fmt.Errorf("insert alert: marshal summary: %w", err)
	}

	return withConnErr(ctx, r.pool, a.TenantID, func(conn *pgxpool.Conn) error {
		_, err = conn.Exec(ctx, `
		INSERT INTO alerts (alert_id, tenant_id, rule, severity, summary, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (alert_id) DO NOTHING`,
			a.ID, tenantUUID, a.Rule, a.Severity, summary, a.FiredAt,
		)
		if err != nil {
			return fmt.Errorf("insert alert: %w", err)
		}
		return nil
	})
}

// List returns the most recent alerts for tenantID, newest first, capped at
// limit rows. limit ≤ 0 returns all rows (bounded by the index scan).
func (r *AlertRepo) List(ctx context.Context, tenantID string, limit int) ([]alerting.StoredAlert, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) ([]alerting.StoredAlert, error) {
		query := `SELECT alert_id, tenant_id, rule, severity, summary, created_at
		FROM alerts ORDER BY created_at DESC`
		args := []any{}
		if limit > 0 {
			query += " LIMIT $1"
			args = append(args, limit)
		}

		rows, err := conn.Query(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("list alerts: %w", err)
		}
		defer rows.Close()

		var out []alerting.StoredAlert
		for rows.Next() {
			var a alerting.StoredAlert
			var summaryJSON []byte
			var tenantUUID uuid.UUID
			if err := rows.Scan(&a.ID, &tenantUUID, &a.Rule, &a.Severity, &summaryJSON, &a.FiredAt); err != nil {
				return nil, fmt.Errorf("scan alert: %w", err)
			}
			a.TenantID = tenantUUID.String()
			if err := json.Unmarshal(summaryJSON, &a.Summary); err != nil {
				a.Summary = map[string]interface{}{}
			}
			out = append(out, a)
		}
		return out, rows.Err()
	})
}
