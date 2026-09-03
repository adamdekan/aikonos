package db

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool builds the production pool config: every connection gets a
// RESET app.current_tenant on release. withTenant sets the GUC on the
// held connection for RLS scoping; without a release-time reset, the
// next acquirer of the same pooled connection would silently inherit
// whatever tenant last ran on it. Fail-closed: if the RESET itself
// errors, AfterRelease returns false so pgxpool destroys the connection
// instead of returning it to the pool with unknown GUC state.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}

	cfg.AfterRelease = func(conn *pgx.Conn) bool {
		// context.Background is deliberate: pgx gives AfterRelease no
		// context, and this must run regardless of the caller's ctx state.
		if _, err := conn.Exec(context.Background(), "RESET app.current_tenant"); err != nil {
			return false
		}
		return true
	}

	return pgxpool.NewWithConfig(ctx, cfg)
}
