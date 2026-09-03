package broker

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	"github.com/adamdekan/aikonos/broker/internal/secrets"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ── fake secrets that capture provider-key calls ──────────────────────────────

type capturingSecrets struct {
	stubSecrets                  // embed the no-op from connectors_test.go
	writtenKeys map[string]string // "tenant/id" → key
	deletedKeys []string
}

func newCapturingSecrets() *capturingSecrets {
	return &capturingSecrets{writtenKeys: map[string]string{}}
}

func (c *capturingSecrets) WriteProviderKey(_ context.Context, tenant, id, key string) error {
	c.writtenKeys[tenant+"/"+id] = key
	return nil
}
func (c *capturingSecrets) DeleteProviderKey(_ context.Context, _, id string) error {
	c.deletedKeys = append(c.deletedKeys, id)
	return nil
}
func (c *capturingSecrets) ReadProviderKey(_ context.Context, _, _ string) (string, bool, error) {
	return "", false, nil
}
func (c *capturingSecrets) ProviderKeyPath(tenant, providerID string) (string, error) {
	return secrets.ProviderKeyPath(tenant, providerID)
}

// ── helper: build a BrokerService wired for provider admin tests ──────────────

func testProviderDeps(t *testing.T, fgaURL string, sec secrets.Provider) Deps {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := policy.Config{OPAEndpoint: "http://unused", OpenFGAEndpoint: fgaURL}
	if fgaURL != "" {
		cfg.OpenFGAStoreID = "store-1"
	}
	eng, err := policy.NewEngine(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sec == nil {
		sec = stubSecrets{}
	}
	return Deps{
		Logger:     zap.NewNop(),
		Audit:      em,
		Policy:     eng,
		TenantID:   "aikonos-dev",
		KnownUsers: []string{"admin@example.com", "alice@example.com"},
		Secrets:    sec,
	}
}

func validProvider() *brokerv1.LlmProvider {
	return &brokerv1.LlmProvider{
		Id:       "openrouter",
		Name:     "OpenRouter",
		Endpoint: "https://openrouter.ai/api/v1",
		Api:      "openai-completions",
		Enabled:  true,
		Models: []*brokerv1.LlmModel{
			{Id: "anthropic/claude-sonnet-4-6", PriceIn: 0.000003, PriceOut: 0.000015},
		},
	}
}

// ── gate tests ────────────────────────────────────────────────────────────────

func TestListLlmProviders_NonAdminDenied(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, nil))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.ListLlmProviders(ctx, &brokerv1.ListLlmProvidersRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin: want PermissionDenied, got %v", err)
	}
}

func TestListLlmProviders_AdminAllowed(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, nil))

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.ListLlmProviders(ctx, &brokerv1.ListLlmProvidersRequest{})
	if err != nil {
		t.Fatalf("admin list: %v", err)
	}
	if resp == nil {
		t.Fatal("want non-nil response")
	}
}

// ListLlmProvidersRequest has no user_id field — the identity comes entirely
// from the verified OIDC context. This test verifies the gate is evaluated
// against the authenticated subject, not any request field.
func TestListLlmProviders_NonAdminContextRejected(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, nil))

	// authenticated as alice (not an admin)
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.ListLlmProviders(ctx, &brokerv1.ListLlmProvidersRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin context: want PermissionDenied, got %v", err)
	}
}

func TestListLlmProviders_RejectsSvcSubject(t *testing.T) {
	// svc- principals must be rejected at callerIdentity before any gate.
	svc := NewBrokerService(testProviderDeps(t, "", nil))
	ctx := ctxWithIdentity(testTenantUUID, "svc-someagent")
	_, err := svc.ListLlmProviders(ctx, &brokerv1.ListLlmProvidersRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("svc- subject: want PermissionDenied, got %v", err)
	}
}

// ── mutation RPC gate tests ───────────────────────────────────────────────────

func TestUpsertLlmProvider_NonAdminDenied(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, nil))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.UpsertLlmProvider(ctx, &brokerv1.UpsertLlmProviderRequest{Provider: validProvider()})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin UpsertLlmProvider: want PermissionDenied, got %v", err)
	}
}

func TestUpsertLlmProvider_RejectsSvcSubject(t *testing.T) {
	svc := NewBrokerService(testProviderDeps(t, "", nil))
	ctx := ctxWithIdentity(testTenantUUID, "svc-someagent")
	_, err := svc.UpsertLlmProvider(ctx, &brokerv1.UpsertLlmProviderRequest{Provider: validProvider()})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("svc- subject UpsertLlmProvider: want PermissionDenied, got %v", err)
	}
}

func TestDeleteLlmProvider_NonAdminDenied(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, nil))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.DeleteLlmProvider(ctx, &brokerv1.DeleteLlmProviderRequest{Id: "openrouter"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin DeleteLlmProvider: want PermissionDenied, got %v", err)
	}
}

func TestDeleteLlmProvider_RejectsSvcSubject(t *testing.T) {
	svc := NewBrokerService(testProviderDeps(t, "", nil))
	ctx := ctxWithIdentity(testTenantUUID, "svc-someagent")
	_, err := svc.DeleteLlmProvider(ctx, &brokerv1.DeleteLlmProviderRequest{Id: "openrouter"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("svc- subject DeleteLlmProvider: want PermissionDenied, got %v", err)
	}
}

func TestSetDefaultProvider_NonAdminDenied(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, nil))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.SetDefaultProvider(ctx, &brokerv1.SetDefaultProviderRequest{Id: "openrouter"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin SetDefaultProvider: want PermissionDenied, got %v", err)
	}
}

func TestSetDefaultProvider_RejectsSvcSubject(t *testing.T) {
	svc := NewBrokerService(testProviderDeps(t, "", nil))
	ctx := ctxWithIdentity(testTenantUUID, "svc-someagent")
	_, err := svc.SetDefaultProvider(ctx, &brokerv1.SetDefaultProviderRequest{Id: "openrouter"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("svc- subject SetDefaultProvider: want PermissionDenied, got %v", err)
	}
}

// ── SetDefaultVisionProvider ──────────────────────────────────────────────────

func TestSetDefaultVisionProvider_NonAdminDenied(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, nil))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.SetDefaultVisionProvider(ctx, &brokerv1.SetDefaultVisionProviderRequest{Id: "openrouter"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin SetDefaultVisionProvider: want PermissionDenied, got %v", err)
	}
}

func TestSetDefaultVisionProvider_RejectsSvcSubject(t *testing.T) {
	svc := NewBrokerService(testProviderDeps(t, "", nil))
	ctx := ctxWithIdentity(testTenantUUID, "svc-someagent")
	_, err := svc.SetDefaultVisionProvider(ctx, &brokerv1.SetDefaultVisionProviderRequest{Id: "openrouter"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("svc- subject SetDefaultVisionProvider: want PermissionDenied, got %v", err)
	}
}

func TestSetDefaultVisionProvider_ClearsPriorDefault(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	store := newFakeLlmProviderStore()
	store.rows[testTenantUUID+"/vision-a"] = fakeLlmProviderRow{
		id: "vision-a", name: "A", endpoint: "https://a.test", api: "openai-completions",
		visionCapable: true, isDefaultVision: true,
	}
	store.rows[testTenantUUID+"/vision-b"] = fakeLlmProviderRow{
		id: "vision-b", name: "B", endpoint: "https://b.test", api: "openai-completions",
		visionCapable: true,
	}
	deps := testProviderDeps(t, srv.URL, nil)
	deps.Providers = store
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.SetDefaultVisionProvider(ctx, &brokerv1.SetDefaultVisionProviderRequest{Id: "vision-b"})
	if err != nil {
		t.Fatalf("SetDefaultVisionProvider: %v", err)
	}
	if store.rows[testTenantUUID+"/vision-a"].isDefaultVision {
		t.Error("prior default-vision provider still marked default after switching")
	}
	if !store.rows[testTenantUUID+"/vision-b"].isDefaultVision {
		t.Error("target provider not marked default-vision")
	}
}

func TestSetDefaultVisionProvider_RejectsNonVisionCapable(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	store := newFakeLlmProviderStore()
	store.rows[testTenantUUID+"/text-only"] = fakeLlmProviderRow{
		id: "text-only", name: "Text", endpoint: "https://x.test", api: "openai-completions",
		visionCapable: false,
	}
	deps := testProviderDeps(t, srv.URL, nil)
	deps.Providers = store
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.SetDefaultVisionProvider(ctx, &brokerv1.SetDefaultVisionProviderRequest{Id: "text-only"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("non-vision-capable target: want InvalidArgument, got %v", err)
	}
	if store.rows[testTenantUUID+"/text-only"].isDefaultVision {
		t.Error("text-only provider must not be silently set as default-vision")
	}
}

// ── SetFallbackProvider ───────────────────────────────────────────────────────

func TestSetFallbackProvider_NonAdminDenied(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, nil))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.SetFallbackProvider(ctx, &brokerv1.SetFallbackProviderRequest{Id: "openrouter"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin SetFallbackProvider: want PermissionDenied, got %v", err)
	}
}

func TestSetFallbackProvider_RejectsSvcSubject(t *testing.T) {
	svc := NewBrokerService(testProviderDeps(t, "", nil))
	ctx := ctxWithIdentity(testTenantUUID, "svc-someagent")
	_, err := svc.SetFallbackProvider(ctx, &brokerv1.SetFallbackProviderRequest{Id: "openrouter"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("svc- subject SetFallbackProvider: want PermissionDenied, got %v", err)
	}
}

func TestSetFallbackProvider_ClearsPriorFallback(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	store := newFakeLlmProviderStore()
	store.rows[testTenantUUID+"/fallback-a"] = fakeLlmProviderRow{
		id: "fallback-a", name: "A", endpoint: "https://a.test", api: "openai-completions",
		isFallback: true,
	}
	store.rows[testTenantUUID+"/fallback-b"] = fakeLlmProviderRow{
		id: "fallback-b", name: "B", endpoint: "https://b.test", api: "openai-completions",
	}
	deps := testProviderDeps(t, srv.URL, nil)
	deps.Providers = store
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.SetFallbackProvider(ctx, &brokerv1.SetFallbackProviderRequest{Id: "fallback-b"})
	if err != nil {
		t.Fatalf("SetFallbackProvider: %v", err)
	}
	if store.rows[testTenantUUID+"/fallback-a"].isFallback {
		t.Error("prior fallback provider still marked fallback after switching")
	}
	if !store.rows[testTenantUUID+"/fallback-b"].isFallback {
		t.Error("target provider not marked fallback")
	}
}

// TestSetFallbackProvider_AllowsNonVisionCapable pins the deliberate asymmetry
// with SetDefaultVisionProvider: a fallback serves chat, so it need not be
// vision_capable.
func TestSetFallbackProvider_AllowsNonVisionCapable(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	store := newFakeLlmProviderStore()
	store.rows[testTenantUUID+"/text-only"] = fakeLlmProviderRow{
		id: "text-only", name: "Text", endpoint: "https://x.test", api: "openai-completions",
		visionCapable: false,
	}
	deps := testProviderDeps(t, srv.URL, nil)
	deps.Providers = store
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	if _, err := svc.SetFallbackProvider(ctx, &brokerv1.SetFallbackProviderRequest{Id: "text-only"}); err != nil {
		t.Fatalf("SetFallbackProvider on a text-only provider: %v", err)
	}
	if !store.rows[testTenantUUID+"/text-only"].isFallback {
		t.Error("text-only provider not marked fallback")
	}
}

func TestSetFallbackProvider_UnknownIdNotFound(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	deps := testProviderDeps(t, srv.URL, nil)
	deps.Providers = newFakeLlmProviderStore()
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.SetFallbackProvider(ctx, &brokerv1.SetFallbackProviderRequest{Id: "nope"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("unknown provider id: want NotFound, got %v", err)
	}
}

// ── UpsertLlmProvider validation ─────────────────────────────────────────────

func TestUpsertLlmProvider_BadURL(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, nil))

	p := validProvider()
	p.Endpoint = "not-a-url"
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.UpsertLlmProvider(ctx, &brokerv1.UpsertLlmProviderRequest{Provider: p})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad URL: want InvalidArgument, got %v", err)
	}
}

func TestUpsertLlmProvider_NonHttpScheme(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, nil))

	p := validProvider()
	p.Endpoint = "ftp://example.com/api"
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.UpsertLlmProvider(ctx, &brokerv1.UpsertLlmProviderRequest{Provider: p})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("non-http scheme: want InvalidArgument, got %v", err)
	}
}

func TestUpsertLlmProvider_EmptyModels(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, nil))

	p := validProvider()
	p.Models = nil
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.UpsertLlmProvider(ctx, &brokerv1.UpsertLlmProviderRequest{Provider: p})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty models: want InvalidArgument, got %v", err)
	}
}

func TestUpsertLlmProvider_NegativePrice(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, nil))

	p := validProvider()
	p.Models[0].PriceIn = -0.001
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.UpsertLlmProvider(ctx, &brokerv1.UpsertLlmProviderRequest{Provider: p})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("negative price: want InvalidArgument, got %v", err)
	}
}

func TestUpsertLlmProvider_UnknownAPI(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, nil))

	p := validProvider()
	p.Api = "unknown-api"
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.UpsertLlmProvider(ctx, &brokerv1.UpsertLlmProviderRequest{Provider: p})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown api: want InvalidArgument, got %v", err)
	}
}

func TestUpsertLlmProvider_EmptyID(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, nil))

	p := validProvider()
	p.Id = ""
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.UpsertLlmProvider(ctx, &brokerv1.UpsertLlmProviderRequest{Provider: p})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty id: want InvalidArgument, got %v", err)
	}
}

// ── UpsertLlmProvider key-write + has_key ─────────────────────────────────────

func TestUpsertLlmProvider_WritesKeyToSecretsAndSetsHasKey(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	sec := newCapturingSecrets()
	deps := testProviderDeps(t, srv.URL, sec)
	deps.Providers = newFakeLlmProviderStore()
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.UpsertLlmProvider(ctx, &brokerv1.UpsertLlmProviderRequest{
		Provider: validProvider(),
		ApiKey:   "sk-test-key",
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	key, ok := sec.writtenKeys[testTenantUUID+"/openrouter"]
	if !ok || key != "sk-test-key" {
		t.Errorf("expected key written to secrets, got %+v", sec.writtenKeys)
	}
	// has_key must be true on the stored row
	stored := deps.Providers.(*fakeLlmProviderStore)
	if row, exists := stored.rows[testTenantUUID+"/openrouter"]; !exists || !row.hasKey {
		t.Errorf("stored row has_key=false, want true: %+v", stored.rows)
	}
}

// ── UpsertLlmProvider carries vision fields through ───────────────────────────

func TestUpsertLlmProvider_CarriesVisionCapableThrough(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	deps := testProviderDeps(t, srv.URL, nil)
	deps.Providers = newFakeLlmProviderStore()
	svc := NewBrokerService(deps)

	p := validProvider()
	p.VisionCapable = true
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	if _, err := svc.UpsertLlmProvider(ctx, &brokerv1.UpsertLlmProviderRequest{Provider: p}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	listResp, err := svc.ListLlmProviders(ctx, &brokerv1.ListLlmProvidersRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found *brokerv1.LlmProvider
	for _, r := range listResp.Providers {
		if r.Id == p.Id {
			found = r
		}
	}
	if found == nil {
		t.Fatal("provider not found in list after upsert")
	}
	if !found.VisionCapable {
		t.Error("vision_capable not carried through Upsert→List round trip")
	}
}

// ── validateProvider: catalog rules ───────────────────────────────────────────

// TestValidateProvider_CatalogRules exercises validateProvider directly (it is
// pure), one case per rule the catalog added.
func TestValidateProvider_CatalogRules(t *testing.T) {
	// with applies mutations to a valid provider so each case names only its own delta.
	with := func(mut func(*brokerv1.LlmProvider)) *brokerv1.LlmProvider {
		p := validProvider()
		mut(p)
		return p
	}
	for _, c := range []struct {
		name    string
		p       *brokerv1.LlmProvider
		wantErr bool
	}{
		{"baseline valid", validProvider(), false},
		{"google-gemini family accepted", with(func(p *brokerv1.LlmProvider) {
			p.Api = apiGoogleGemini
		}), false},
		{"aws-bedrock family accepted", with(func(p *brokerv1.LlmProvider) {
			p.Api = apiAWSBedrock
		}), false},
		{"empty mode means chat", with(func(p *brokerv1.LlmProvider) {
			p.Models[0].Mode = ""
		}), false},
		{"known mode accepted", with(func(p *brokerv1.LlmProvider) {
			p.Models[0].Mode = modeEmbedding
		}), false},
		{"unknown mode rejected", with(func(p *brokerv1.LlmProvider) {
			p.Models[0].Mode = "telepathy"
		}), true},
		{"known pricing unit accepted", with(func(p *brokerv1.LlmProvider) {
			p.Models[0].Pricing = &brokerv1.LlmModelPricing{Unit: "per_page", InMicros: 3000}
		}), false},
		{"free unit accepted with no amounts", with(func(p *brokerv1.LlmProvider) {
			p.Models[0].Pricing = &brokerv1.LlmModelPricing{Unit: "free"}
		}), false},
		{"unknown pricing unit rejected", with(func(p *brokerv1.LlmProvider) {
			p.Models[0].Pricing = &brokerv1.LlmModelPricing{Unit: "per_vibe"}
		}), true},
		{"negative pricing amount rejected", with(func(p *brokerv1.LlmProvider) {
			p.Models[0].Pricing = &brokerv1.LlmModelPricing{Unit: "per_mtok", InMicros: -1}
		}), true},
		{"ascending tiers on per_mtok accepted", with(func(p *brokerv1.LlmProvider) {
			p.Models[0].Pricing = &brokerv1.LlmModelPricing{Unit: "per_mtok", InMicros: 1, Tiers: []*brokerv1.LlmPricingTier{
				{MinContextTokens: 128000, InMicros: 2}, {MinContextTokens: 200000, InMicros: 3},
			}}
		}), false},
		{"tiers on a non-per_mtok unit rejected", with(func(p *brokerv1.LlmProvider) {
			p.Models[0].Pricing = &brokerv1.LlmModelPricing{Unit: "per_image", InMicros: 40000, Tiers: []*brokerv1.LlmPricingTier{
				{MinContextTokens: 200000, InMicros: 2},
			}}
		}), true},
		{"descending tiers rejected", with(func(p *brokerv1.LlmProvider) {
			p.Models[0].Pricing = &brokerv1.LlmModelPricing{Unit: "per_mtok", InMicros: 1, Tiers: []*brokerv1.LlmPricingTier{
				{MinContextTokens: 200000, InMicros: 3}, {MinContextTokens: 128000, InMicros: 2},
			}}
		}), true},
		{"zero tier threshold rejected", with(func(p *brokerv1.LlmProvider) {
			p.Models[0].Pricing = &brokerv1.LlmModelPricing{Unit: "per_mtok", InMicros: 1, Tiers: []*brokerv1.LlmPricingTier{
				{MinContextTokens: 0, InMicros: 2},
			}}
		}), true},
		{"region config accepted on bedrock", with(func(p *brokerv1.LlmProvider) {
			p.Api = apiAWSBedrock
			p.Config = map[string]string{"region": "eu-central-1"}
		}), false},
		{"region config rejected on openai", with(func(p *brokerv1.LlmProvider) {
			p.Config = map[string]string{"region": "eu-central-1"}
		}), true},
		{"unknown config key rejected on bedrock", with(func(p *brokerv1.LlmProvider) {
			p.Api = apiAWSBedrock
			p.Config = map[string]string{"regionn": "eu-central-1"}
		}), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := validateProvider(c.p)
			if c.wantErr && status.Code(err) != codes.InvalidArgument {
				t.Fatalf("want InvalidArgument, got %v", err)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("want accepted, got %v", err)
			}
		})
	}
}

// ── SetDefaultProviderFor ─────────────────────────────────────────────────────

// setDefaultForSvc wires an admin-gated service over a fake store seeded with
// one chat provider, one vision-capable provider and one embedding provider.
func setDefaultForSvc(t *testing.T) (*BrokerService, *fakeLlmProviderStore) {
	t.Helper()
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	t.Cleanup(srv.Close)

	store := newFakeLlmProviderStore()
	store.rows[testTenantUUID+"/chat-only"] = fakeLlmProviderRow{
		id: "chat-only", name: "Chat", endpoint: "https://c.test", api: "openai-completions",
		models: []db.Model{{ID: "gpt-4o", Mode: modeChat}},
	}
	store.rows[testTenantUUID+"/vision-p"] = fakeLlmProviderRow{
		id: "vision-p", name: "Vision", endpoint: "https://v.test", api: "openai-completions",
		visionCapable: true, models: []db.Model{{ID: "gpt-4o", Mode: modeChat}},
	}
	store.rows[testTenantUUID+"/embed-p"] = fakeLlmProviderRow{
		id: "embed-p", name: "Embed", endpoint: "https://e.test", api: "openai-completions",
		models: []db.Model{{ID: "text-embed-3", Mode: modeEmbedding}},
	}
	deps := testProviderDeps(t, srv.URL, nil)
	deps.Providers = store
	return NewBrokerService(deps), store
}

func TestSetDefaultProviderFor_NonAdminDenied(t *testing.T) {
	svc, _ := setDefaultForSvc(t)
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.SetDefaultProviderFor(ctx, &brokerv1.SetDefaultProviderForRequest{
		Capability: modeEmbedding, ProviderId: "embed-p",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin: want PermissionDenied, got %v", err)
	}
}

func TestSetDefaultProviderFor_SetsAndClears(t *testing.T) {
	svc, store := setDefaultForSvc(t)
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")

	if _, err := svc.SetDefaultProviderFor(ctx, &brokerv1.SetDefaultProviderForRequest{
		Capability: modeEmbedding, ProviderId: "embed-p",
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if store.defaults[modeEmbedding] != "embed-p" {
		t.Fatalf("defaults after set: %+v", store.defaults)
	}

	// An empty provider_id is the clear verb — the whole reason the new RPC
	// exists alongside the three legacy ones.
	if _, err := svc.SetDefaultProviderFor(ctx, &brokerv1.SetDefaultProviderForRequest{
		Capability: modeEmbedding,
	}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, ok := store.defaults[modeEmbedding]; ok {
		t.Errorf("defaults after clear: %+v", store.defaults)
	}
}

func TestSetDefaultProviderFor_UnknownCapability(t *testing.T) {
	svc, _ := setDefaultForSvc(t)
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.SetDefaultProviderFor(ctx, &brokerv1.SetDefaultProviderForRequest{
		Capability: "telepathy", ProviderId: "chat-only",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown capability: want InvalidArgument, got %v", err)
	}
}

// The vision fail-closed rule must hold on the generic RPC too, or it would be
// the way around the guard on SetDefaultVisionProvider.
func TestSetDefaultProviderFor_VisionRequiresVisionCapable(t *testing.T) {
	svc, store := setDefaultForSvc(t)
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.SetDefaultProviderFor(ctx, &brokerv1.SetDefaultProviderForRequest{
		Capability: db.CapabilityVision, ProviderId: "chat-only",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("text-only provider for vision: want InvalidArgument, got %v", err)
	}
	if _, ok := store.defaults[db.CapabilityVision]; ok {
		t.Error("a rejected vision default must not be written")
	}

	if _, err := svc.SetDefaultProviderFor(ctx, &brokerv1.SetDefaultProviderForRequest{
		Capability: db.CapabilityVision, ProviderId: "vision-p",
	}); err != nil {
		t.Fatalf("vision-capable provider for vision: %v", err)
	}
}

// A modality default naming a provider with no model of that mode would point a
// future consumer at a provider that cannot serve the request at all.
func TestSetDefaultProviderFor_ModalityRequiresMatchingModel(t *testing.T) {
	svc, store := setDefaultForSvc(t)
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")

	_, err := svc.SetDefaultProviderFor(ctx, &brokerv1.SetDefaultProviderForRequest{
		Capability: modeEmbedding, ProviderId: "chat-only",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("chat-only provider for embedding: want InvalidArgument, got %v", err)
	}
	if _, ok := store.defaults[modeEmbedding]; ok {
		t.Error("a rejected modality default must not be written")
	}
}

func TestSetDefaultProviderFor_UnknownProviderNotFound(t *testing.T) {
	svc, _ := setDefaultForSvc(t)
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.SetDefaultProviderFor(ctx, &brokerv1.SetDefaultProviderForRequest{
		Capability: db.CapabilityChat, ProviderId: "nope",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("unknown provider: want NotFound, got %v", err)
	}
}

// The three legacy RPCs keep their proto surface but must land in the same
// llm_provider_defaults rows — never the frozen boolean columns.
func TestLegacySetDefaultRpcs_WriteCapabilityDefaults(t *testing.T) {
	svc, store := setDefaultForSvc(t)
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")

	if _, err := svc.SetDefaultProvider(ctx, &brokerv1.SetDefaultProviderRequest{Id: "chat-only"}); err != nil {
		t.Fatalf("SetDefaultProvider: %v", err)
	}
	if _, err := svc.SetDefaultVisionProvider(ctx, &brokerv1.SetDefaultVisionProviderRequest{Id: "vision-p"}); err != nil {
		t.Fatalf("SetDefaultVisionProvider: %v", err)
	}
	if _, err := svc.SetFallbackProvider(ctx, &brokerv1.SetFallbackProviderRequest{Id: "vision-p"}); err != nil {
		t.Fatalf("SetFallbackProvider: %v", err)
	}

	for capability, want := range map[string]string{
		db.CapabilityChat:     "chat-only",
		db.CapabilityVision:   "vision-p",
		db.CapabilityFallback: "vision-p",
	} {
		if store.defaults[capability] != want {
			t.Errorf("defaults[%s] = %q, want %q (full map %+v)", capability, store.defaults[capability], want, store.defaults)
		}
	}
}

// An empty id has no meaning on the legacy RPCs (their contract has no clear
// verb), so it must stay an error rather than silently clearing the default.
func TestLegacySetDefaultRpcs_RejectEmptyId(t *testing.T) {
	svc, _ := setDefaultForSvc(t)
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	for _, call := range []struct {
		name string
		run  func() error
	}{
		{"SetDefaultProvider", func() error {
			_, err := svc.SetDefaultProvider(ctx, &brokerv1.SetDefaultProviderRequest{})
			return err
		}},
		{"SetDefaultVisionProvider", func() error {
			_, err := svc.SetDefaultVisionProvider(ctx, &brokerv1.SetDefaultVisionProviderRequest{})
			return err
		}},
		{"SetFallbackProvider", func() error {
			_, err := svc.SetFallbackProvider(ctx, &brokerv1.SetFallbackProviderRequest{})
			return err
		}},
	} {
		if got := status.Code(call.run()); got != codes.InvalidArgument {
			t.Errorf("%s with empty id: want InvalidArgument, got %v", call.name, got)
		}
	}
}

// ── ListLlmProviders carries the defaults map ─────────────────────────────────

func TestListLlmProviders_ReturnsDefaultsMap(t *testing.T) {
	svc, _ := setDefaultForSvc(t)
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	if _, err := svc.SetDefaultProviderFor(ctx, &brokerv1.SetDefaultProviderForRequest{
		Capability: modeEmbedding, ProviderId: "embed-p",
	}); err != nil {
		t.Fatalf("set: %v", err)
	}

	resp, err := svc.ListLlmProviders(ctx, &brokerv1.ListLlmProvidersRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if resp.Defaults[modeEmbedding] != "embed-p" {
		t.Errorf("defaults map: got %+v", resp.Defaults)
	}
}

// ── Upsert carries config + per-model mode/pricing ────────────────────────────

func TestUpsertLlmProvider_CarriesConfigAndModelPricing(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	deps := testProviderDeps(t, srv.URL, nil)
	store := newFakeLlmProviderStore()
	deps.Providers = store
	svc := NewBrokerService(deps)

	p := validProvider()
	p.Api = apiAWSBedrock
	p.Config = map[string]string{"region": "eu-central-1"}
	p.Models[0].Mode = modeChat
	p.Models[0].Pricing = &brokerv1.LlmModelPricing{
		Unit: "per_mtok", InMicros: 3_000_000, OutMicros: 15_000_000,
		Tiers: []*brokerv1.LlmPricingTier{{MinContextTokens: 200000, InMicros: 6_000_000}},
	}

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	if _, err := svc.UpsertLlmProvider(ctx, &brokerv1.UpsertLlmProviderRequest{Provider: p}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if len(store.upserts) != 1 {
		t.Fatalf("want 1 upsert, got %d", len(store.upserts))
	}
	row := store.upserts[0]
	if row.Config["region"] != "eu-central-1" {
		t.Errorf("config: got %+v", row.Config)
	}
	if len(row.Models) != 1 || row.Models[0].Pricing == nil {
		t.Fatalf("models: got %+v", row.Models)
	}
	if row.Models[0].Mode != modeChat || row.Models[0].Pricing.Unit != db.PricingUnitPerMTok {
		t.Errorf("mode/unit: got %q / %+v", row.Models[0].Mode, row.Models[0].Pricing)
	}
	if len(row.Models[0].Pricing.Tiers) != 1 || row.Models[0].Pricing.Tiers[0].MinContextTokens != 200000 {
		t.Errorf("tiers: got %+v", row.Models[0].Pricing.Tiers)
	}

	// …and back out through the north List mapper.
	resp, err := svc.ListLlmProviders(ctx, &brokerv1.ListLlmProvidersRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(resp.Providers) != 1 {
		t.Fatalf("want 1 provider, got %d", len(resp.Providers))
	}
	got := resp.Providers[0]
	if got.Config["region"] != "eu-central-1" {
		t.Errorf("list config: got %+v", got.Config)
	}
	if got.Models[0].Pricing.GetInMicros() != 3_000_000 {
		t.Errorf("list pricing: got %+v", got.Models[0].Pricing)
	}
}

// ── ListLlmProviders never returns api_key ────────────────────────────────────

func TestListLlmProviders_NeverReturnsApiKey(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	sec := newCapturingSecrets()
	deps := testProviderDeps(t, srv.URL, sec)
	store := newFakeLlmProviderStore()
	store.rows[testTenantUUID+"/openrouter"] = fakeLlmProviderRow{
		id: "openrouter", name: "OR", endpoint: "https://x.com", api: "openai-completions",
		hasKey: true,
	}
	deps.Providers = store
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.ListLlmProviders(ctx, &brokerv1.ListLlmProvidersRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, p := range resp.Providers {
		if p.ApiKey != "" {
			t.Errorf("api_key must never be returned on north List, got %q for %s", p.ApiKey, p.Id)
		}
		if !p.HasKey {
			t.Errorf("has_key should be true for %s (stored with has_key=true)", p.Id)
		}
	}
}
