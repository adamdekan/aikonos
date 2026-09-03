package db_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/adamdekan/aikonos/broker/internal/db"
)

// Pure-logic tests — no live Postgres required.
// These verify the ProvisioningRule struct, JSONB round-trip for Groups, and
// constructor shape. Live-DB behaviour (ClaimProvisioning atomicity, RLS) is
// exercised by integration tests against a real DB.

func TestProvisioningRepo_ConstructsWithNilPool(t *testing.T) {
	// Construction must not panic with a nil pool (mirrors other repos in this package).
	repo := db.NewProvisioningRepo(nil, nil)
	if repo == nil {
		t.Fatal("NewProvisioningRepo returned nil")
	}
}

func TestProvisioningRule_ZeroValue(t *testing.T) {
	var r db.ProvisioningRule
	if r.ID != (uuid.UUID{}) {
		t.Error("zero-value ID should be zero UUID")
	}
	if r.Matcher != "" {
		t.Error("zero-value Matcher should be empty")
	}
	if r.Groups != nil {
		t.Error("zero-value Groups should be nil")
	}
}

func TestProvisioningRule_GroupsJSONRoundTrip(t *testing.T) {
	groups := []string{"security-team", "devs", "admins"}
	b, err := json.Marshal(groups)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got []string
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != len(groups) {
		t.Fatalf("len %d != %d", len(got), len(groups))
	}
	for i := range groups {
		if got[i] != groups[i] {
			t.Errorf("[%d] %q != %q", i, got[i], groups[i])
		}
	}
}

func TestProvisioningRule_Fields(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	id := uuid.New()
	r := db.ProvisioningRule{
		ID:        id,
		Matcher:   "*@contoso.com",
		Groups:    []string{"devs"},
		CreatedBy: "admin@example.com",
		CreatedAt: now,
	}
	if r.ID != id {
		t.Error("ID field mismatch")
	}
	if r.Matcher != "*@contoso.com" {
		t.Error("Matcher field mismatch")
	}
	if len(r.Groups) != 1 || r.Groups[0] != "devs" {
		t.Error("Groups field mismatch")
	}
	if r.CreatedBy != "admin@example.com" {
		t.Error("CreatedBy field mismatch")
	}
	if !r.CreatedAt.Equal(now) {
		t.Error("CreatedAt field mismatch")
	}
}
