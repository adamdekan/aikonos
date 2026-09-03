package db

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// ── RLS enforcement under the restricted app role ────────────────────────────
//
// These tests prove the fix in : RLS actually isolates
// tenants only when the connection role is NOT a superuser/owner (which bypass
// RLS unconditionally). They connect via TEST_DATABASE_URL_APP — a DSN for the
// non-superuser `aikonos_app` role — and skip when it is unset, so `go test`
// stays green without a live stack.
//
// Run against a migrated + role-provisioned stack:
//
//	TEST_DATABASE_URL_APP=postgres://aikonos_app:dev-app-password-change-me@localhost:5432/aikonos \
//	  go test ./broker/internal/db/... -run TestRLS -v
//
// Contrast: the same assertions under the superuser owner role (TEST_DATABASE_URL,
// role `aikonos`) would FAIL — Get would return the foreign-tenant row — which is
// exactly the cross-tenant read this fix closes.

func openAppTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL_APP")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL_APP not set — skipping app-role RLS tests")
	}
	pool, err := NewPool(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open app test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestRLS_TaskGet_CrossTenantDenied proves TaskRepo.Get — which scopes by
// task_id alone and relies on RLS for tenant isolation — does NOT leak a task
// across tenants when the connection role is RLS-enforced. This is the
// regression guard for the finding: Get carries no explicit tenant_id predicate,
// so under a superuser role it returned any tenant's task.
func TestRLS_TaskGet_CrossTenantDenied(t *testing.T) {
	pool := openAppTestPool(t)
	repo := NewTaskRepo(pool, zap.NewNop())
	ctx := context.Background()

	tenantA := uuid.New()
	tenantB := uuid.New()
	taskID := uuid.New()

	if err := repo.Create(ctx, &Task{
		TaskID:      taskID,
		TenantID:    tenantA,
		OwnerUserID: "alice@example.com",
		Prompt:      "tenant A secret prompt",
		CostBudget:  1000,
	}); err != nil {
		t.Fatalf("Create under tenant A: %v", err)
	}

	// Tenant B must NOT see tenant A's task, even naming its exact id — RLS
	// filters it because Get's query has no tenant predicate.
	if _, err := repo.Get(ctx, tenantB.String(), taskID.String()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant Get: got err=%v, want errors.Is(err, ErrNotFound) — RLS is not isolating (superuser role?)", err)
	}

	// Sanity: the owning tenant still reads it, so the miss above is isolation,
	// not an always-empty query.
	got, err := repo.Get(ctx, tenantA.String(), taskID.String())
	if err != nil {
		t.Fatalf("same-tenant Get: %v", err)
	}
	if got.TaskID != taskID {
		t.Fatalf("same-tenant Get: got task %s, want %s", got.TaskID, taskID)
	}
}

// TestRLS_ResolveAgentApiKey_TenantAgnostic proves AgentApiKeyRepo.Resolve finds
// a minted key by its hash under the RLS-enforced app role WITHOUT any tenant set
// on the connection. This is the regression guard for the finding: a direct
// SELECT on agent_api_keys returned zero rows because the tenant_isolation policy
// evaluated false with app.current_tenant unset, so every external-agent key
// authenticated as "revoked or not found". Resolve now goes through the
// resolve_agent_api_key SECURITY DEFINER function (migration 034), which bypasses
// RLS for exactly this tenant-agnostic hash lookup.
func TestRLS_ResolveAgentApiKey_TenantAgnostic(t *testing.T) {
	pool := openAppTestPool(t)
	ctx := context.Background()

	tenant := uuid.New()
	agentID := uuid.New()

	agents := NewAgentRepo(pool, zap.NewNop())
	if err := agents.Create(ctx, &Agent{
		ID:           agentID,
		TenantID:     tenant,
		Name:         "rls-resolve-agent",
		LLMModel:     "claude-opus-4-8",
		ApprovalMode: "auto",
		CreatedBy:    "test@example.com",
	}); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	keys := NewAgentApiKeyRepo(pool, zap.NewNop())
	const keyHash = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	if err := keys.Create(ctx, &StoredApiKey{
		ID:        uuid.New(),
		TenantID:  tenant,
		AgentID:   agentID,
		KeyHash:   keyHash,
		KeyPrefix: "tk_test0",
		Label:     "rls-resolve",
		CreatedBy: "test@example.com",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create api key: %v", err)
	}

	// Resolve sets no tenant on its connection — under enforced RLS this only
	// succeeds via the definer carve-out.
	got, err := keys.Resolve(ctx, keyHash)
	if err != nil {
		t.Fatalf("Resolve under app role: %v (RLS carve-out missing? migration 034 not applied?)", err)
	}
	if got.TenantID != tenant {
		t.Fatalf("Resolve: got tenant %s, want %s", got.TenantID, tenant)
	}
	if got.AgentID != agentID {
		t.Fatalf("Resolve: got agent %s, want %s", got.AgentID, agentID)
	}

	// A hash that was never minted still resolves to not-found, not a leak.
	if _, err := keys.Resolve(ctx, "0000000000000000000000000000000000000000000000000000000000000000"); !errors.Is(err, ErrApiKeyNotFound) {
		t.Fatalf("unknown-hash Resolve: got err=%v, want ErrApiKeyNotFound", err)
	}
}
