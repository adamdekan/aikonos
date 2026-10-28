package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// These tests exercise McpConnectionRepo without a real Postgres instance.
// The db package has no testcontainers/shared-pool harness (see scheduled_runs_test.go
// which tests only pure-logic helpers). Real CRUD round-trips are verified
// in-cluster via scripts/verify-mcp.sh. This file covers the struct/zero-value
// invariants that are independent of a live DB.

func TestMcpConnectionDefaults(t *testing.T) {
	c := &McpConnection{}
	if c.Transport != "" {
		t.Errorf("zero Transport should be empty string, got %q", c.Transport)
	}
	if c.AuthType != "" {
		t.Errorf("zero AuthType should be empty string, got %q", c.AuthType)
	}
}

// TestMcpConnectionRepoNonNilConstruct verifies the repo constructor returns a
// non-nil value with a nil pool (nil access only occurs at query time; real
// CRUD round-trips are in-cluster via scripts/verify-mcp.sh).
func TestMcpConnectionRepoNonNilConstruct(t *testing.T) {
	repo := NewMcpConnectionRepo(nil, nil)
	if repo == nil {
		t.Fatal("NewMcpConnectionRepo returned nil")
	}
}

// TestMcpConnectionRepo_OAuthRoundTrip runs against a migrated DB (migration
// 061 applied); it skips without TEST_DATABASE_URL.
func TestMcpConnectionRepo_OAuthRoundTrip(t *testing.T) {
	pool := openTestPool(t)
	repo := NewMcpConnectionRepo(pool, zap.NewNop())
	ctx := context.Background()
	c := &McpConnection{ID: uuid.New(), TenantID: uuid.New(), Name: "oauth-mcp", URL: "https://mcp.example.com/mcp",
		Transport: "streamable_http", AuthType: "oauth", CreatedBy: "admin",
		OAuthClientID: "client-1", OAuthScopes: []string{"a", "b"}, OAuthIssuer: "https://as.example.com",
		OAuthAuthURL: "https://as.example.com/authorize", OAuthTokenURL: "https://as.example.com/token",
		OAuthResource: "https://mcp.example.com/mcp"}
	if err := repo.Add(ctx, c); err != nil {
		t.Fatalf("add: %v", err)
	}
	t.Cleanup(func() { _ = repo.Delete(ctx, c.TenantID.String(), c.ID.String()) })
	got, err := repo.Get(ctx, c.TenantID.String(), c.ID.String())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.OAuthClientID != "client-1" || len(got.OAuthScopes) != 2 || got.OAuthTokenURL != c.OAuthTokenURL || got.OAuthResource != c.OAuthResource {
		t.Errorf("round trip = %+v", got)
	}
	got.AuthType, got.OAuthClientID, got.OAuthScopes = "none", "", nil
	if err := repo.Update(ctx, c.TenantID.String(), got); err != nil {
		t.Fatalf("update: %v", err)
	}
	if again, _ := repo.Get(ctx, c.TenantID.String(), c.ID.String()); again.AuthType != "none" || again.OAuthClientID != "" || len(again.OAuthScopes) != 0 {
		t.Errorf("after update = %+v", again)
	}
}
