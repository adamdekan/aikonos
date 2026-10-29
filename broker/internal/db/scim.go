// broker/internal/db/scim.go
// AG-16 SCIM 2.0 provisioning state (migration 060): per-tenant bearer tokens,
// provisioned users and groups. Every query is tenant-scoped (RLS).
package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// ErrScimConflict is returned when a userName, externalId or group name is
// already taken in the tenant (SCIM 409 uniqueness).
var ErrScimConflict = errors.New("scim: uniqueness conflict")

// ScimToken is one SCIM bearer token row (never the raw token).
type ScimToken struct {
	ID         uuid.UUID
	Prefix     string
	CreatedBy  string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	ExpiresAt  time.Time // F13: refused from this time on (migration 064)
}

// ScimEmail is one entry of a SCIM user's emails attribute.
type ScimEmail struct {
	Value   string `json:"value"`
	Type    string `json:"type,omitempty"`
	Primary bool   `json:"primary,omitempty"`
}

// ScimUser is one provisioned user. ExternalID is the OIDC subject.
type ScimUser struct {
	ID          uuid.UUID
	UserName    string
	ExternalID  string
	DisplayName string
	Emails      []ScimEmail
	Active      bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ScimGroup is one provisioned group. FGAGroup is fixed at creation.
type ScimGroup struct {
	ID          uuid.UUID
	DisplayName string
	ExternalID  string
	FGAGroup    string
	Members     []uuid.UUID // scim_users ids
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ScimRepo persists SCIM tokens, users and groups.
type ScimRepo struct {
	pool   *pgxpool.Pool
	logger *zap.Logger
}

func NewScimRepo(pool *pgxpool.Pool, logger *zap.Logger) *ScimRepo {
	return &ScimRepo{pool: pool, logger: logger}
}

func scimConflict(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return ErrScimConflict
	}
	return err
}

// ── tokens ───────────────────────────────────────────────────────────────────

// CreateToken stores a token hash; when replaces is set, that token is revoked
// in the same transaction (rotation). ErrNotFound when replaces is not an
// active token of the tenant.
func (r *ScimRepo) CreateToken(ctx context.Context, tenantID, hash, prefix, by string, replaces *uuid.UUID) (ScimToken, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (ScimToken, error) {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return ScimToken{}, err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if replaces != nil {
			tag, err := tx.Exec(ctx, `UPDATE scim_tokens SET revoked_at = NOW(), revoked_by = $3
			  WHERE tenant_id = $1 AND id = $2 AND revoked_at IS NULL`, tenantID, *replaces, by)
			if err != nil {
				return ScimToken{}, fmt.Errorf("revoke replaced scim token: %w", err)
			}
			if tag.RowsAffected() == 0 {
				return ScimToken{}, ErrNotFound
			}
		}
		t := ScimToken{Prefix: prefix, CreatedBy: by}
		if err := tx.QueryRow(ctx, `
		INSERT INTO scim_tokens (tenant_id, token_hash, token_prefix, created_by)
		VALUES ($1, $2, $3, $4) RETURNING id, created_at, expires_at`,
			tenantID, hash, prefix, by).Scan(&t.ID, &t.CreatedAt, &t.ExpiresAt); err != nil {
			return ScimToken{}, fmt.Errorf("insert scim token: %w", err)
		}
		return t, tx.Commit(ctx)
	})
}

// ListTokens returns the tenant's active tokens, newest first.
func (r *ScimRepo) ListTokens(ctx context.Context, tenantID string) ([]ScimToken, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) ([]ScimToken, error) {
		rows, err := conn.Query(ctx, `
		SELECT id, token_prefix, created_by, created_at, last_used_at, expires_at FROM scim_tokens
		 WHERE tenant_id = $1 AND revoked_at IS NULL ORDER BY created_at DESC`, tenantID)
		if err != nil {
			return nil, fmt.Errorf("list scim tokens: %w", err)
		}
		defer rows.Close()
		var out []ScimToken
		for rows.Next() {
			var t ScimToken
			if err := rows.Scan(&t.ID, &t.Prefix, &t.CreatedBy, &t.CreatedAt, &t.LastUsedAt, &t.ExpiresAt); err != nil {
				return nil, err
			}
			out = append(out, t)
		}
		return out, rows.Err()
	})
}

// RevokeToken revokes an active token; ErrNotFound when there is none.
func (r *ScimRepo) RevokeToken(ctx context.Context, tenantID string, id uuid.UUID, by string) error {
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		tag, err := conn.Exec(ctx, `UPDATE scim_tokens SET revoked_at = NOW(), revoked_by = $3
		  WHERE tenant_id = $1 AND id = $2 AND revoked_at IS NULL`, tenantID, id, by)
		if err != nil {
			return fmt.Errorf("revoke scim token: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ErrScimTokenExpired is returned by ResolveToken for an unrevoked token past
// its expires_at (migration 064): the admin rotates it.
var ErrScimTokenExpired = errors.New("scim: token expired")

// ResolveToken returns the id of the active token with this hash and stamps
// its last use; ErrNotFound when none, ErrScimTokenExpired (with the id, use
// not stamped) when it has expired.
func (r *ScimRepo) ResolveToken(ctx context.Context, tenantID, hash string) (uuid.UUID, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (uuid.UUID, error) {
		var (
			id   uuid.UUID
			live bool
		)
		err := conn.QueryRow(ctx, `UPDATE scim_tokens
		  SET last_used_at = CASE WHEN expires_at > NOW() THEN NOW() ELSE last_used_at END
		  WHERE tenant_id = $1 AND token_hash = $2 AND revoked_at IS NULL
		  RETURNING id, expires_at > NOW()`,
			tenantID, hash).Scan(&id, &live)
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, ErrNotFound
		}
		if err == nil && !live {
			return id, ErrScimTokenExpired
		}
		return id, err
	})
}

// ── users ────────────────────────────────────────────────────────────────────

const scimUserCols = `id, user_name, external_id, display_name, emails, active, created_at, updated_at`

func scanScimUser(row pgx.Row) (ScimUser, error) {
	var u ScimUser
	var emails []byte
	if err := row.Scan(&u.ID, &u.UserName, &u.ExternalID, &u.DisplayName, &emails, &u.Active, &u.CreatedAt, &u.UpdatedAt); err != nil {
		return ScimUser{}, err
	}
	if err := json.Unmarshal(emails, &u.Emails); err != nil {
		return ScimUser{}, fmt.Errorf("decode scim emails: %w", err)
	}
	return u, nil
}

// ListUsers returns the tenant's users ordered by creation.
// ponytail: whole-tenant read, filtered and paged in Go; push the filter into
// SQL if a tenant ever provisions tens of thousands of users.
func (r *ScimRepo) ListUsers(ctx context.Context, tenantID string) ([]ScimUser, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) ([]ScimUser, error) {
		rows, err := conn.Query(ctx, `SELECT `+scimUserCols+` FROM scim_users WHERE tenant_id = $1 ORDER BY created_at, id`, tenantID)
		if err != nil {
			return nil, fmt.Errorf("list scim users: %w", err)
		}
		defer rows.Close()
		var out []ScimUser
		for rows.Next() {
			u, err := scanScimUser(rows)
			if err != nil {
				return nil, err
			}
			out = append(out, u)
		}
		return out, rows.Err()
	})
}

// GetUser returns one user; ErrNotFound when absent.
func (r *ScimRepo) GetUser(ctx context.Context, tenantID string, id uuid.UUID) (ScimUser, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (ScimUser, error) {
		u, err := scanScimUser(conn.QueryRow(ctx, `SELECT `+scimUserCols+` FROM scim_users WHERE tenant_id = $1 AND id = $2`, tenantID, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ScimUser{}, ErrNotFound
		}
		return u, err
	})
}

// CreateUser inserts u and fills its id and timestamps.
func (r *ScimRepo) CreateUser(ctx context.Context, tenantID string, u *ScimUser) error {
	emails, _ := json.Marshal(nonNilEmails(u.Emails))
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		err := conn.QueryRow(ctx, `
		INSERT INTO scim_users (tenant_id, user_name, external_id, display_name, emails, active)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, created_at, updated_at`,
			tenantID, u.UserName, u.ExternalID, u.DisplayName, emails, u.Active,
		).Scan(&u.ID, &u.CreatedAt, &u.UpdatedAt)
		return scimConflict(err)
	})
}

// UpdateUser replaces the mutable attributes of u (by id).
func (r *ScimRepo) UpdateUser(ctx context.Context, tenantID string, u *ScimUser) error {
	emails, _ := json.Marshal(nonNilEmails(u.Emails))
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		err := conn.QueryRow(ctx, `
		UPDATE scim_users SET user_name = $3, external_id = $4, display_name = $5, emails = $6,
		       active = $7, updated_at = NOW()
		 WHERE tenant_id = $1 AND id = $2 RETURNING updated_at`,
			tenantID, u.ID, u.UserName, u.ExternalID, u.DisplayName, emails, u.Active,
		).Scan(&u.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return scimConflict(err)
	})
}

// DeleteUser removes the user and its group memberships.
func (r *ScimRepo) DeleteUser(ctx context.Context, tenantID string, id uuid.UUID) error {
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `UPDATE scim_groups SET members = array_remove(members, $2), updated_at = NOW()
		  WHERE tenant_id = $1 AND $2 = ANY(members)`, tenantID, id); err != nil {
			return fmt.Errorf("drop scim memberships: %w", err)
		}
		tag, err := tx.Exec(ctx, `DELETE FROM scim_users WHERE tenant_id = $1 AND id = $2`, tenantID, id)
		if err != nil {
			return fmt.Errorf("delete scim user: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return tx.Commit(ctx)
	})
}

func nonNilEmails(e []ScimEmail) []ScimEmail {
	if e == nil {
		return []ScimEmail{}
	}
	return e
}

// ── groups ───────────────────────────────────────────────────────────────────

const scimGroupCols = `id, display_name, external_id, fga_group, members, created_at, updated_at`

func scanScimGroup(row pgx.Row) (ScimGroup, error) {
	var g ScimGroup
	err := row.Scan(&g.ID, &g.DisplayName, &g.ExternalID, &g.FGAGroup, &g.Members, &g.CreatedAt, &g.UpdatedAt)
	return g, err
}

// ListGroups returns the tenant's groups ordered by creation.
func (r *ScimRepo) ListGroups(ctx context.Context, tenantID string) ([]ScimGroup, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) ([]ScimGroup, error) {
		rows, err := conn.Query(ctx, `SELECT `+scimGroupCols+` FROM scim_groups WHERE tenant_id = $1 ORDER BY created_at, id`, tenantID)
		if err != nil {
			return nil, fmt.Errorf("list scim groups: %w", err)
		}
		defer rows.Close()
		var out []ScimGroup
		for rows.Next() {
			g, err := scanScimGroup(rows)
			if err != nil {
				return nil, err
			}
			out = append(out, g)
		}
		return out, rows.Err()
	})
}

// GetGroup returns one group; ErrNotFound when absent.
func (r *ScimRepo) GetGroup(ctx context.Context, tenantID string, id uuid.UUID) (ScimGroup, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (ScimGroup, error) {
		g, err := scanScimGroup(conn.QueryRow(ctx, `SELECT `+scimGroupCols+` FROM scim_groups WHERE tenant_id = $1 AND id = $2`, tenantID, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ScimGroup{}, ErrNotFound
		}
		return g, err
	})
}

// CreateGroup inserts g and fills its id and timestamps.
func (r *ScimRepo) CreateGroup(ctx context.Context, tenantID string, g *ScimGroup) error {
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		err := conn.QueryRow(ctx, `
		INSERT INTO scim_groups (tenant_id, display_name, external_id, fga_group, members)
		VALUES ($1, $2, $3, $4, $5) RETURNING id, created_at, updated_at`,
			tenantID, g.DisplayName, g.ExternalID, g.FGAGroup, nonNilIDs(g.Members),
		).Scan(&g.ID, &g.CreatedAt, &g.UpdatedAt)
		return scimConflict(err)
	})
}

// UpdateGroup replaces displayName, externalId and members of g (by id).
func (r *ScimRepo) UpdateGroup(ctx context.Context, tenantID string, g *ScimGroup) error {
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		err := conn.QueryRow(ctx, `
		UPDATE scim_groups SET display_name = $3, external_id = $4, members = $5, updated_at = NOW()
		 WHERE tenant_id = $1 AND id = $2 RETURNING updated_at`,
			tenantID, g.ID, g.DisplayName, g.ExternalID, nonNilIDs(g.Members),
		).Scan(&g.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
}

// DeleteGroup removes the group.
func (r *ScimRepo) DeleteGroup(ctx context.Context, tenantID string, id uuid.UUID) error {
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		tag, err := conn.Exec(ctx, `DELETE FROM scim_groups WHERE tenant_id = $1 AND id = $2`, tenantID, id)
		if err != nil {
			return fmt.Errorf("delete scim group: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func nonNilIDs(ids []uuid.UUID) []uuid.UUID {
	if ids == nil {
		return []uuid.UUID{}
	}
	return ids
}
