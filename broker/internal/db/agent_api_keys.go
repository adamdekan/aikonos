// broker/internal/db/agent_api_keys.go
// Repository for per-agent API keys (agent_api_keys table, migration 012).
// Every query acquires a connection and sets app.current_tenant for RLS.
// No raw SQL outside this package.
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

// StoredApiKey is a row from agent_api_keys.
type StoredApiKey struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	AgentID    uuid.UUID
	KeyHash    string // HMAC-SHA256(pepper, sha256(raw_key))
	KeyPrefix  string // first 8 chars of tk_… for display
	Label      string
	CreatedBy  string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// Revoked returns true when the key has been explicitly revoked.
func (k StoredApiKey) Revoked() bool { return k.RevokedAt != nil }

// ErrApiKeyNotFound is returned by Revoke/Resolve when no matching active key exists.
var ErrApiKeyNotFound = errors.New("agent_api_key not found")

// AgentApiKeyRepo implements broker.ApiKeyStore backed by Postgres.
type AgentApiKeyRepo struct {
	pool   *pgxpool.Pool
	logger *zap.Logger
}

func NewAgentApiKeyRepo(pool *pgxpool.Pool, logger *zap.Logger) *AgentApiKeyRepo {
	return &AgentApiKeyRepo{pool: pool, logger: logger}
}

func (r *AgentApiKeyRepo) Create(ctx context.Context, k *StoredApiKey) error {
	return withConnErr(ctx, r.pool, k.TenantID.String(), func(conn *pgxpool.Conn) error {
		_, err := conn.Exec(ctx, `
		INSERT INTO agent_api_keys
			(id, tenant_id, agent_id, key_hash, key_prefix, label, created_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			k.ID, k.TenantID, k.AgentID, k.KeyHash, k.KeyPrefix, k.Label, k.CreatedBy, k.CreatedAt,
		)
		return err
	})
}

func (r *AgentApiKeyRepo) ListByAgent(ctx context.Context, tenantID, agentID string) ([]*StoredApiKey, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) ([]*StoredApiKey, error) {
		aid, err := uuid.Parse(agentID)
		if err != nil {
			return nil, fmt.Errorf("invalid agent_id: %w", err)
		}
		rows, err := conn.Query(ctx, `
		SELECT id, tenant_id, agent_id, key_hash, key_prefix, label,
		       created_by, created_at, last_used_at, revoked_at
		FROM agent_api_keys
		WHERE agent_id = $1 AND revoked_at IS NULL
		ORDER BY created_at DESC`, aid)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []*StoredApiKey
		for rows.Next() {
			k := &StoredApiKey{}
			if err := rows.Scan(
				&k.ID, &k.TenantID, &k.AgentID, &k.KeyHash, &k.KeyPrefix, &k.Label,
				&k.CreatedBy, &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt,
			); err != nil {
				return nil, err
			}
			out = append(out, k)
		}
		return out, rows.Err()
	})
}

func (r *AgentApiKeyRepo) Revoke(ctx context.Context, tenantID, agentID, keyID string) error {
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		kid, err := uuid.Parse(keyID)
		if err != nil {
			return ErrApiKeyNotFound
		}
		aid, err := uuid.Parse(agentID)
		if err != nil {
			return ErrApiKeyNotFound
		}
		tid, err := uuid.Parse(tenantID)
		if err != nil {
			return ErrApiKeyNotFound
		}
		tag, err := conn.Exec(ctx,
			`UPDATE agent_api_keys SET revoked_at = NOW()
		 WHERE id = $1 AND agent_id = $2 AND tenant_id = $3 AND revoked_at IS NULL`,
			kid, aid, tid,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrApiKeyNotFound
		}
		return nil
	})
}

func (r *AgentApiKeyRepo) Resolve(ctx context.Context, keyHash string) (*StoredApiKey, error) {
	// Resolve does not filter by tenant — the hash is globally unique and the
	// inbound tenant is unknown until after resolution. agent_api_keys is
	// RLS-enforced under the non-superuser app role, so a direct SELECT with no
	// app.current_tenant set would see zero rows; the lookup goes through the
	// SECURITY DEFINER function resolve_agent_api_key (migration 034), the sole
	// sanctioned RLS bypass for this tenant-agnostic hash lookup. The broker
	// enforces tenant correctness against the returned tenant_id afterwards.
	k, err := withConnUnscoped(ctx, r.pool, func(conn *pgxpool.Conn) (*StoredApiKey, error) {
		k := &StoredApiKey{}
		err := conn.QueryRow(ctx,
			`SELECT id, tenant_id, agent_id, key_hash, key_prefix, label,
			        created_by, created_at, last_used_at, revoked_at
			 FROM resolve_agent_api_key($1)`, keyHash,
		).Scan(
			&k.ID, &k.TenantID, &k.AgentID, &k.KeyHash, &k.KeyPrefix, &k.Label,
			&k.CreatedBy, &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt,
		)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrApiKeyNotFound
			}
			return nil, err
		}
		return k, nil
	})
	if err != nil {
		return nil, err
	}
	// Best-effort last_used_at update — fire-and-forget. The tenant is now known
	// from the resolved row, so scope the connection so RLS admits the UPDATE.
	go func() {
		if err := withConnErr(context.Background(), r.pool, k.TenantID.String(), func(c2 *pgxpool.Conn) error {
			_, err := c2.Exec(context.Background(),
				`UPDATE agent_api_keys SET last_used_at = NOW() WHERE id = $1`, k.ID)
			return err
		}); err != nil {
			r.logger.Warn("agent_api_keys: last_used_at update failed",
				zap.String("key_id", k.ID.String()),
				zap.Error(err))
		}
	}()
	return k, nil
}
