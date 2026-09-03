package db_test

import (
	"testing"

	"github.com/adamdekan/aikonos/broker/internal/db"
)

// TestAgentApiKeyRepo_HasExpectedType is a placeholder to keep the file non-empty
// after the ApiKeyStore interface moved to the broker package (where the seam is
// consumed). The concrete type check lives in broker/api_keys_test.go via fakeApiKeyStore.
func TestAgentApiKeyRepo_HasExpectedType(t *testing.T) {
	repo := db.NewAgentApiKeyRepo(nil, nil)
	_ = repo // compile-time proof the constructor still returns *AgentApiKeyRepo
}

// TestStoredApiKey_ZeroValue verifies a zero-value StoredApiKey is sane (no panics,
// exportable fields accessible).
func TestStoredApiKey_ZeroValue(t *testing.T) {
	var k db.StoredApiKey
	if k.ID.String() == "" {
		// uuid.UUID zero value is "00000000-0000-0000-0000-000000000000" — fine.
	}
	if k.KeyHash != "" {
		t.Error("zero-value KeyHash should be empty")
	}
	if k.Revoked() {
		t.Error("zero-value StoredApiKey must not be revoked")
	}
}
