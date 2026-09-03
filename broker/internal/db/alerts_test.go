package db

import (
	"testing"

	"github.com/adamdekan/aikonos/broker/internal/alerting"
)

// These tests exercise AlertRepo without a real Postgres instance.
// Real CRUD round-trips are verified in-cluster via scripts/verify-netacl.sh
// (same pattern as network_rules). This file covers the struct/zero-value
// invariants and constructor correctness.

func TestAlertRepoNonNilConstruct(t *testing.T) {
	repo := NewAlertRepo(nil, nil)
	if repo == nil {
		t.Fatal("NewAlertRepo returned nil")
	}
}

func TestStoredAlertZeroValue(t *testing.T) {
	var a alerting.StoredAlert
	if a.ID != "" {
		t.Errorf("zero ID should be empty string, got %q", a.ID)
	}
	if a.Summary != nil {
		t.Errorf("zero Summary should be nil")
	}
}

// TestAlertRepoImplementsInterface verifies db.AlertRepo satisfies alerting.AlertRepo
// at compile time — if the interface changes the compiler catches the drift here.
func TestAlertRepoImplementsInterface(t *testing.T) {
	var _ alerting.AlertRepo = (*AlertRepo)(nil)
}
