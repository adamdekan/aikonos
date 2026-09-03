// broker/internal/db/conn.go
// The withConn/withConnErr/withConnUnscoped seam is the single place every
// repo method acquires a pooled connection. It adds a fourth layer in front
// of three that stay untouched: the AfterRelease RESET hook (pool.go:26),
// the non-superuser aikonos_app role, and RLS itself — this seam only makes
// the existing Acquire→withTenant→Release convention impossible to forget,
// it does not change what any of those three enforce.
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// withConn acquires a connection from pool, sets app.current_tenant for RLS
// via withTenant, runs fn with the scoped connection, and releases it
// regardless of outcome. T is the query's result type, so both single-row
// and multi-row (slice) query shapes call through the same function with
// their return value preserved.
func withConn[T any](ctx context.Context, pool *pgxpool.Pool, tenant string, fn func(conn *pgxpool.Conn) (T, error)) (T, error) {
	var zero T
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return zero, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Release()

	if err := withTenant(ctx, conn, tenant); err != nil {
		return zero, fmt.Errorf("set tenant: %w", err)
	}

	return fn(conn)
}

// withConn2 is withConn for the handful of call sites that return two
// values plus error (e.g. a row and a "found" flag) instead of one.
func withConn2[A, B any](ctx context.Context, pool *pgxpool.Pool, tenant string, fn func(conn *pgxpool.Conn) (A, B, error)) (A, B, error) {
	var zeroA A
	var zeroB B
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return zeroA, zeroB, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Release()

	if err := withTenant(ctx, conn, tenant); err != nil {
		return zeroA, zeroB, fmt.Errorf("set tenant: %w", err)
	}

	return fn(conn)
}

// withConnErr is withConn for the common error-only call sites (INSERT/
// UPDATE/DELETE with nothing to return), so those call sites don't need a
// throwaway value type just to satisfy withConn's generic signature.
func withConnErr(ctx context.Context, pool *pgxpool.Pool, tenant string, fn func(conn *pgxpool.Conn) error) error {
	_, err := withConn(ctx, pool, tenant, func(conn *pgxpool.Conn) (struct{}, error) {
		return struct{}{}, fn(conn)
	})
	return err
}

// withConnUnscoped acquires a connection and runs fn with NO withTenant call.
// Reserved for the sanctioned cross-tenant SECURITY DEFINER sweeps — today
// that's agent_baselines.go's DistinctAgentsWithWindows/PruneWindowsBefore
// and agent_api_keys.go's Resolve, each of which reads through a SECURITY
// DEFINER SQL function precisely because no single tenant applies to the
// query. Every other call site must use withConn/withConnErr.
func withConnUnscoped[T any](ctx context.Context, pool *pgxpool.Pool, fn func(conn *pgxpool.Conn) (T, error)) (T, error) {
	var zero T
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return zero, fmt.Errorf("acquire conn: %w", err)
	}
	defer conn.Release()

	return fn(conn)
}
