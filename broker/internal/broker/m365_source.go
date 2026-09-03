package broker

// m365_source.go — OrgSettingsM365Source implements connector.TenantAppSource
// by composing OrgSettingsRepo (m365_tenant_id/client_id/enabled) with
// Secrets.ReadM365App (the Vault-held client_secret). A short TTL cache makes
// the per-request lookup (ensureOneDriveOBO runs on every ListConnectors)
// cheap without a Postgres + Vault round trip on every call — sound only
// because the singleton broker (cmd/broker/singleton.go) guarantees a single
// in-memory cache. Invalidate is wired from m365_admin.go's
// Upsert/DeleteM365Connection so a config change is visible immediately
// rather than waiting out the TTL.

import (
	"context"
	"sync"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/connector"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/secrets"
)

// m365SourceCacheTTL bounds how stale a resolved (or absent) M365 connection
// can be before the next lookup re-reads Postgres/Vault.
const m365SourceCacheTTL = 30 * time.Second

type m365CacheEntry struct {
	app      connector.M365App
	ok       bool
	cachedAt time.Time
}

// orgSettingsGetter is the seam OrgSettingsM365Source depends on for tenant
// metadata — satisfied by *db.OrgSettingsRepo in production. Narrowing it to
// an interface (rpc-twins-tails precedent) lets unit tests inject a fake
// instead of standing up a live Postgres pool.
type orgSettingsGetter interface {
	Get(ctx context.Context, tenantID string) (db.OrgSettings, error)
}

// OrgSettingsM365Source is the production connector.TenantAppSource.
type OrgSettingsM365Source struct {
	orgSettings orgSettingsGetter
	secrets     secrets.Provider

	mu    sync.Mutex
	cache map[string]m365CacheEntry
}

// NewOrgSettingsM365Source builds the TenantAppSource wired in main.go.
func NewOrgSettingsM365Source(orgSettings *db.OrgSettingsRepo, sec secrets.Provider) *OrgSettingsM365Source {
	return &OrgSettingsM365Source{orgSettings: orgSettings, secrets: sec, cache: make(map[string]m365CacheEntry)}
}

// M365App resolves the tenant's configured M365 app, serving from the TTL
// cache when fresh. ok=false covers every "not configured" reason (no row,
// disabled, no secret) uniformly — callers don't need to distinguish them.
func (s *OrgSettingsM365Source) M365App(ctx context.Context, tenant string) (connector.M365App, bool, error) {
	if cached, hit := s.cached(tenant); hit {
		return cached.app, cached.ok, nil
	}
	app, ok, err := s.resolve(ctx, tenant)
	if err != nil {
		return connector.M365App{}, false, err
	}
	s.store(tenant, app, ok)
	return app, ok, nil
}

func (s *OrgSettingsM365Source) resolve(ctx context.Context, tenant string) (connector.M365App, bool, error) {
	if s.orgSettings == nil || s.secrets == nil {
		return connector.M365App{}, false, nil
	}
	settings, err := s.orgSettings.Get(ctx, tenant)
	if err != nil {
		return connector.M365App{}, false, err
	}
	if settings.M365TenantID == "" || settings.M365ClientID == "" ||
		settings.M365Enabled == nil || !*settings.M365Enabled {
		return connector.M365App{}, false, nil
	}
	secret, ok, err := s.secrets.ReadM365App(ctx, tenant)
	if err != nil {
		return connector.M365App{}, false, err
	}
	if !ok {
		return connector.M365App{}, false, nil
	}
	return connector.M365App{
		TenantID:     settings.M365TenantID,
		ClientID:     settings.M365ClientID,
		ClientSecret: secret,
	}, true, nil
}

func (s *OrgSettingsM365Source) cached(tenant string) (m365CacheEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.cache[tenant]
	if !ok || time.Since(e.cachedAt) > m365SourceCacheTTL {
		return m365CacheEntry{}, false
	}
	return e, true
}

func (s *OrgSettingsM365Source) store(tenant string, app connector.M365App, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[tenant] = m365CacheEntry{app: app, ok: ok, cachedAt: time.Now()}
}

// Invalidate clears the cached entry for a tenant. Called on every
// Upsert/DeleteM365Connection so a config change takes effect on the very
// next lookup instead of waiting out the TTL.
func (s *OrgSettingsM365Source) Invalidate(tenant string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, tenant)
}
