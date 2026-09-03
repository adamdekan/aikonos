// broker/cmd/broker/singleton.go
// Enforces the one-broker-per-deployment invariant: a session-level Postgres
// advisory lock, acquired on a dedicated held-open connection, refuses a
// second instance from starting. See docs/00-aikonos-architecture.md §10.
package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// singletonLockKey is the fixed advisory-lock key used by every broker
// instance. Arbitrary but stable — changing it breaks the invariant across
// a rolling deploy.
const singletonLockKey int64 = 78412093

// singletonLocker abstracts pg_try_advisory_lock so the fail-fast decision
// in acquireSingletonLock is unit-testable without a live Postgres.
type singletonLocker interface {
	TryLock(ctx context.Context, key int64) (bool, error)
}

// acquireSingletonLock returns a non-nil error when the lock is held by
// another instance (TryLock returns false, nil) or when TryLock itself
// errors. A nil return means the lock was acquired.
func acquireSingletonLock(ctx context.Context, l singletonLocker, key int64) error {
	ok, err := l.TryLock(ctx, key)
	if err != nil {
		return fmt.Errorf("singleton lock acquisition failed: %w", err)
	}
	if !ok {
		return fmt.Errorf(
			"another broker instance holds the singleton lock — one broker per deployment; see docs/00-aikonos-architecture.md",
		)
	}
	return nil
}

// pgSingletonLocker backs singletonLocker with a dedicated pool connection
// held open for the process lifetime — a session-level advisory lock is
// released when its connection closes, so the connection must never be
// returned to the pool.
type pgSingletonLocker struct {
	conn *pgxpool.Conn
}

func (l *pgSingletonLocker) TryLock(ctx context.Context, key int64) (bool, error) {
	var locked bool
	if err := l.conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&locked); err != nil {
		return false, err
	}
	return locked, nil
}
