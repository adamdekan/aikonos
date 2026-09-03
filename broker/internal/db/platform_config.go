// broker/internal/db/platform_config.go
// Repository for tenant-scoped runtime config (platform_config). Every query
// sets app.current_tenant for RLS. Satisfies config.configRepo.
package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// PlatformConfigRepo reads and writes platform_config rows under RLS.
type PlatformConfigRepo struct {
	pool   *pgxpool.Pool
	logger *zap.Logger
}

// NewPlatformConfigRepo builds a repo backed by pool.
func NewPlatformConfigRepo(pool *pgxpool.Pool, logger *zap.Logger) *PlatformConfigRepo {
	return &PlatformConfigRepo{pool: pool, logger: logger}
}

// Get returns the stored value for key in tenant's config space, or
// (_, false, nil) when no row exists.
func (r *PlatformConfigRepo) Get(ctx context.Context, tenant, key string) (string, bool, error) {
	return withConn2(ctx, r.pool, tenant, func(conn *pgxpool.Conn) (string, bool, error) {
		var val string
		err := conn.QueryRow(ctx,
			`SELECT value FROM platform_config WHERE key = $1`, key,
		).Scan(&val)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return "", false, nil
			}
			return "", false, fmt.Errorf("get platform_config %s: %w", key, err)
		}
		return val, true, nil
	})
}

// Set upserts a config row, recording the actor who made the change.
func (r *PlatformConfigRepo) Set(ctx context.Context, tenant, key, value, actor string) error {
	return withConnErr(ctx, r.pool, tenant, func(conn *pgxpool.Conn) error {
		_, err := conn.Exec(ctx, `
		INSERT INTO platform_config (tenant_id, key, value, updated_by)
		VALUES ($1::uuid, $2, $3, $4)
		ON CONFLICT (tenant_id, key) DO UPDATE
		  SET value = EXCLUDED.value,
		      updated_at = NOW(),
		      updated_by = EXCLUDED.updated_by`,
			tenant, key, value, actor,
		)
		if err != nil {
			return fmt.Errorf("upsert platform_config %s: %w", key, err)
		}
		return nil
	})
}

// List returns all config rows for tenant as a key→value map.
func (r *PlatformConfigRepo) List(ctx context.Context, tenant string) (map[string]string, error) {
	return withConn(ctx, r.pool, tenant, func(conn *pgxpool.Conn) (map[string]string, error) {
		rows, err := conn.Query(ctx, `SELECT key, value FROM platform_config`)
		if err != nil {
			return nil, fmt.Errorf("list platform_config: %w", err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				return nil, err
			}
			out[k] = v
		}
		return out, rows.Err()
	})
}
