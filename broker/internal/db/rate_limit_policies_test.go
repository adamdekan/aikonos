package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// TestRateLimitPolicyRepo_ListAll_SpansTenants (F21): the limiter reload reads
// every tenant's policies, so one tenant's admin change keeps the others'.
func TestRateLimitPolicyRepo_ListAll_SpansTenants(t *testing.T) {
	pool := openTestPool(t)
	repo := NewRateLimitPolicyRepo(pool, zap.NewNop())
	ctx := context.Background()
	a, b := uuid.NewString(), uuid.NewString()
	rpm := int32(5)
	for _, tenant := range []string{a, b} {
		if _, err := repo.Upsert(ctx, tenant, RateLimitPolicy{Provider: "p2b-test", RPMLimit: &rpm, CreatedBy: "admin@example.com"}); err != nil {
			t.Fatalf("Upsert(%s): %v", tenant, err)
		}
	}
	rows, err := repo.ListAll(ctx)
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.TenantID.String()] = true
	}
	if !seen[a] || !seen[b] {
		t.Fatalf("ListAll must hold both tenants' policies, got a=%v b=%v", seen[a], seen[b])
	}
}
