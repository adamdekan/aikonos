// broker/internal/db/agent_public_keys.go
// Repository for external-agent public keys (agent_public_keys, migration 057,
// AG-05). Every query is tenant-scoped through app.current_tenant (RLS).
package db

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// StoredPublicKey is a row from agent_public_keys.
type StoredPublicKey struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	AgentID    uuid.UUID
	Kid        string
	Alg        string // ES256 | EdDSA
	PublicKey  string // SPKI PEM
	Status     string // active | retired
	CreatedBy  string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastUsedAt *time.Time
	RetiredAt  *time.Time
}

var (
	// ErrPublicKeyNotFound: no such key (Get), or no active key to retire.
	ErrPublicKeyNotFound = errors.New("agent_public_key not found")
	// ErrPublicKeyExists: the agent already has a key (active or retired) with this kid.
	ErrPublicKeyExists = errors.New("agent_public_key kid already registered")
)

const publicKeyCols = `id, tenant_id, agent_id, kid, alg, public_key, status,
	created_by, created_at, expires_at, last_used_at, retired_at`

func scanPublicKey(row pgx.Row) (*StoredPublicKey, error) {
	k := &StoredPublicKey{}
	err := row.Scan(&k.ID, &k.TenantID, &k.AgentID, &k.Kid, &k.Alg, &k.PublicKey, &k.Status,
		&k.CreatedBy, &k.CreatedAt, &k.ExpiresAt, &k.LastUsedAt, &k.RetiredAt)
	return k, err
}

// AgentPublicKeyRepo is backed by Postgres.
type AgentPublicKeyRepo struct {
	pool   *pgxpool.Pool
	logger *zap.Logger
}

func NewAgentPublicKeyRepo(pool *pgxpool.Pool, logger *zap.Logger) *AgentPublicKeyRepo {
	return &AgentPublicKeyRepo{pool: pool, logger: logger}
}

func (r *AgentPublicKeyRepo) Create(ctx context.Context, k *StoredPublicKey) error {
	return withConnErr(ctx, r.pool, k.TenantID.String(), func(conn *pgxpool.Conn) error {
		_, err := conn.Exec(ctx, `
		INSERT INTO agent_public_keys
			(id, tenant_id, agent_id, kid, alg, public_key, status, created_by, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'active', $7, $8, $9)`,
			k.ID, k.TenantID, k.AgentID, k.Kid, k.Alg, k.PublicKey, k.CreatedBy, k.CreatedAt, k.ExpiresAt)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrPublicKeyExists
		}
		return err
	})
}

// ListByAgent returns the agent's active keys, newest first. Retired keys are
// kept for audit but not listed, like revoked tk_ keys.
func (r *AgentPublicKeyRepo) ListByAgent(ctx context.Context, tenantID, agentID string) ([]*StoredPublicKey, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) ([]*StoredPublicKey, error) {
		rows, err := conn.Query(ctx, `SELECT `+publicKeyCols+` FROM agent_public_keys
		WHERE agent_id = $1 AND status = 'active' ORDER BY created_at DESC`, agentID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []*StoredPublicKey
		for rows.Next() {
			k, err := scanPublicKey(rows)
			if err != nil {
				return nil, err
			}
			out = append(out, k)
		}
		return out, rows.Err()
	})
}

// Retire marks an active key retired. ErrPublicKeyNotFound if none matched.
func (r *AgentPublicKeyRepo) Retire(ctx context.Context, tenantID, agentID, keyID string) error {
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		tag, err := conn.Exec(ctx, `UPDATE agent_public_keys SET status = 'retired', retired_at = NOW()
		WHERE id = $1 AND agent_id = $2 AND status = 'active'`, keyID, agentID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrPublicKeyNotFound
		}
		return nil
	})
}

// Get returns the agent's key with this kid whatever its status or expiry, so
// the caller can tell an unknown kid from a retired or expired one.
func (r *AgentPublicKeyRepo) Get(ctx context.Context, tenantID, agentID, kid string) (*StoredPublicKey, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (*StoredPublicKey, error) {
		k, err := scanPublicKey(conn.QueryRow(ctx, `SELECT `+publicKeyCols+` FROM agent_public_keys
		WHERE agent_id = $1 AND kid = $2`, agentID, kid))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrPublicKeyNotFound
		}
		return k, err
	})
}

// TouchLastUsed records a successful assertion (best effort, fire-and-forget).
func (r *AgentPublicKeyRepo) TouchLastUsed(tenantID, keyID string) {
	touchLastUsed(r.pool, r.logger, tenantID, `UPDATE agent_public_keys SET last_used_at = NOW() WHERE id = $1`, keyID)
}
