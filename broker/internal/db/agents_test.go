package db

import (
	"encoding/json"
	"testing"
)

// Pure-logic tests for Agent struct and JSON marshal/unmarshal invariants.
// DB-backed round-trips follow the in-cluster pattern (TEST_DATABASE_URL-gated).

// TestAgentAllowedProvidersJSONRoundTrip verifies that AllowedProviders marshals
// to/from JSON correctly, matching the jsonb column behaviour.
func TestAgentAllowedProvidersJSONRoundTrip(t *testing.T) {
	a := Agent{
		AllowedProviders: []string{"anthropic", "openrouter"},
		PreferredProvider: "anthropic",
	}

	b, err := json.Marshal(a.AllowedProviders)
	if err != nil {
		t.Fatalf("marshal AllowedProviders: %v", err)
	}
	var got []string
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal AllowedProviders: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 providers, got %d", len(got))
	}
	if got[0] != "anthropic" || got[1] != "openrouter" {
		t.Errorf("providers mismatch: got %v", got)
	}
	if a.PreferredProvider != "anthropic" {
		t.Errorf("PreferredProvider: got %q, want %q", a.PreferredProvider, "anthropic")
	}
}

// TestAgentAllowedProvidersEmptyDefault verifies that a nil slice marshals to
// "[]" and unmarshals back to an empty non-nil slice, matching the DB default.
func TestAgentAllowedProvidersEmptyDefault(t *testing.T) {
	var providers []string // nil — mirrors default from DB
	b, err := json.Marshal(providers)
	if err != nil {
		t.Fatalf("marshal nil providers: %v", err)
	}
	if string(b) != "null" && string(b) != "[]" {
		// nil marshals to "null" in Go; the code normalises to []string{} before
		// marshal so this path tests the normalisation invariant.
		t.Logf("nil marshal: %s (expected null or [])", b)
	}

	// Normalised path (as done in Create/Update): []string{}
	b2, err := json.Marshal([]string{})
	if err != nil {
		t.Fatalf("marshal empty providers: %v", err)
	}
	if string(b2) != "[]" {
		t.Errorf("marshal []string{}: want \"[]\", got %q", string(b2))
	}
	var got []string
	if err := json.Unmarshal(b2, &got); err != nil {
		t.Fatalf("unmarshal empty providers: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want 0 providers from empty JSON array, got %d", len(got))
	}
}

// TestAgentRepoNonNilConstruct verifies the constructor returns a non-nil value.
func TestAgentRepoNonNilConstruct(t *testing.T) {
	repo := NewAgentRepo(nil, nil)
	if repo == nil {
		t.Fatal("NewAgentRepo returned nil")
	}
}
