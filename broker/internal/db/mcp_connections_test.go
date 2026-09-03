package db

import (
	"testing"
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
