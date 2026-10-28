// broker/internal/db/revocation.go
// AG-06 revocation state: user and tenant blocks (kill_switches, migration 001)
// and the per-principal revocation epoch ("not before"): user_directory and
// agents.revoked_before (migration 054), and the agent status (AG-03,
// migration 056). Reads back the broker's cached
// checker (internal/revocation); writes back the admin RPCs.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// UserBlock is one active user block.
type UserBlock struct {
	Reason string
	By     string
	At     time.Time
}

// RevocationRepo persists blocks and revocation epochs.
type RevocationRepo struct {
	pool   *pgxpool.Pool
	logger *zap.Logger
}

func NewRevocationRepo(pool *pgxpool.Pool, logger *zap.Logger) *RevocationRepo {
	return &RevocationRepo{pool: pool, logger: logger}
}

// UserState reports whether subject (or the whole tenant) is blocked and the
// subject's revocation epoch (zero when never revoked). An empty subject reads
// the tenant block only.
func (r *RevocationRepo) UserState(ctx context.Context, tenantID, subject string) (blocked bool, notBefore time.Time, err error) {
	var nb *time.Time
	err = withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		return conn.QueryRow(ctx, `
		SELECT EXISTS (
		         SELECT 1 FROM kill_switches
		          WHERE tenant_id = $1 AND active
		            AND (scope = 'tenant' OR (scope = 'user' AND subject = $2))),
		       (SELECT revoked_before FROM user_directory WHERE tenant_id = $1 AND subject = $2)`,
			tenantID, subject,
		).Scan(&blocked, &nb)
	})
	if err != nil {
		return false, time.Time{}, fmt.Errorf("read user revocation: %w", err)
	}
	if nb != nil {
		notBefore = *nb
	}
	return blocked, notBefore, nil
}

// AgentState returns the agent's lifecycle status (AG-03) and revocation
// epoch (zero when never revoked). status is "" when the agent does not exist.
func (r *RevocationRepo) AgentState(ctx context.Context, tenantID, agentID string) (string, time.Time, error) {
	var nb *time.Time
	var st *string
	err := withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		return conn.QueryRow(ctx,
			`SELECT (SELECT revoked_before FROM agents WHERE id = $1 AND tenant_id = $2),
			        (SELECT status FROM agents WHERE id = $1 AND tenant_id = $2)`,
			agentID, tenantID,
		).Scan(&nb, &st)
	})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read agent revocation: %w", err)
	}
	var notBefore time.Time
	if nb != nil {
		notBefore = *nb
	}
	if st == nil {
		return "", notBefore, nil
	}
	return *st, notBefore, nil
}

// SetAgentStatus moves the agent from one lifecycle status to another (AG-03)
// and records the reason. Compare-and-set: ErrAgentNotFound when the agent
// does not exist in the tenant or is no longer in status from.
func (r *RevocationRepo) SetAgentStatus(ctx context.Context, tenantID, agentID, from, to, reason string) error {
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		tag, err := conn.Exec(ctx,
			`UPDATE agents SET status = $4, status_reason = $5, updated_at = NOW()
			  WHERE id = $2 AND tenant_id = $1 AND status = $3`,
			tenantID, agentID, from, to, reason,
		)
		if err != nil {
			return fmt.Errorf("set agent status: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrAgentNotFound
		}
		return nil
	})
}

// BlockUser activates a user block. It reports false when one is already
// active (the existing block, its reason and actor are kept).
func (r *RevocationRepo) BlockUser(ctx context.Context, tenantID, subject, reason, by string) (bool, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (bool, error) {
		tag, err := conn.Exec(ctx, `
		INSERT INTO kill_switches (tenant_id, scope, subject, reason, activated_by)
		VALUES ($1, 'user', $2, $3, $4)
		ON CONFLICT (tenant_id, scope, subject) WHERE active DO NOTHING`,
			tenantID, subject, reason, by,
		)
		if err != nil {
			return false, fmt.Errorf("block user: %w", err)
		}
		return tag.RowsAffected() == 1, nil
	})
}

// UnblockUser deactivates the user's active block. It reports false when the
// user was not blocked.
func (r *RevocationRepo) UnblockUser(ctx context.Context, tenantID, subject, by string) (bool, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (bool, error) {
		tag, err := conn.Exec(ctx, `
		UPDATE kill_switches SET active = FALSE, deactivated_at = NOW(), deactivated_by = $3
		 WHERE tenant_id = $1 AND scope = 'user' AND subject = $2 AND active`,
			tenantID, subject, by,
		)
		if err != nil {
			return false, fmt.Errorf("unblock user: %w", err)
		}
		return tag.RowsAffected() > 0, nil
	})
}

// UserBlocks returns the tenant's active user blocks keyed by subject.
func (r *RevocationRepo) UserBlocks(ctx context.Context, tenantID string) (map[string]UserBlock, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (map[string]UserBlock, error) {
		rows, err := conn.Query(ctx, `
		SELECT subject, COALESCE(reason, ''), activated_by, activated_at
		  FROM kill_switches WHERE tenant_id = $1 AND scope = 'user' AND active`, tenantID)
		if err != nil {
			return nil, fmt.Errorf("list user blocks: %w", err)
		}
		defer rows.Close()
		out := map[string]UserBlock{}
		for rows.Next() {
			var s string
			var b UserBlock
			if err := rows.Scan(&s, &b.Reason, &b.By, &b.At); err != nil {
				return nil, err
			}
			out[s] = b
		}
		return out, rows.Err()
	})
}

// RevokeUserSessions moves the user's revocation epoch to at. The user must
// be in the directory (has signed in); otherwise ErrNotFound.
func (r *RevocationRepo) RevokeUserSessions(ctx context.Context, tenantID, subject string, at time.Time) error {
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		tag, err := conn.Exec(ctx,
			`UPDATE user_directory SET revoked_before = $3 WHERE tenant_id = $1 AND subject = $2`,
			tenantID, subject, at,
		)
		if err != nil {
			return fmt.Errorf("revoke user sessions: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// RevokeAgentSessions moves the agent's revocation epoch to at, or returns
// ErrAgentNotFound.
func (r *RevocationRepo) RevokeAgentSessions(ctx context.Context, tenantID, agentID string, at time.Time) error {
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		tag, err := conn.Exec(ctx,
			`UPDATE agents SET revoked_before = $3 WHERE id = $2 AND tenant_id = $1`,
			tenantID, agentID, at,
		)
		if err != nil {
			return fmt.Errorf("revoke agent sessions: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrAgentNotFound
		}
		return nil
	})
}
