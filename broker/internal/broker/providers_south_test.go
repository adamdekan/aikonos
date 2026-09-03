package broker

import (
	"context"
	"sync"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/metrics"
	"github.com/adamdekan/aikonos/broker/internal/secrets"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ── fakeLlmProviderStore — injectable LlmProvider repo for handler tests ──────

// fakeLlmProviderRow holds just the fields the handlers care about.
type fakeLlmProviderRow struct {
	id, name, endpoint, api string
	hasKey                  bool
	isDefault               bool
	visionCapable           bool
	isDefaultVision         bool
	isFallback              bool
	priceInMicrosPerMTok    int64
	priceOutMicrosPerMTok   int64
	models                  []db.Model
	config                  map[string]string
}

// modelsOrOne mirrors the real schema's guarantee that a stored provider always
// has at least one model (validateProvider rejects an empty list at Upsert), so
// capability-eligibility checks see a realistic row unless a test says otherwise.
func (r fakeLlmProviderRow) modelsOrOne() []db.Model {
	if r.models != nil {
		return r.models
	}
	return []db.Model{{ID: r.id + "-model"}}
}

type fakeLlmProviderStore struct {
	mu   sync.Mutex
	rows map[string]fakeLlmProviderRow // "tenant/id" → row
	// defaults mirrors llm_provider_defaults: capability → provider id.
	defaults map[string]string
	upserts  []db.Provider
	deletes  []string
}

func newFakeLlmProviderStore() *fakeLlmProviderStore {
	return &fakeLlmProviderStore{
		rows:     map[string]fakeLlmProviderRow{},
		defaults: map[string]string{},
	}
}

func (f *fakeLlmProviderStore) List(_ context.Context, tenant string) ([]db.Provider, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []db.Provider
	for k, r := range f.rows {
		if len(k) <= len(tenant)+1 || k[:len(tenant)] != tenant {
			continue
		}
		out = append(out, db.Provider{
			ID:              r.id,
			Name:            r.name,
			Endpoint:        r.endpoint,
			API:             r.api,
			HasKey:          r.hasKey,
			IsDefault:       r.isDefault,
			VisionCapable:   r.visionCapable,
			IsDefaultVision: r.isDefaultVision,
			IsFallback:      r.isFallback,
			Models:          r.modelsOrOne(),
			Config:          r.config,
		})
	}
	return out, nil
}

func (f *fakeLlmProviderStore) Get(_ context.Context, tenant, id string) (db.Provider, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[tenant+"/"+id]
	if !ok {
		return db.Provider{}, db.ErrProviderNotFound
	}
	return db.Provider{
		ID: r.id, Name: r.name, Endpoint: r.endpoint, API: r.api, HasKey: r.hasKey, IsDefault: r.isDefault,
		VisionCapable: r.visionCapable, IsDefaultVision: r.isDefaultVision, IsFallback: r.isFallback,
		Models: r.modelsOrOne(), Config: r.config,
		PriceInMicrosPerMTok: r.priceInMicrosPerMTok, PriceOutMicrosPerMTok: r.priceOutMicrosPerMTok,
	}, nil
}

func (f *fakeLlmProviderStore) Upsert(_ context.Context, tenant string, p db.Provider) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Preserve is_default_vision/is_fallback across an Upsert — mirrors the real
	// repo's Upsert, which does not touch is_default/is_default_vision/is_fallback.
	existing := f.rows[tenant+"/"+p.ID]
	f.rows[tenant+"/"+p.ID] = fakeLlmProviderRow{
		id: p.ID, name: p.Name, endpoint: p.Endpoint, api: p.API,
		hasKey: p.HasKey, isDefault: p.IsDefault,
		visionCapable: p.VisionCapable, isDefaultVision: existing.isDefaultVision,
		isFallback: existing.isFallback,
		models:     p.Models, config: p.Config,
	}
	f.upserts = append(f.upserts, p)
	return nil
}

func (f *fakeLlmProviderStore) Delete(_ context.Context, _, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, id)
	return nil
}

// SetDefaultFor mirrors the real repo: one row per capability, plus the boolean
// view List/Get recompute for chat/vision/fallback.
func (f *fakeLlmProviderStore) SetDefaultFor(_ context.Context, tenant, capability, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id == "" {
		delete(f.defaults, capability)
	} else {
		if _, ok := f.rows[tenant+"/"+id]; !ok {
			return db.ErrProviderNotFound
		}
		f.defaults[capability] = id
	}
	want := tenant + "/" + id
	for k, r := range f.rows {
		on := id != "" && k == want
		switch capability {
		case db.CapabilityChat:
			r.isDefault = on
		case db.CapabilityVision:
			r.isDefaultVision = on
		case db.CapabilityFallback:
			r.isFallback = on
		}
		f.rows[k] = r
	}
	return nil
}

func (f *fakeLlmProviderStore) DefaultsFor(_ context.Context, _ string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.defaults))
	for k, v := range f.defaults {
		out[k] = v
	}
	return out, nil
}

func (f *fakeLlmProviderStore) SetDefault(ctx context.Context, tenant, id string) error {
	return f.SetDefaultFor(ctx, tenant, db.CapabilityChat, id)
}

func (f *fakeLlmProviderStore) SetDefaultVision(ctx context.Context, tenant, id string) error {
	return f.SetDefaultFor(ctx, tenant, db.CapabilityVision, id)
}

func (f *fakeLlmProviderStore) SetFallback(ctx context.Context, tenant, id string) error {
	return f.SetDefaultFor(ctx, tenant, db.CapabilityFallback, id)
}

// ── fakeUsageRecorder ─────────────────────────────────────────────────────────

type fakeUsageRecorder struct {
	mu    sync.Mutex
	calls []metrics.UsageAttrs
}

func (f *fakeUsageRecorder) RecordUsage(_ context.Context, a metrics.UsageAttrs) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, a)
}

func (f *fakeUsageRecorder) last() (metrics.UsageAttrs, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return metrics.UsageAttrs{}, false
	}
	return f.calls[len(f.calls)-1], true
}

// ── helper: SandboxService for south tests ────────────────────────────────────

func newSouthSvc(t *testing.T, sec secrets.Provider, store *fakeLlmProviderStore, rec metrics.UsageRecorder) *SandboxService {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	deps := Deps{
		Logger:          zap.NewNop(),
		Audit:           em,
		GatewaySpiffeID: testGateway,
		TenantID:        testTenant,
		Secrets:         sec,
		Providers:       store,
		UsageMetrics:    rec,
	}
	return NewSandboxService(deps)
}

// ── requireGatewayPeer tests ──────────────────────────────────────────────────

func TestGetLlmProviders_RequiresGatewayPeer(t *testing.T) {
	svc := newSouthSvc(t, newCapturingSecrets(), newFakeLlmProviderStore(), nil)
	_, err := svc.GetLlmProviders(context.Background(), &brokerv1.GetLlmProvidersRequest{TenantId: testTenant})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("no peer: want PermissionDenied, got %v", err)
	}
}

func TestGetLlmProviders_WrongPeerDenied(t *testing.T) {
	svc := newSouthSvc(t, newCapturingSecrets(), newFakeLlmProviderStore(), nil)
	ctx := gatewayCtx("spiffe://aikonos.com/some-other-svc")
	_, err := svc.GetLlmProviders(ctx, &brokerv1.GetLlmProvidersRequest{TenantId: testTenant})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("wrong peer: want PermissionDenied, got %v", err)
	}
}

func TestEmitLlmUsage_RequiresGatewayPeer(t *testing.T) {
	svc := newSouthSvc(t, newCapturingSecrets(), newFakeLlmProviderStore(), nil)
	_, err := svc.EmitLlmUsage(context.Background(), &brokerv1.EmitLlmUsageRequest{TenantId: testTenant})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("no peer: want PermissionDenied, got %v", err)
	}
}

func TestEmitLlmUsage_WrongPeerDenied(t *testing.T) {
	svc := newSouthSvc(t, newCapturingSecrets(), newFakeLlmProviderStore(), nil)
	ctx := gatewayCtx("spiffe://aikonos.com/wrong")
	_, err := svc.EmitLlmUsage(ctx, &brokerv1.EmitLlmUsageRequest{TenantId: testTenant})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("wrong peer: want PermissionDenied, got %v", err)
	}
}

// ── EmitLlmUsage functional tests ────────────────────────────────────────────

func TestEmitLlmUsage_EmptyTenantRejected(t *testing.T) {
	svc := newSouthSvc(t, newCapturingSecrets(), newFakeLlmProviderStore(), nil)
	ctx := gatewayCtx(testGateway)
	_, err := svc.EmitLlmUsage(ctx, &brokerv1.EmitLlmUsageRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty tenant: want InvalidArgument, got %v", err)
	}
}

func TestEmitLlmUsage_RecordsMetricsAndAudit(t *testing.T) {
	rec := &fakeUsageRecorder{}
	svc := newSouthSvc(t, newCapturingSecrets(), newFakeLlmProviderStore(), rec)
	ctx := gatewayCtx(testGateway)

	_, err := svc.EmitLlmUsage(ctx, &brokerv1.EmitLlmUsageRequest{
		TenantId:  testTenant,
		UserId:    "alice@example.com",
		AgentId:   "agent-1",
		Provider:  "openrouter",
		Model:     "claude-sonnet-4-6",
		TokensIn:  100,
		TokensOut: 200,
		Cost:      0.0042,
	})
	if err != nil {
		t.Fatalf("EmitLlmUsage: %v", err)
	}

	got, ok := rec.last()
	if !ok {
		t.Fatal("expected RecordUsage to be called")
	}
	if got.Provider != "openrouter" || got.Model != "claude-sonnet-4-6" || got.TokensIn != 100 || got.TokensOut != 200 {
		t.Errorf("usage attrs wrong: %+v", got)
	}
}

// EmitLlmUsage is fire-and-forget for audit/metrics — must never return an error.
func TestEmitLlmUsage_NeverReturnsError(t *testing.T) {
	// nil recorder — must not panic or error
	svc := newSouthSvc(t, newCapturingSecrets(), newFakeLlmProviderStore(), nil)
	ctx := gatewayCtx(testGateway)
	_, err := svc.EmitLlmUsage(ctx, &brokerv1.EmitLlmUsageRequest{
		TenantId: testTenant,
		Provider: "openrouter",
		Model:    "m",
	})
	if err != nil {
		t.Fatalf("EmitLlmUsage must never return error, got %v", err)
	}
}

// ── GetLlmProviders functional ────────────────────────────────────────────────

func TestGetLlmProviders_ReturnsApiKey(t *testing.T) {
	sec := newCapturingSecrets()
	// pre-populate a key in the secrets fake via WriteProviderKey
	_ = sec.WriteProviderKey(context.Background(), testTenant, "openrouter", "sk-secret")

	store := newFakeLlmProviderStore()
	store.rows[testTenant+"/openrouter"] = fakeLlmProviderRow{
		id: "openrouter", name: "OR", endpoint: "https://x.com", api: "openai-completions", hasKey: true,
	}

	// Override ReadProviderKey to return our written value.
	readSec := &readingSecrets{written: sec.writtenKeys}
	svc := newSouthSvc(t, readSec, store, nil)

	ctx := gatewayCtx(testGateway)
	resp, err := svc.GetLlmProviders(ctx, &brokerv1.GetLlmProvidersRequest{TenantId: testTenant})
	if err != nil {
		t.Fatalf("GetLlmProviders: %v", err)
	}
	if len(resp.Providers) == 0 {
		t.Fatal("expected at least one provider")
	}
	found := false
	for _, p := range resp.Providers {
		if p.Id == "openrouter" {
			found = true
			if p.ApiKey != "sk-secret" {
				t.Errorf("expected api_key=sk-secret, got %q", p.ApiKey)
			}
		}
	}
	if !found {
		t.Error("openrouter provider not in response")
	}
}

// TestGetLlmProviders_CarriesVisionFieldsThrough verifies the south response
// includes vision_capable/is_default_vision, not just api_key/has_key.
func TestGetLlmProviders_CarriesVisionFieldsThrough(t *testing.T) {
	store := newFakeLlmProviderStore()
	store.rows[testTenant+"/vision-a"] = fakeLlmProviderRow{
		id: "vision-a", name: "A", endpoint: "https://a.test", api: "openai-completions",
		visionCapable: true, isDefaultVision: true,
	}
	svc := newSouthSvc(t, newCapturingSecrets(), store, nil)

	ctx := gatewayCtx(testGateway)
	resp, err := svc.GetLlmProviders(ctx, &brokerv1.GetLlmProvidersRequest{TenantId: testTenant})
	if err != nil {
		t.Fatalf("GetLlmProviders: %v", err)
	}
	var found *brokerv1.LlmProvider
	for _, p := range resp.Providers {
		if p.Id == "vision-a" {
			found = p
		}
	}
	if found == nil {
		t.Fatal("vision-a provider not in south response")
	}
	if !found.VisionCapable {
		t.Error("vision_capable not carried through south GetLlmProviders")
	}
	if !found.IsDefaultVision {
		t.Error("is_default_vision not carried through south GetLlmProviders")
	}
}

// TestGetLlmProviders_CarriesCatalogFieldsThrough verifies the south response
// carries mode / per-model pricing / config and the computed default booleans.
// The gateway picks its model and prices its calls off this response, so a field
// missing here is invisible until spend is already wrong.
func TestGetLlmProviders_CarriesCatalogFieldsThrough(t *testing.T) {
	store := newFakeLlmProviderStore()
	store.rows[testTenant+"/bedrock"] = fakeLlmProviderRow{
		id: "bedrock", name: "Bedrock", api: "aws-bedrock",
		endpoint: "https://bedrock-runtime.eu-central-1.amazonaws.com/openai/v1",
		config:   map[string]string{"region": "eu-central-1"},
		models: []db.Model{{ID: "claude", Mode: "chat", Pricing: &db.ModelPricing{
			Unit: db.PricingUnitPerMTok, InMicros: 3_000_000, OutMicros: 15_000_000,
			Tiers: []db.PricingTier{{MinContextTokens: 200000, InMicros: 6_000_000}},
		}}},
	}
	svc := newSouthSvc(t, newCapturingSecrets(), store, nil)

	// Set through the legacy chat capability; the response must show is_default.
	if err := store.SetDefault(context.Background(), testTenant, "bedrock"); err != nil {
		t.Fatalf("SetDefault: %v", err)
	}

	resp, err := svc.GetLlmProviders(gatewayCtx(testGateway), &brokerv1.GetLlmProvidersRequest{TenantId: testTenant})
	if err != nil {
		t.Fatalf("GetLlmProviders: %v", err)
	}
	var found *brokerv1.LlmProvider
	for _, p := range resp.Providers {
		if p.Id == "bedrock" {
			found = p
		}
	}
	if found == nil {
		t.Fatal("bedrock provider not in south response")
	}
	if found.Config["region"] != "eu-central-1" {
		t.Errorf("config not carried through south, got %+v", found.Config)
	}
	if len(found.Models) != 1 {
		t.Fatalf("models: got %+v", found.Models)
	}
	if found.Models[0].Mode != "chat" {
		t.Errorf("mode not carried through south, got %q", found.Models[0].Mode)
	}
	pr := found.Models[0].Pricing
	if pr == nil || pr.Unit != db.PricingUnitPerMTok || pr.InMicros != 3_000_000 {
		t.Fatalf("pricing not carried through south, got %+v", pr)
	}
	if len(pr.Tiers) != 1 || pr.Tiers[0].MinContextTokens != 200000 {
		t.Errorf("tiers not carried through south, got %+v", pr.Tiers)
	}
	if !found.IsDefault {
		t.Error("is_default must be computed from llm_provider_defaults on the south path too")
	}
}

// readingSecrets wraps capturingSecrets but actually reads back written keys.
type readingSecrets struct {
	capturingSecrets
	written map[string]string
}

func (r *readingSecrets) ReadProviderKey(_ context.Context, tenant, id string) (string, bool, error) {
	k := r.written[tenant+"/"+id]
	return k, k != "", nil
}
