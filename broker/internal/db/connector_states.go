// broker/internal/db/connector_states.go
// Repository for connector OAuth state (connector_oauth_states). Every query
// sets app.current_tenant for RLS. No raw SQL outside this package.
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

// ConnectorState is one in-flight OAuth round-trip's server-only context.
type ConnectorState struct {
	State     string
	TenantID  string
	UserID    string
	Provider  string
	Verifier  string
	Scopes    []string
	ExpiresAt time.Time
}

type ConnectorStateRepo struct {
	pool   *pgxpool.Pool
	logger *zap.Logger
}

func NewConnectorStateRepo(pool *pgxpool.Pool, logger *zap.Logger) *ConnectorStateRepo {
	return &ConnectorStateRepo{pool: pool, logger: logger}
}

// Put records a pending auth and sweeps the tenant's expired rows so abandoned
// flows' verifier material is not retained past its TTL.
func (r *ConnectorStateRepo) Put(ctx context.Context, st ConnectorState) error {
	return withConnErr(ctx, r.pool, st.TenantID, func(conn *pgxpool.Conn) error {
		if _, err := conn.Exec(ctx, `DELETE FROM connector_oauth_states WHERE expires_at < NOW()`); err != nil {
			r.logger.Debug("connector state sweep failed", zap.Error(err))
		}
		_, err := conn.Exec(ctx, `
		INSERT INTO connector_oauth_states (state, tenant_id, user_id, provider, verifier, scopes, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			st.State, st.TenantID, st.UserID, st.Provider, st.Verifier, st.Scopes, st.ExpiresAt,
		)
		if err != nil {
			return fmt.Errorf("insert connector_oauth_state: %w", err)
		}
		return nil
	})
}

// Take atomically claims and deletes the state (single use), returning false if
// it is unknown, expired, or belongs to another tenant (RLS). The DELETE ...
// RETURNING makes the claim race-free across replicas.
func (r *ConnectorStateRepo) Take(ctx context.Context, tenantID, state string) (*ConnectorState, bool, error) {
	return withConn2(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (*ConnectorState, bool, error) {
		// Filter tenant_id explicitly (not only via RLS) so the claim is
		// tenant-correct even where the connection role owns the table and bypasses
		// row-level security.
		var st ConnectorState
		err := conn.QueryRow(ctx, `
		DELETE FROM connector_oauth_states
		WHERE state = $1 AND tenant_id = $2 AND expires_at > NOW()
		RETURNING state, tenant_id, user_id, provider, verifier, scopes, expires_at`,
			state, tenantID,
		).Scan(&st.State, &st.TenantID, &st.UserID, &st.Provider, &st.Verifier, &st.Scopes, &st.ExpiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("take connector_oauth_state: %w", err)
		}
		return &st, true, nil
	})
}
