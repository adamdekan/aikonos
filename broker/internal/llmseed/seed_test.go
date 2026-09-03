package llmseed

import (
	"context"
	"os"
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/secrets"
)

// ── in-memory fakes ───────────────────────────────────────────────────────────

type fakeRepo struct {
	mu       sync.Mutex
	rows     map[string]db.Provider // "tenant/id"
	upserts  int
	defaults []string
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{rows: map[string]db.Provider{}}
}

func (f *fakeRepo) Get(_ context.Context, tenant, id string) (db.Provider, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.rows[tenant+"/"+id]
	if !ok {
		return db.Provider{}, db.ErrProviderNotFound
	}
	return p, nil
}

func (f *fakeRepo) Upsert(_ context.Context, tenant string, p db.Provider) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[tenant+"/"+p.ID] = p
	f.upserts++
	return nil
}

func (f *fakeRepo) SetDefault(_ context.Context, _, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.defaults = append(f.defaults, id)
	return nil
}

type fakeSec struct {
	mu      sync.Mutex
	written map[string]string // "tenant/id" → key
}

func newFakeSec() *fakeSec { return &fakeSec{written: map[string]string{}} }

func (f *fakeSec) WriteProviderKey(_ context.Context, tenant, id, key string) error {
	f.mu.Lock()
	f.written[tenant+"/"+id] = key
	f.mu.Unlock()
	return nil
}
func (f *fakeSec) ReadProviderKey(_ context.Context, tenant, id string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.written[tenant+"/"+id]
	return v, ok, nil
}
func (f *fakeSec) DeleteProviderKey(_ context.Context, _, _ string) error { return nil }
func (f *fakeSec) ProviderKeyPath(tenant, id string) (string, error) {
	return secrets.ProviderKeyPath(tenant, id)
}
func (f *fakeSec) ReadConnector(_ context.Context, _, _, _ string) (*secrets.ConnectorToken, error) {
	return nil, secrets.ErrNotFound
}
func (f *fakeSec) WriteConnector(_ context.Context, _, _, _ string, _ *secrets.ConnectorToken) error {
	return nil
}
func (f *fakeSec) DeleteConnector(_ context.Context, _, _, _ string) error   { return nil }
func (f *fakeSec) ReadM365App(context.Context, string) (string, bool, error) { return "", false, nil }
func (f *fakeSec) WriteM365App(context.Context, string, string) error        { return nil }
func (f *fakeSec) DeleteM365App(context.Context, string) error               { return nil }

const testTenant = "11111111-1111-1111-1111-111111111111"

// ── tests ─────────────────────────────────────────────────────────────────────

func writeSeedFile(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "seed*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return f.Name()
}

const minimalSeedYAML = `
- id: openrouter
  name: OpenRouter
  endpoint: https://openrouter.ai/api/v1
  api: openai-completions
  enabled: true
  is_default: true
  apiKey: "sk-test-key"
  models:
    - id: anthropic/claude-sonnet-4-6
      priceIn: 0.000003
      priceOut: 0.000015
`

func TestSeed_InsertsNewProvider(t *testing.T) {
	repo := newFakeRepo()
	sec := newFakeSec()
	path := writeSeedFile(t, minimalSeedYAML)

	Seed(context.Background(), repo, sec, testTenant, path, zap.NewNop())

	if repo.upserts != 1 {
		t.Fatalf("want 1 upsert, got %d", repo.upserts)
	}
	p, err := repo.Get(context.Background(), testTenant, "openrouter")
	if err != nil {
		t.Fatalf("provider not stored: %v", err)
	}
	if !p.HasKey {
		t.Error("has_key should be true when api key was stored")
	}
	if len(repo.defaults) != 1 || repo.defaults[0] != "openrouter" {
		t.Errorf("SetDefault not called for default provider, got %v", repo.defaults)
	}
	if sec.written[testTenant+"/openrouter"] != "sk-test-key" {
		t.Errorf("key not written to secrets: %+v", sec.written)
	}
}

func TestSeed_Idempotent(t *testing.T) {
	repo := newFakeRepo()
	sec := newFakeSec()
	path := writeSeedFile(t, minimalSeedYAML)

	Seed(context.Background(), repo, sec, testTenant, path, zap.NewNop())
	Seed(context.Background(), repo, sec, testTenant, path, zap.NewNop())

	if repo.upserts != 1 {
		t.Errorf("second Seed must not upsert again, got %d upserts", repo.upserts)
	}
}

func TestSeed_AdminEditNotClobbered(t *testing.T) {
	repo := newFakeRepo()
	sec := newFakeSec()
	path := writeSeedFile(t, minimalSeedYAML)

	// First seed inserts the provider.
	Seed(context.Background(), repo, sec, testTenant, path, zap.NewNop())

	// Admin edit: change the name.
	p, _ := repo.Get(context.Background(), testTenant, "openrouter")
	p.Name = "Admin-Edited"
	_ = repo.Upsert(context.Background(), testTenant, p)

	// Second seed must not overwrite.
	Seed(context.Background(), repo, sec, testTenant, path, zap.NewNop())
	p2, _ := repo.Get(context.Background(), testTenant, "openrouter")
	if p2.Name != "Admin-Edited" {
		t.Errorf("admin edit was clobbered: got name %q", p2.Name)
	}
}

func TestSeed_EmptyPathSkips(t *testing.T) {
	repo := newFakeRepo()
	Seed(context.Background(), repo, nil, testTenant, "", zap.NewNop())
	if repo.upserts != 0 {
		t.Errorf("empty path must be a no-op, got %d upserts", repo.upserts)
	}
}

func TestSeed_EnvInterpolation(t *testing.T) {
	t.Setenv("TEST_SEED_API_KEY", "env-key-value")
	repo := newFakeRepo()
	sec := newFakeSec()
	path := writeSeedFile(t, `
- id: provider-env
  name: EnvProvider
  endpoint: https://example.com/api
  api: openai-completions
  enabled: true
  apiKey: "${TEST_SEED_API_KEY}"
  models:
    - id: model-1
`)
	Seed(context.Background(), repo, sec, testTenant, path, zap.NewNop())
	if sec.written[testTenant+"/provider-env"] != "env-key-value" {
		t.Errorf("env interpolation failed: %+v", sec.written)
	}
}

func TestSeed_BlankResolvedKeyStoresProviderWithoutKey(t *testing.T) {
	// An unset env var → blank key → provider is seeded with has_key=false, no Vault write.
	repo := newFakeRepo()
	sec := newFakeSec()
	path := writeSeedFile(t, `
- id: no-key-provider
  name: NoKey
  endpoint: https://example.com/api
  api: openai-completions
  enabled: true
  apiKey: "${AIKONOS_TEST_UNSET_ENV_VAR_XYZ}"
  models:
    - id: model-1
`)
	os.Unsetenv("AIKONOS_TEST_UNSET_ENV_VAR_XYZ")
	Seed(context.Background(), repo, sec, testTenant, path, zap.NewNop())
	if repo.upserts != 1 {
		t.Fatalf("provider should be upserted even without a key")
	}
	p, _ := repo.Get(context.Background(), testTenant, "no-key-provider")
	if p.HasKey {
		t.Error("has_key should be false when api key resolved to blank")
	}
	if _, ok := sec.written[testTenant+"/no-key-provider"]; ok {
		t.Error("must not write a blank key to Vault")
	}
}
