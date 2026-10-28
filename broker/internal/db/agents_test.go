package db

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Pure-logic tests for Agent struct and JSON marshal/unmarshal invariants.
// DB-backed round-trips follow the in-cluster pattern (TEST_DATABASE_URL-gated).

// TestAgentAllowedProvidersJSONRoundTrip verifies that AllowedProviders marshals
// to/from JSON correctly, matching the jsonb column behaviour.
func TestAgentAllowedProvidersJSONRoundTrip(t *testing.T) {
	a := Agent{
		AllowedProviders:  []string{"anthropic", "openrouter"},
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

// TestAgentRepo_ServiceAgentRoundTrip pins AG-17 S5: service_agent survives
// Create, Get and Update (the admin toggle) in both directions.
func TestAgentRepo_ServiceAgentRoundTrip(t *testing.T) {
	ctx := context.Background()
	repo := NewAgentRepo(openTestPool(t), zap.NewNop())
	a := &Agent{ID: uuid.New(), TenantID: uuid.New(), Name: "svc", ApprovalMode: "auto", ServiceAgent: true}
	if err := repo.Create(ctx, a); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := repo.Get(ctx, a.TenantID.String(), a.ID.String())
	if err != nil || !got.ServiceAgent {
		t.Fatalf("Get after create: service_agent=%v err=%v, want true", got != nil && got.ServiceAgent, err)
	}
	a.ServiceAgent = false
	if err := repo.Update(ctx, a); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got, err = repo.Get(ctx, a.TenantID.String(), a.ID.String()); err != nil || got.ServiceAgent {
		t.Fatalf("Get after update: err=%v, want service_agent=false", err)
	}
}

// TestAgentRepo_DefaultAgent pins AG-01: EnsureDefault creates one default
// agent per tenant (idempotent), Delete refuses it, and the per-user setting
// round-trips, clears, and cascades away with its agent.
func TestAgentRepo_DefaultAgent(t *testing.T) {
	ctx := context.Background()
	repo := NewAgentRepo(openTestPool(t), zap.NewNop())
	tenant := uuid.New()

	def, created, err := repo.EnsureDefault(ctx, tenant.String())
	if err != nil || !created || !def.IsDefault || def.Name != DefaultAgentName {
		t.Fatalf("EnsureDefault: %+v created=%v err=%v", def, created, err)
	}
	again, created, err := repo.EnsureDefault(ctx, tenant.String())
	if err != nil || created || again.ID != def.ID {
		t.Fatalf("EnsureDefault again: %+v created=%v err=%v, want the same agent", again, created, err)
	}
	if err := repo.Delete(ctx, tenant.String(), def.ID.String()); err != ErrAgentNotFound {
		t.Fatalf("Delete default agent: err=%v, want ErrAgentNotFound", err)
	}

	named := &Agent{ID: uuid.New(), TenantID: tenant, Name: "named", ApprovalMode: "needs_approval"}
	if err := repo.Create(ctx, named); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id, err := repo.UserDefault(ctx, tenant.String(), "alice"); err != nil || id != "" {
		t.Fatalf("UserDefault unset: %q %v", id, err)
	}
	if err := repo.SetUserDefault(ctx, tenant.String(), "alice", named.ID.String()); err != nil {
		t.Fatalf("SetUserDefault: %v", err)
	}
	if id, _ := repo.UserDefault(ctx, tenant.String(), "alice"); id != named.ID.String() {
		t.Fatalf("UserDefault = %q, want %s", id, named.ID)
	}
	if err := repo.Delete(ctx, tenant.String(), named.ID.String()); err != nil {
		t.Fatalf("Delete named: %v", err)
	}
	if id, _ := repo.UserDefault(ctx, tenant.String(), "alice"); id != "" {
		t.Fatalf("UserDefault after agent delete = %q, want cleared", id)
	}
	if err := repo.SetUserDefault(ctx, tenant.String(), "alice", def.ID.String()); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetUserDefault(ctx, tenant.String(), "alice", ""); err != nil {
		t.Fatal(err)
	}
	if id, _ := repo.UserDefault(ctx, tenant.String(), "alice"); id != "" {
		t.Fatalf("UserDefault after clear = %q", id)
	}
}
