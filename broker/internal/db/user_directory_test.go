package db

import (
	"testing"
	"time"
)

// Tests cover struct/zero-value invariants and constructor behaviour; real CRUD
// round-trips require a live Postgres instance and are covered by the compose
// smoke (scripts/compose-verify.sh). This mirrors the mcp_connections_test.go
// pattern.

func TestUserDirectoryEntry_ZeroValue(t *testing.T) {
	e := UserDirectoryEntry{}
	if e.Subject != "" {
		t.Errorf("zero Subject should be empty string, got %q", e.Subject)
	}
	if e.Email != "" {
		t.Errorf("zero Email should be empty string, got %q", e.Email)
	}
	if e.Name != "" {
		t.Errorf("zero Name should be empty string, got %q", e.Name)
	}
	if !e.LastSeen.IsZero() {
		t.Errorf("zero LastSeen should be zero time, got %v", e.LastSeen)
	}
}

func TestNewUserDirectoryRepo_NonNil(t *testing.T) {
	repo := NewUserDirectoryRepo(nil, nil)
	if repo == nil {
		t.Fatal("NewUserDirectoryRepo returned nil")
	}
}

// TestUserDirectoryEntry_Fields verifies that a populated entry carries values.
func TestUserDirectoryEntry_Fields(t *testing.T) {
	now := time.Now()
	e := UserDirectoryEntry{
		Subject:  "cccccccc-oid",
		Email:    "adam@x.com",
		Name:     "Adam Dekan",
		LastSeen: now,
	}
	if e.Subject != "cccccccc-oid" {
		t.Errorf("Subject = %q", e.Subject)
	}
	if e.Name != "Adam Dekan" {
		t.Errorf("Name = %q", e.Name)
	}
	if e.Email != "adam@x.com" {
		t.Errorf("Email = %q", e.Email)
	}
	if !e.LastSeen.Equal(now) {
		t.Errorf("LastSeen drift")
	}
}
