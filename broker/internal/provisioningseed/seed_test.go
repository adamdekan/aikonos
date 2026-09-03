package provisioningseed

import (
	"context"
	"os"
	"testing"

	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/db"
)

// fakeStore is an in-memory RuleStore for tests.
type fakeStore struct {
	rules  []db.ProvisioningRule
	addErr error
}

func (f *fakeStore) ListRules(_ context.Context, _ string) ([]db.ProvisioningRule, error) {
	return f.rules, nil
}

func (f *fakeStore) AddRule(_ context.Context, _ string, rule db.ProvisioningRule) error {
	if f.addErr != nil {
		return f.addErr
	}
	f.rules = append(f.rules, rule)
	return nil
}

const testTenant = "11111111-1111-1111-1111-111111111111"

// writeYAML writes content to a temp file and returns its path.
func writeYAML(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "provisioning*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	return f.Name()
}

func TestSeed_HappyPath(t *testing.T) {
	path := writeYAML(t, `
rules:
  - matcher: "*@example.com"
    groups: ["security-team"]
  - matcher: "alice@corp.com"
    groups: ["admins", "security-team"]
`)
	store := &fakeStore{}
	Seed(context.Background(), store, testTenant, path, zap.NewNop())
	if len(store.rules) != 2 {
		t.Fatalf("want 2 rules seeded, got %d", len(store.rules))
	}
}

func TestSeed_Idempotent(t *testing.T) {
	// Second run must not insert rules that already exist.
	path := writeYAML(t, `
rules:
  - matcher: "*@example.com"
    groups: ["security-team"]
`)
	store := &fakeStore{}
	Seed(context.Background(), store, testTenant, path, zap.NewNop())
	if len(store.rules) != 1 {
		t.Fatalf("first run: want 1 rule, got %d", len(store.rules))
	}
	Seed(context.Background(), store, testTenant, path, zap.NewNop())
	if len(store.rules) != 1 {
		t.Errorf("second run: want still 1 rule (idempotent), got %d", len(store.rules))
	}
}

func TestSeed_InvalidEntrySkipped(t *testing.T) {
	path := writeYAML(t, `
rules:
  - matcher: "*"        # bare wildcard — invalid
    groups: ["security-team"]
  - matcher: "bad email"
    groups: ["security-team"]
  - matcher: "*@example.com"
    groups: []          # empty groups — invalid
  - matcher: "*@valid.com"
    groups: ["security-team"]
`)
	store := &fakeStore{}
	Seed(context.Background(), store, testTenant, path, zap.NewNop())
	if len(store.rules) != 1 {
		t.Errorf("want 1 valid rule, got %d (invalid entries should be skipped)", len(store.rules))
	}
	if store.rules[0].Matcher != "*@valid.com" {
		t.Errorf("unexpected rule seeded: %s", store.rules[0].Matcher)
	}
}

func TestSeed_EmptyPath(t *testing.T) {
	store := &fakeStore{}
	// No path and no env var → should be a silent no-op.
	_ = os.Unsetenv("AIKONOS_PROVISIONING_SEED_FILE")
	Seed(context.Background(), store, testTenant, "", zap.NewNop())
	if len(store.rules) != 0 {
		t.Errorf("empty path: want 0 rules, got %d", len(store.rules))
	}
}

func TestSeed_MissingFile(t *testing.T) {
	store := &fakeStore{}
	Seed(context.Background(), store, testTenant, "/nonexistent/path/seed.yaml", zap.NewNop())
	if len(store.rules) != 0 {
		t.Errorf("missing file: want 0 rules, got %d", len(store.rules))
	}
}
