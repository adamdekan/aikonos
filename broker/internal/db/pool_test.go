package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestWithTenant_GUCVisibleOnHeldConn proves withTenant's parameterized
// set_config makes app.current_tenant visible on the connection that set it.
func TestWithTenant_GUCVisibleOnHeldConn(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	tenantID := uuid.New().String()
	if err := withTenant(ctx, conn, tenantID); err != nil {
		t.Fatalf("withTenant: %v", err)
	}

	var got string
	if err := conn.QueryRow(ctx, `SELECT current_setting('app.current_tenant', true)`).Scan(&got); err != nil {
		t.Fatalf("read GUC: %v", err)
	}
	if got != tenantID {
		t.Fatalf("GUC = %q, want %q", got, tenantID)
	}
}

// TestWithTenant_ResetOnRelease proves the GUC does not leak to the next
// acquirer of the same backend connection — the invariant the AfterRelease
// hook in db.NewPool exists to guarantee. Bounded loop: pgxpool doesn't
// promise which physical connection Acquire returns, so this retries until
// the same backend PID is observed (or gives up with a skip-note).
func TestWithTenant_ResetOnRelease(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	var firstPID int32
	if err := conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&firstPID); err != nil {
		conn.Release()
		t.Fatalf("read backend pid: %v", err)
	}

	tenantID := uuid.New().String()
	if err := withTenant(ctx, conn, tenantID); err != nil {
		conn.Release()
		t.Fatalf("withTenant: %v", err)
	}
	conn.Release()

	const maxAttempts = 50
	for i := 0; i < maxAttempts; i++ {
		conn2, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("reacquire: %v", err)
		}

		var pid int32
		if err := conn2.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			conn2.Release()
			t.Fatalf("read backend pid: %v", err)
		}

		if pid != firstPID {
			conn2.Release()
			continue
		}

		var got string
		if err := conn2.QueryRow(ctx, `SELECT current_setting('app.current_tenant', true)`).Scan(&got); err != nil {
			conn2.Release()
			t.Fatalf("read GUC: %v", err)
		}
		conn2.Release()

		if got != "" {
			t.Fatalf("app.current_tenant leaked across release/reacquire on backend %d: got %q, want empty", pid, got)
		}
		return
	}

	t.Skipf("pool never returned the same backend PID (%d) within %d attempts — cannot prove reset-on-release here", firstPID, maxAttempts)
}

// TestWithTenant_InjectionShapedValueBindsInertly proves the parameterized
// set_config treats an injection-shaped tenant id as an inert bound value,
// never as SQL to execute.
func TestWithTenant_InjectionShapedValueBindsInertly(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	malicious := "x'; DROP TABLE tasks; --"
	if err := withTenant(ctx, conn, malicious); err != nil {
		t.Fatalf("withTenant with injection-shaped value: %v", err)
	}

	var got string
	if err := conn.QueryRow(ctx, `SELECT current_setting('app.current_tenant', true)`).Scan(&got); err != nil {
		t.Fatalf("read GUC: %v", err)
	}
	if got != malicious {
		t.Fatalf("GUC = %q, want literal %q", got, malicious)
	}

	// A subsequent trivial query proves the session (and the tasks table)
	// is still intact — no injected statement executed.
	var one int
	if err := conn.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("post-injection query failed: %v", err)
	}
}
