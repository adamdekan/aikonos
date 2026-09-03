package broker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/connector"
	"github.com/adamdekan/aikonos/broker/internal/db"
)

// fakeOrgSettingsGetter is the orgSettingsGetter test double — a live
// *db.OrgSettingsRepo needs Postgres, so unit tests inject this instead.
type fakeOrgSettingsGetter struct {
	settings db.OrgSettings
	err      error
	calls    int
}

func (f *fakeOrgSettingsGetter) Get(context.Context, string) (db.OrgSettings, error) {
	f.calls++
	return f.settings, f.err
}

func enabledM365Settings() db.OrgSettings {
	enabled := true
	return db.OrgSettings{M365TenantID: "entra-1", M365ClientID: "client-1", M365Enabled: &enabled}
}

func TestOrgSettingsM365Source_ResolvesConfiguredTenant(t *testing.T) {
	sec := newCapturingM365Secrets()
	sec.stored = "secret-1"
	settings := &fakeOrgSettingsGetter{settings: enabledM365Settings()}
	src := &OrgSettingsM365Source{orgSettings: settings, secrets: sec, cache: map[string]m365CacheEntry{}}

	app, ok, err := src.M365App(context.Background(), "t1")
	if err != nil {
		t.Fatalf("M365App: %v", err)
	}
	if !ok {
		t.Fatal("want ok=true for a fully configured tenant")
	}
	if app != (connector.M365App{TenantID: "entra-1", ClientID: "client-1", ClientSecret: "secret-1"}) {
		t.Fatalf("unexpected app: %+v", app)
	}
}

func TestOrgSettingsM365Source_UnconfiguredWhenNoRow(t *testing.T) {
	settings := &fakeOrgSettingsGetter{settings: db.OrgSettings{}}
	src := &OrgSettingsM365Source{orgSettings: settings, secrets: newCapturingM365Secrets(), cache: map[string]m365CacheEntry{}}

	_, ok, err := src.M365App(context.Background(), "t1")
	if err != nil {
		t.Fatalf("M365App: %v", err)
	}
	if ok {
		t.Fatal("want ok=false when org_settings has no m365 fields set")
	}
}

func TestOrgSettingsM365Source_UnconfiguredWhenDisabled(t *testing.T) {
	disabled := false
	settings := &fakeOrgSettingsGetter{settings: db.OrgSettings{M365TenantID: "e", M365ClientID: "c", M365Enabled: &disabled}}
	src := &OrgSettingsM365Source{orgSettings: settings, secrets: newCapturingM365Secrets(), cache: map[string]m365CacheEntry{}}

	_, ok, err := src.M365App(context.Background(), "t1")
	if err != nil {
		t.Fatalf("M365App: %v", err)
	}
	if ok {
		t.Fatal("want ok=false when m365_enabled=false")
	}
}

func TestOrgSettingsM365Source_UnconfiguredWhenNoSecretStored(t *testing.T) {
	settings := &fakeOrgSettingsGetter{settings: enabledM365Settings()}
	src := &OrgSettingsM365Source{orgSettings: settings, secrets: newCapturingM365Secrets(), cache: map[string]m365CacheEntry{}}

	_, ok, err := src.M365App(context.Background(), "t1")
	if err != nil {
		t.Fatalf("M365App: %v", err)
	}
	if ok {
		t.Fatal("want ok=false when metadata is configured but no Vault secret is stored")
	}
}

func TestOrgSettingsM365Source_PropagatesGetError(t *testing.T) {
	wantErr := errors.New("db down")
	settings := &fakeOrgSettingsGetter{err: wantErr}
	src := &OrgSettingsM365Source{orgSettings: settings, secrets: newCapturingM365Secrets(), cache: map[string]m365CacheEntry{}}

	_, _, err := src.M365App(context.Background(), "t1")
	if !errors.Is(err, wantErr) {
		t.Fatalf("want the underlying Get error, got %v", err)
	}
}

func TestOrgSettingsM365Source_CachesWithinTTL(t *testing.T) {
	sec := newCapturingM365Secrets()
	sec.stored = "secret-1"
	settings := &fakeOrgSettingsGetter{settings: enabledM365Settings()}
	src := &OrgSettingsM365Source{orgSettings: settings, secrets: sec, cache: map[string]m365CacheEntry{}}

	if _, _, err := src.M365App(context.Background(), "t1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.M365App(context.Background(), "t1"); err != nil {
		t.Fatal(err)
	}
	if settings.calls != 1 {
		t.Fatalf("want a single underlying Get within the TTL, got %d calls", settings.calls)
	}
}

func TestOrgSettingsM365Source_ReResolvesAfterTTLExpiry(t *testing.T) {
	sec := newCapturingM365Secrets()
	sec.stored = "secret-1"
	settings := &fakeOrgSettingsGetter{settings: enabledM365Settings()}
	src := &OrgSettingsM365Source{orgSettings: settings, secrets: sec, cache: map[string]m365CacheEntry{}}

	if _, _, err := src.M365App(context.Background(), "t1"); err != nil {
		t.Fatal(err)
	}
	// Force the cache entry stale rather than sleeping out the real TTL.
	src.mu.Lock()
	e := src.cache["t1"]
	e.cachedAt = time.Now().Add(-2 * m365SourceCacheTTL)
	src.cache["t1"] = e
	src.mu.Unlock()

	if _, _, err := src.M365App(context.Background(), "t1"); err != nil {
		t.Fatal(err)
	}
	if settings.calls != 2 {
		t.Fatalf("want a second Get after TTL expiry, got %d calls", settings.calls)
	}
}

func TestOrgSettingsM365Source_InvalidateClearsCache(t *testing.T) {
	sec := newCapturingM365Secrets()
	sec.stored = "secret-1"
	settings := &fakeOrgSettingsGetter{settings: enabledM365Settings()}
	src := &OrgSettingsM365Source{orgSettings: settings, secrets: sec, cache: map[string]m365CacheEntry{}}

	if _, _, err := src.M365App(context.Background(), "t1"); err != nil {
		t.Fatal(err)
	}
	src.Invalidate("t1")
	if _, _, err := src.M365App(context.Background(), "t1"); err != nil {
		t.Fatal(err)
	}
	if settings.calls != 2 {
		t.Fatalf("want Invalidate to force a fresh Get, got %d calls", settings.calls)
	}
}

func TestOrgSettingsM365Source_NilDepsIsUnconfigured(t *testing.T) {
	src := &OrgSettingsM365Source{cache: map[string]m365CacheEntry{}}
	_, ok, err := src.M365App(context.Background(), "t1")
	if err != nil {
		t.Fatalf("M365App: %v", err)
	}
	if ok {
		t.Fatal("want ok=false when orgSettings/secrets are both nil")
	}
}
