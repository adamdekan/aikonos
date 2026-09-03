package broker

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/db"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// capturingWebSearchSecrets records web.search key writes/deletes and can be
// pre-seeded with a stored key (blank-preserves tests). Embeds
// capturingSecrets purely for the LlmProvider-key stub methods that come
// along for the ride when satisfying secrets.Provider.
type capturingWebSearchSecrets struct {
	capturingSecrets
	stored           string
	writtenWebSearch map[string]string // tenant -> key
	deletedWebSearch []string
}

func newCapturingWebSearchSecrets() *capturingWebSearchSecrets {
	return &capturingWebSearchSecrets{capturingSecrets: *newCapturingSecrets(), writtenWebSearch: map[string]string{}}
}

func (c *capturingWebSearchSecrets) ReadWebSearchApp(_ context.Context, tenant string) (string, bool, error) {
	if v, ok := c.writtenWebSearch[tenant]; ok {
		return v, true, nil
	}
	return c.stored, c.stored != "", nil
}
func (c *capturingWebSearchSecrets) WriteWebSearchApp(_ context.Context, tenant, key string) error {
	c.writtenWebSearch[tenant] = key
	return nil
}
func (c *capturingWebSearchSecrets) DeleteWebSearchApp(_ context.Context, tenant string) error {
	c.deletedWebSearch = append(c.deletedWebSearch, tenant)
	delete(c.writtenWebSearch, tenant)
	return nil
}

func validWebSearchConfig() *brokerv1.WebSearchConfig {
	return &brokerv1.WebSearchConfig{Engine: "brave", MaxResults: 5}
}

// ── gate tests: all four RPCs deny non-admin and reject svc- subjects ────────

func TestGetWebSearchConfig_NonAdminDenied(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, nil))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.GetWebSearchConfig(ctx, &brokerv1.GetWebSearchConfigRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin: want PermissionDenied, got %v", err)
	}
}

func TestGetWebSearchConfig_RejectsSvcSubject(t *testing.T) {
	svc := NewBrokerService(testProviderDeps(t, "", nil))
	ctx := ctxWithIdentity(testTenantUUID, "svc-someagent")
	_, err := svc.GetWebSearchConfig(ctx, &brokerv1.GetWebSearchConfigRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("svc- subject: want PermissionDenied, got %v", err)
	}
}

func TestUpsertWebSearchConfig_NonAdminDenied(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, nil))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.UpsertWebSearchConfig(ctx, &brokerv1.UpsertWebSearchConfigRequest{Config: validWebSearchConfig()})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin: want PermissionDenied, got %v", err)
	}
}

func TestUpsertWebSearchConfig_RejectsSvcSubject(t *testing.T) {
	svc := NewBrokerService(testProviderDeps(t, "", nil))
	ctx := ctxWithIdentity(testTenantUUID, "svc-someagent")
	_, err := svc.UpsertWebSearchConfig(ctx, &brokerv1.UpsertWebSearchConfigRequest{Config: validWebSearchConfig()})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("svc- subject: want PermissionDenied, got %v", err)
	}
}

func TestDeleteWebSearchConfig_NonAdminDenied(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, nil))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.DeleteWebSearchConfig(ctx, &brokerv1.DeleteWebSearchConfigRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin: want PermissionDenied, got %v", err)
	}
}

func TestDeleteWebSearchConfig_RejectsSvcSubject(t *testing.T) {
	svc := NewBrokerService(testProviderDeps(t, "", nil))
	ctx := ctxWithIdentity(testTenantUUID, "svc-someagent")
	_, err := svc.DeleteWebSearchConfig(ctx, &brokerv1.DeleteWebSearchConfigRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("svc- subject: want PermissionDenied, got %v", err)
	}
}

func TestTestWebSearchConfig_NonAdminDenied(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, nil))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.TestWebSearchConfig(ctx, &brokerv1.TestWebSearchConfigRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin: want PermissionDenied, got %v", err)
	}
}

func TestTestWebSearchConfig_RejectsSvcSubject(t *testing.T) {
	svc := NewBrokerService(testProviderDeps(t, "", nil))
	ctx := ctxWithIdentity(testTenantUUID, "svc-someagent")
	_, err := svc.TestWebSearchConfig(ctx, &brokerv1.TestWebSearchConfigRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("svc- subject: want PermissionDenied, got %v", err)
	}
}

// ── GetWebSearchConfig behavior ──────────────────────────────────────────────

func TestGetWebSearchConfig_UnconfiguredReturnsZeroValue(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, nil))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.GetWebSearchConfig(ctx, &brokerv1.GetWebSearchConfigRequest{})
	if err != nil {
		t.Fatalf("GetWebSearchConfig: %v", err)
	}
	if resp.Config.Engine != "" || resp.Config.MaxResults != 0 {
		t.Fatalf("unconfigured config must be zero-value, got %+v", resp.Config)
	}
	if resp.Config.HasKey {
		t.Fatal("want has_key=false when nothing stored")
	}
}

func TestGetWebSearchConfig_HasKeyFromVaultEvenWithoutOrgSettings(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	sec := newCapturingWebSearchSecrets()
	sec.stored = "existing-key"
	svc := NewBrokerService(testProviderDeps(t, srv.URL, sec))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.GetWebSearchConfig(ctx, &brokerv1.GetWebSearchConfigRequest{})
	if err != nil {
		t.Fatalf("GetWebSearchConfig: %v", err)
	}
	if !resp.Config.HasKey {
		t.Fatal("want has_key=true when a key is stored in Vault")
	}
	// api_key must never travel on the wire — the response type doesn't even
	// have a field for it, but confirm no leakage indirectly via has_key being
	// the only key-derived signal.
	if resp.Config.Engine != "" {
		t.Fatalf("metadata still zero-value with no org_settings row, got %+v", resp.Config)
	}
}

// ── UpsertWebSearchConfig validation + key handling ──────────────────────────

func TestUpsertWebSearchConfig_RequiresEngine(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, nil))
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")

	cases := []*brokerv1.WebSearchConfig{
		nil,
		{},
		{MaxResults: 5},
	}
	for _, cfg := range cases {
		_, err := svc.UpsertWebSearchConfig(ctx, &brokerv1.UpsertWebSearchConfigRequest{Config: cfg})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("config=%+v: want InvalidArgument, got %v", cfg, err)
		}
	}
}

func TestUpsertWebSearchConfig_RejectsNegativeMaxResults(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, nil))
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")

	_, err := svc.UpsertWebSearchConfig(ctx, &brokerv1.UpsertWebSearchConfigRequest{
		Config: &brokerv1.WebSearchConfig{Engine: "brave", MaxResults: -1},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("negative max_results: want InvalidArgument, got %v", err)
	}
}

// stubSecrets (connectors_test.go) satisfies secrets.Provider but not the
// websearchSecrets type assertion — Upsert must fail closed, not silently
// report hasKey=true while the key never reached Vault.
func TestUpsertWebSearchConfig_SecretsWithoutWebSearchSupportFailsClosed(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, stubSecrets{}))
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")

	_, err := svc.UpsertWebSearchConfig(ctx, &brokerv1.UpsertWebSearchConfigRequest{
		Config: validWebSearchConfig(),
		ApiKey: "super-secret-key",
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("want Internal when Secrets doesn't support web.search key storage, got %v", err)
	}
}

func TestUpsertWebSearchConfig_NoOrgSettingsConfiguredFailsClosedButWritesKey(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	sec := newCapturingWebSearchSecrets()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, sec))
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")

	_, err := svc.UpsertWebSearchConfig(ctx, &brokerv1.UpsertWebSearchConfigRequest{
		Config: validWebSearchConfig(),
		ApiKey: "super-secret-key",
	})
	// testProviderDeps never wires Deps.OrgSettings (no live Postgres in unit
	// tests) — the RPC must fail closed rather than silently drop the metadata.
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition (org settings unconfigured), got %v", err)
	}
	if got := sec.writtenWebSearch[testTenantUUID]; got != "super-secret-key" {
		t.Fatalf("key must still be written to Vault before the metadata check, got %v", sec.writtenWebSearch)
	}
}

// F-1 precedent (mirrors TestUpsertM365Connection_BlankSecretPreservesStoredNeverWrites):
// a blank api_key on an edit must never call WriteWebSearchApp, even though a
// key is already stored.
func TestUpsertWebSearchConfig_BlankKeyPreservesStoredNeverWrites(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	sec := newCapturingWebSearchSecrets()
	sec.stored = "existing-key"
	svc := NewBrokerService(testProviderDeps(t, srv.URL, sec))
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")

	_, err := svc.UpsertWebSearchConfig(ctx, &brokerv1.UpsertWebSearchConfigRequest{
		Config: validWebSearchConfig(),
		ApiKey: "",
	})
	// testProviderDeps never wires Deps.OrgSettings (no live Postgres in unit
	// tests) — same fail-closed path as
	// TestUpsertWebSearchConfig_NoOrgSettingsConfiguredFailsClosedButWritesKey;
	// the point here is the read-only behavior below, not this status code.
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition (org settings unconfigured), got %v", err)
	}
	if len(sec.writtenWebSearch) != 0 {
		t.Fatalf("blank api_key on an edit must never call WriteWebSearchApp, got writes: %v", sec.writtenWebSearch)
	}
}

// emitWebSearchAudit is the single place that shapes a web.search audit
// event; test it directly rather than through a full RPC round-trip (Upsert
// only reaches audit on a successful Merge, which needs a live
// OrgSettingsRepo unavailable in this unit-test harness).
func TestEmitWebSearchAudit_ChangedKeysOnlyNeverValues(t *testing.T) {
	sink := &capturingAuditSink{}
	svc := NewBrokerService(Deps{Audit: sink})

	svc.emitWebSearchAudit(context.Background(), "trace-1", testTenantUUID, "admin@example.com",
		"websearch.config.upsert", []string{"engine", "max_results", "api_key"})

	ev := sink.last()
	if ev == nil {
		t.Fatal("expected an audit event")
	}
	if ev.Context == nil {
		t.Fatal("expected a context struct")
	}
	changedVal, ok := ev.Context.Fields["changed_keys"]
	if !ok {
		t.Fatal("changed_keys missing from audit context")
	}
	list := changedVal.GetListValue()
	if list == nil {
		t.Fatal("changed_keys must be a list value")
	}
	wantKeys := map[string]bool{"engine": true, "max_results": true, "api_key": true}
	for _, v := range list.Values {
		if !wantKeys[v.GetStringValue()] {
			t.Fatalf("unexpected/leaked value in changed_keys: %q", v.GetStringValue())
		}
	}
	if len(list.Values) != len(wantKeys) {
		t.Fatalf("want %d changed keys, got %d: %v", len(wantKeys), len(list.Values), list.Values)
	}
}

// ── DeleteWebSearchConfig best-effort key cleanup ────────────────────────────

func TestDeleteWebSearchConfig_DeletesKeyBestEffort(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	sec := newCapturingWebSearchSecrets()
	sec.stored = "to-remove"
	deps := testProviderDeps(t, srv.URL, sec)
	sink := &capturingAuditSink{}
	deps.Audit = sink
	svc := NewBrokerService(deps)
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")

	if _, err := svc.DeleteWebSearchConfig(ctx, &brokerv1.DeleteWebSearchConfigRequest{}); err != nil {
		t.Fatalf("DeleteWebSearchConfig: %v", err)
	}
	if len(sec.deletedWebSearch) != 1 || sec.deletedWebSearch[0] != testTenantUUID {
		t.Fatalf("want DeleteWebSearchApp called for %q, got %v", testTenantUUID, sec.deletedWebSearch)
	}
	if sink.last() == nil {
		t.Fatal("expected an audit event even when org_settings is unconfigured")
	}
}

// ── webSearchHasKey ───────────────────────────────────────────────────────────

func TestWebSearchHasKey(t *testing.T) {
	sec := newCapturingWebSearchSecrets()
	svc := NewBrokerService(Deps{Secrets: sec})
	if svc.webSearchHasKey(context.Background(), "t1") {
		t.Fatal("want false when nothing stored")
	}
	sec.stored = "abc"
	if !svc.webSearchHasKey(context.Background(), "t1") {
		t.Fatal("want true once a key is stored")
	}
}

func TestWebSearchHasKey_NilSecretsIsFalse(t *testing.T) {
	svc := NewBrokerService(Deps{})
	if svc.webSearchHasKey(context.Background(), "t1") {
		t.Fatal("nil Secrets must resolve to false, not panic")
	}
}

// ── resolveWebSearchTestInputs ────────────────────────────────────────────────

func TestResolveWebSearchTestInputs_RequestOverridesStored(t *testing.T) {
	sec := newCapturingWebSearchSecrets()
	sec.stored = "stored-key"
	svc := NewBrokerService(Deps{Secrets: sec})

	engine, apiKey := svc.resolveWebSearchTestInputs(context.Background(), "t1", &brokerv1.TestWebSearchConfigRequest{
		Config: &brokerv1.WebSearchConfig{Engine: "brave"},
		ApiKey: "req-key",
	})
	if engine != "brave" || apiKey != "req-key" {
		t.Fatalf("unexpected resolution: engine=%q apiKey=%q", engine, apiKey)
	}
}

func TestResolveWebSearchTestInputs_BlankKeyFallsBackToStored(t *testing.T) {
	sec := newCapturingWebSearchSecrets()
	sec.stored = "stored-key"
	svc := NewBrokerService(Deps{Secrets: sec})

	_, apiKey := svc.resolveWebSearchTestInputs(context.Background(), "t1", &brokerv1.TestWebSearchConfigRequest{
		Config: &brokerv1.WebSearchConfig{Engine: "brave"},
	})
	if apiKey != "stored-key" {
		t.Fatalf("want fallback to stored key, got %q", apiKey)
	}
}

// ── runWebSearchProbe: fail-closed on unknown/unset engine ───────────────────

func TestRunWebSearchProbe_UnsetEngineFailsClosed(t *testing.T) {
	svc := NewBrokerService(Deps{})
	resp := svc.runWebSearchProbe(context.Background(), "", "some-key")
	if resp.Ok {
		t.Fatal("unset engine must not report ok=true")
	}
}

func TestRunWebSearchProbe_UnknownEngineFailsClosed(t *testing.T) {
	svc := NewBrokerService(Deps{})
	resp := svc.runWebSearchProbe(context.Background(), "not-a-real-engine", "some-key")
	if resp.Ok {
		t.Fatal("unknown engine must not report ok=true")
	}
}

// CP6: the probe now dispatches through toolproxy.ProbeSearchEngine (shared
// with the web.search handler) instead of a broker-local per-engine switch —
// exa/tavily must fail closed on a missing key exactly like brave, with no
// per-engine method needed in this package.
func TestRunWebSearchProbe_ExaNoKeyFailsClosed(t *testing.T) {
	svc := NewBrokerService(Deps{})
	resp := svc.runWebSearchProbe(context.Background(), "exa", "")
	if resp.Ok {
		t.Fatal("blank key must not report ok=true")
	}
}

func TestRunWebSearchProbe_TavilyNoKeyFailsClosed(t *testing.T) {
	svc := NewBrokerService(Deps{})
	resp := svc.runWebSearchProbe(context.Background(), "tavily", "")
	if resp.Ok {
		t.Fatal("blank key must not report ok=true")
	}
}

func TestRunWebSearchProbe_BraveNoKeyFailsClosed(t *testing.T) {
	svc := NewBrokerService(Deps{})
	resp := svc.runWebSearchProbe(context.Background(), "brave", "")
	if resp.Ok {
		t.Fatal("blank key must not report ok=true")
	}
}

// ── pure response/mapping helpers ─────────────────────────────────────────────

func TestWebSearchToProto(t *testing.T) {
	ts := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	out := webSearchToProto(db.OrgSettings{
		WebsearchEngine:     "brave",
		WebsearchMaxResults: 5,
		UpdatedBy:           "admin@x.com",
		UpdatedAt:           ts,
	}, true)
	if out.Engine != "brave" || out.MaxResults != 5 || !out.HasKey {
		t.Fatalf("unexpected mapping: %+v", out)
	}
	if out.UpdatedBy != "admin@x.com" || out.UpdatedAt != "2026-07-10T08:00:00Z" {
		t.Fatalf("provenance not mapped: %+v", out)
	}

	zero := webSearchToProto(db.OrgSettings{}, false)
	if zero.Engine != "" || zero.MaxResults != 0 || zero.HasKey || zero.UpdatedAt != "" {
		t.Fatalf("zero-value settings must map to zero-value proto, got %+v", zero)
	}
}

// ── OrgSettingsWebSearchSource.Resolve ────────────────────────────────────────

func TestOrgSettingsWebSearchSource_NilRepoIsUnconfigured(t *testing.T) {
	src := NewOrgSettingsWebSearchSource(nil)
	engine, maxResults, configured, err := src.Resolve(context.Background(), "t1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if configured || engine != "" || maxResults != 0 {
		t.Fatalf("nil repo must resolve as unconfigured, got engine=%q maxResults=%d configured=%v", engine, maxResults, configured)
	}
}
