// broker/internal/db/user_directory.go
// Passive display-name cache for authenticated OIDC subjects. Every verified
// north caller is upserted here; the broker reads it only for human-readable
// display in admin views — never for authz decisions.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// UserDirectoryEntry is one cached caller identity.
type UserDirectoryEntry struct {
	Subject  string
	Email    string
	Name     string
	LastSeen time.Time
}

// UserDirectoryRepo persists OIDC subject→display-name mappings.
type UserDirectoryRepo struct {
	pool   *pgxpool.Pool
	logger *zap.Logger
}

// NewUserDirectoryRepo constructs the repo. Nil pool is accepted (queries will
// fail at call time, not at construction — mirrors the other repos in this pkg).
func NewUserDirectoryRepo(pool *pgxpool.Pool, logger *zap.Logger) *UserDirectoryRepo {
	return &UserDirectoryRepo{pool: pool, logger: logger}
}

// Upsert inserts or updates the directory entry for (tenantID, subject).
// Empty email/name values do not overwrite an existing non-empty value.
func (r *UserDirectoryRepo) Upsert(ctx context.Context, tenantID, subject, email, name string) error {
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		_, err := conn.Exec(ctx, `
		INSERT INTO user_directory (tenant_id, subject, email, name, last_seen)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (tenant_id, subject) DO UPDATE
		SET
			email     = CASE WHEN EXCLUDED.email <> '' THEN EXCLUDED.email ELSE user_directory.email END,
			name      = CASE WHEN EXCLUDED.name  <> '' THEN EXCLUDED.name  ELSE user_directory.name  END,
			last_seen = NOW()`,
			tenantID, subject, email, name,
		)
		if err != nil {
			return fmt.Errorf("upsert user_directory: %w", err)
		}
		return nil
	})
}

// MemberEntry is one roster row (A3): a directory entry plus its provisioning
// origin. ProvisionedAt is nil for manually-created (non-JIT) users.
type MemberEntry struct {
	Subject       string
	Email         string
	Name          string
	LastSeen      time.Time
	ProvisionedAt *time.Time
}

// ListMembers returns every directory row for the tenant with provisioning
// origin, ordered by email then subject for a stable roster.
func (r *UserDirectoryRepo) ListMembers(ctx context.Context, tenantID string) ([]MemberEntry, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) ([]MemberEntry, error) {
		rows, err := conn.Query(ctx, `
		SELECT subject, email, name, last_seen, provisioned_at
		  FROM user_directory
		 ORDER BY NULLIF(email, '') NULLS LAST, subject`)
		if err != nil {
			return nil, fmt.Errorf("list members: %w", err)
		}
		defer rows.Close()
		var out []MemberEntry
		for rows.Next() {
			var e MemberEntry
			if err := rows.Scan(&e.Subject, &e.Email, &e.Name, &e.LastSeen, &e.ProvisionedAt); err != nil {
				return nil, err
			}
			out = append(out, e)
		}
		return out, rows.Err()
	})
}

// ListByTenant returns all directory entries for tenantID, keyed by subject.
func (r *UserDirectoryRepo) ListByTenant(ctx context.Context, tenantID string) (map[string]UserDirectoryEntry, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (map[string]UserDirectoryEntry, error) {
		rows, err := conn.Query(ctx, `SELECT subject, email, name, last_seen FROM user_directory`)
		if err != nil {
			return nil, fmt.Errorf("list user_directory: %w", err)
		}
		defer rows.Close()
		out := make(map[string]UserDirectoryEntry)
		for rows.Next() {
			var e UserDirectoryEntry
			if err := rows.Scan(&e.Subject, &e.Email, &e.Name, &e.LastSeen); err != nil {
				return nil, err
			}
			out[e.Subject] = e
		}
		return out, rows.Err()
	})
}
