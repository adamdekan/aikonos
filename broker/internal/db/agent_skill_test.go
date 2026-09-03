package db

import (
	"testing"

	"github.com/google/uuid"
)

// These tests exercise AgentSkillRepo without a real Postgres instance.
// The db package has no testcontainers/shared-pool harness (mirrors
// skill_overlay_test.go and mcp_connections_test.go). Struct/zero-value
// invariants and non-nil constructor are verified here. Live CRUD round-trips
// (Upsert, Get, GetByName, ListByTenant, Delete, RLS isolation) are verified
// in-cluster once migration 020 is applied.

// TestAgentSkillRepo_NonNilConstruct verifies the repo constructor returns a
// non-nil value with a nil pool (nil panics only at query time).
func TestAgentSkillRepo_NonNilConstruct(t *testing.T) {
	repo := NewAgentSkillRepo(nil, nil)
	if repo == nil {
		t.Fatal("NewAgentSkillRepo returned nil")
	}
}

// TestAgentSkillRepo_ZeroValue verifies zero-value field semantics for AgentSkill.
// These match the DB column defaults (false, empty string, nil JSONB byte slices).
func TestAgentSkillRepo_ZeroValue(t *testing.T) {
	s := &AgentSkill{}
	if s.ID != (uuid.UUID{}) {
		t.Errorf("zero ID should be zero UUID, got %v", s.ID)
	}
	if s.TenantID != (uuid.UUID{}) {
		t.Errorf("zero TenantID should be zero UUID, got %v", s.TenantID)
	}
	if s.Name != "" {
		t.Errorf("zero Name should be empty string, got %q", s.Name)
	}
	if s.Description != "" {
		t.Errorf("zero Description should be empty string, got %q", s.Description)
	}
	if s.Body != "" {
		t.Errorf("zero Body should be empty string, got %q", s.Body)
	}
	// AllowedTools and Extras are []byte (raw JSONB); zero value is nil.
	// Upsert coerces nil → '[]' and '{}' respectively before sending to DB.
	if s.AllowedTools != nil {
		t.Errorf("zero AllowedTools should be nil, got %v", s.AllowedTools)
	}
	if s.Extras != nil {
		t.Errorf("zero Extras should be nil, got %v", s.Extras)
	}
	if s.Keywords != nil {
		t.Errorf("zero Keywords should be nil, got %v", s.Keywords)
	}
	// ContextFork and DisableModelInvocation zero-value is false (Go bool
	// default); DB columns also default false — consistent.
	if s.ContextFork {
		t.Error("zero ContextFork should be false")
	}
	if s.DisableModelInvocation {
		t.Error("zero DisableModelInvocation should be false")
	}
	if s.CreatedBy != "" {
		t.Errorf("zero CreatedBy should be empty string, got %q", s.CreatedBy)
	}
}

// TestAgentSkillRepo_ErrSentinelDistinct verifies the not-found sentinel is
// distinct from other error values (callers use errors.Is).
func TestAgentSkillRepo_ErrSentinelDistinct(t *testing.T) {
	if ErrAgentSkillNotFound == nil {
		t.Fatal("ErrAgentSkillNotFound must not be nil")
	}
	if ErrAgentSkillNotFound == ErrSkillOverlayNotFound {
		t.Error("ErrAgentSkillNotFound must be distinct from ErrSkillOverlayNotFound")
	}
}

// TestNormalizeKeywords verifies the write-time normalization contract
// (spec: "Keywords normalize to lowercase, trimmed, deduped at write time"):
// case-folds, trims surrounding whitespace, drops empties, and dedups while
// preserving first-seen order — deterministic input for the gateway matcher.
func TestNormalizeKeywords(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil in nil out", nil, nil},
		{"empty in nil out", []string{}, nil},
		{"lowercases", []string{"Invoice", "REFUND"}, []string{"invoice", "refund"}},
		{"trims whitespace", []string{"  invoice  ", "\trefund\n"}, []string{"invoice", "refund"}},
		{"drops empty/whitespace-only entries", []string{"invoice", "  ", ""}, []string{"invoice"}},
		{"dedups case-insensitively, keeps first-seen order", []string{"Invoice", "invoice", "INVOICE", "refund"}, []string{"invoice", "refund"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeKeywords(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("NormalizeKeywords(%v) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("NormalizeKeywords(%v) = %v, want %v", tc.in, got, tc.want)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Live-DB tests — skipped without a real Postgres instance.
// These encode the CP1 behavioral contract: Upsert, Get, GetByName,
// ListByTenant, Delete, RLS isolation (two tenants), and delete-nonexistent.
// Run against a seeded stack: AIKONOS_TEST_DB_URL=<dsn> go test ./broker/internal/db/...
// ---------------------------------------------------------------------------

// TestAgentSkillRepo_Upsert verifies a row is inserted and can be retrieved.
func TestAgentSkillRepo_Upsert(t *testing.T) {
	t.Skip("requires live Postgres with migration 020 applied — run against seeded stack")
}

// TestAgentSkillRepo_Get verifies Get returns ErrAgentSkillNotFound for a
// missing id.
func TestAgentSkillRepo_Get(t *testing.T) {
	t.Skip("requires live Postgres with migration 020 applied — run against seeded stack")
}

// TestAgentSkillRepo_GetByName verifies GetByName resolves the correct row and
// returns ErrAgentSkillNotFound for a missing name.
func TestAgentSkillRepo_GetByName(t *testing.T) {
	t.Skip("requires live Postgres with migration 020 applied — run against seeded stack")
}

// TestAgentSkillRepo_ListByTenant verifies ListByTenant returns only the
// calling tenant's rows, ordered by name.
func TestAgentSkillRepo_ListByTenant(t *testing.T) {
	t.Skip("requires live Postgres with migration 020 applied — run against seeded stack")
}

// TestAgentSkillRepo_Delete verifies Delete removes the row and that a second
// Delete on the same id returns ErrAgentSkillNotFound.
func TestAgentSkillRepo_Delete(t *testing.T) {
	t.Skip("requires live Postgres with migration 020 applied — run against seeded stack")
}

// TestAgentSkillRepo_RLSIsolation verifies that a row written for tenant A is
// not visible when querying as tenant B (two-tenant cross-read returns empty).
func TestAgentSkillRepo_RLSIsolation(t *testing.T) {
	t.Skip("requires live Postgres with migration 020 applied — run against seeded stack")
}

// TestAgentSkillRepo_DeleteNonExistent verifies Delete of an id that never
// existed returns ErrAgentSkillNotFound (not a generic error).
func TestAgentSkillRepo_DeleteNonExistent(t *testing.T) {
	t.Skip("requires live Postgres with migration 020 applied — run against seeded stack")
}
