package db

import (
	"testing"
)

// These tests exercise SkillOverlayRepo without a real Postgres instance.
// The db package has no testcontainers/shared-pool harness. Real CRUD
// round-trips are verified in-cluster. This file covers struct/zero-value
// invariants and non-nil constructor only (mirrors mcp_connections_test.go).

func TestSkillOverlayDefaults(t *testing.T) {
	s := &SkillOverlay{}
	if s.ToolID != "" {
		t.Errorf("zero ToolID should be empty string, got %q", s.ToolID)
	}
	if s.ExecutorKind != "" {
		t.Errorf("zero ExecutorKind should be empty string, got %q", s.ExecutorKind)
	}
	if s.CapabilityScope != "" {
		t.Errorf("zero CapabilityScope should be empty string, got %q", s.CapabilityScope)
	}
	if s.EffectClass != "" {
		t.Errorf("zero EffectClass should be empty string, got %q", s.EffectClass)
	}
	if s.DisplayName != "" {
		t.Errorf("zero DisplayName should be empty string, got %q", s.DisplayName)
	}
	if s.Description != "" {
		t.Errorf("zero Description should be empty string, got %q", s.Description)
	}
	// Enabled zero-value is false (Go bool default), but the DB column defaults
	// to TRUE. The Go struct intentionally carries no default — the DB default
	// is authoritative. Callers reading from DB always get the DB value via Scan;
	// a zero-value Go struct is never written unless the caller explicitly sets it.
	_ = s.Enabled
	if s.CreatedBy != "" {
		t.Errorf("zero CreatedBy should be empty string, got %q", s.CreatedBy)
	}
}

// TestSkillOverlayRepoNonNilConstruct verifies the repo constructor returns a
// non-nil value with a nil pool (nil panics only at query time; real
// CRUD round-trips are in-cluster).
func TestSkillOverlayRepoNonNilConstruct(t *testing.T) {
	repo := NewSkillOverlayRepo(nil, nil)
	if repo == nil {
		t.Fatal("NewSkillOverlayRepo returned nil")
	}
}
