package db

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// ── Pure-logic tests (no Postgres required) ───────────────────────────────────

func TestProvider_JSONRoundTrip(t *testing.T) {
	p := Provider{
		ID:       "openrouter",
		Name:     "OpenRouter",
		Endpoint: "https://openrouter.ai/api/v1",
		API:      "openai-completions",
		Models: []Model{
			{
				ID:              "anthropic/claude-sonnet-4.6",
				PriceIn:         0.003,
				PriceOut:        0.015,
				PriceCacheRead:  0.0003,
				PriceCacheWrite: 0.00375,
				ContextWindow:   200000,
				MaxTokens:       8192,
			},
		},
		Enabled:   true,
		IsDefault: true,
		HasKey:    true,
		UpdatedBy: "admin@example.com",
		UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	b, err := json.Marshal(p.Models)
	if err != nil {
		t.Fatalf("marshal models: %v", err)
	}
	var got []Model
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal models: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 model, got %d", len(got))
	}
	m := got[0]
	if m.ID != "anthropic/claude-sonnet-4.6" {
		t.Errorf("ID: got %q", m.ID)
	}
	if m.PriceIn != 0.003 {
		t.Errorf("PriceIn: got %v", m.PriceIn)
	}
	if m.PriceOut != 0.015 {
		t.Errorf("PriceOut: got %v", m.PriceOut)
	}
	if m.PriceCacheRead != 0.0003 {
		t.Errorf("PriceCacheRead: got %v", m.PriceCacheRead)
	}
	if m.PriceCacheWrite != 0.00375 {
		t.Errorf("PriceCacheWrite: got %v", m.PriceCacheWrite)
	}
	if m.ContextWindow != 200000 {
		t.Errorf("ContextWindow: got %d", m.ContextWindow)
	}
	if m.MaxTokens != 8192 {
		t.Errorf("MaxTokens: got %d", m.MaxTokens)
	}
}

// TestModel_CatalogJSONRoundTrip pins the JSONB wire shape of the catalog
// additions — mode, the unit-discriminated pricing record and its tiers. These
// keys are the contract between the Go structs and every row already in
// Postgres, so a rename here silently drops a stored price.
func TestModel_CatalogJSONRoundTrip(t *testing.T) {
	in := []Model{{
		ID:            "gpt-4o",
		Mode:          "chat",
		ContextWindow: 128000,
		MaxTokens:     16384,
		Pricing: &ModelPricing{
			Unit:             PricingUnitPerMTok,
			InMicros:         2_500_000,
			OutMicros:        10_000_000,
			CacheReadMicros:  1_250_000,
			CacheWriteMicros: 3_125_000,
			Tiers: []PricingTier{
				{MinContextTokens: 200000, InMicros: 5_000_000, OutMicros: 20_000_000},
			},
		},
	}}

	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"mode":"chat"`, `"pricing"`, `"unit":"per_mtok"`,
		`"in_micros":2500000`, `"out_micros":10000000`, `"cache_read_micros":1250000`,
		`"cache_write_micros":3125000`, `"tiers"`, `"min_context_tokens":200000`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("serialized model missing %s, got %s", key, b)
		}
	}

	var got []Model
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 1 || got[0].Pricing == nil {
		t.Fatalf("pricing lost in round trip: %+v", got)
	}
	if got[0].Mode != "chat" {
		t.Errorf("Mode: got %q", got[0].Mode)
	}
	if got[0].Pricing.Unit != PricingUnitPerMTok || got[0].Pricing.InMicros != 2_500_000 {
		t.Errorf("pricing: got %+v", got[0].Pricing)
	}
	if len(got[0].Pricing.Tiers) != 1 || got[0].Pricing.Tiers[0].MinContextTokens != 200000 {
		t.Errorf("tiers: got %+v", got[0].Pricing.Tiers)
	}
}

// A pre-catalog model row carries neither key; both must stay absent from the
// serialized form so an untouched row's JSONB does not churn on every save.
func TestModel_LegacyRowOmitsCatalogKeys(t *testing.T) {
	b, err := json.Marshal([]Model{{ID: "legacy", PriceIn: 0.003}})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"mode"`, `"pricing"`} {
		if strings.Contains(string(b), key) {
			t.Errorf("legacy model must omit %s, got %s", key, b)
		}
	}
}

func TestProvider_ZeroModels(t *testing.T) {
	b, err := json.Marshal([]Model{})
	if err != nil {
		t.Fatal(err)
	}
	var got []Model
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("want 0 models, got %d", len(got))
	}
}

func TestLlmProviderRepoConstructor(t *testing.T) {
	repo := NewLlmProviderRepo(nil, nil)
	if repo == nil {
		t.Fatal("NewLlmProviderRepo returned nil")
	}
}

// TestProviderCols_IncludesApiVersion guards the Azure api_version wiring at the
// column-list level so a future column reorder can't silently drop it from the
// SELECT used by List/Get (scanProvider scans it positionally).
func TestProviderCols_IncludesApiVersion(t *testing.T) {
	if !strings.Contains(providerCols, "api_version") {
		t.Errorf("providerCols must include api_version, got %q", providerCols)
	}
	// api_version sits between api and models in the scan order.
	if !strings.Contains(providerCols, "api, api_version, models") {
		t.Errorf("api_version must be positioned between api and models, got %q", providerCols)
	}
}

// TestProvider_APIVersionField confirms the struct carries the Azure
// api-version through round-trips at the Go level.
func TestProvider_APIVersionField(t *testing.T) {
	p := Provider{API: "azure-openai", APIVersion: "2024-08-01-preview"}
	if p.APIVersion != "2024-08-01-preview" {
		t.Errorf("APIVersion: got %q", p.APIVersion)
	}
}

// TestProviderCols_IncludesVisionFields guards the vision-routing wiring at
// the column-list level so a future column reorder can't silently drop
// vision_capable/is_default_vision from the SELECT used by List/Get
// (scanProvider scans it positionally).
func TestProviderCols_IncludesVisionFields(t *testing.T) {
	if !strings.Contains(providerCols, "vision_capable") {
		t.Errorf("providerCols must include vision_capable, got %q", providerCols)
	}
	if !strings.Contains(providerCols, "is_default_vision") {
		t.Errorf("providerCols must include is_default_vision, got %q", providerCols)
	}
	// vision_capable/is_default_vision sit right after is_default in scan order.
	if !strings.Contains(providerCols, "is_default, vision_capable, is_default_vision, is_fallback, has_key") {
		t.Errorf("vision fields must be positioned between is_default and has_key, got %q", providerCols)
	}
}

// TestProviderCols_IncludesFallback guards is_fallback at the column-list level
// so a future column reorder can't silently drop it from the SELECT used by
// List/Get (scanProvider scans it positionally).
func TestProviderCols_IncludesFallback(t *testing.T) {
	if !strings.Contains(providerCols, "is_fallback") {
		t.Errorf("providerCols must include is_fallback, got %q", providerCols)
	}
	if !strings.Contains(providerCols, "is_default_vision, is_fallback, has_key") {
		t.Errorf("is_fallback must be positioned between is_default_vision and has_key, got %q", providerCols)
	}
}

// TestProviderCols_IncludesConfig guards the family-specific config column at
// the column-list level: scanProvider scans it positionally as the last field,
// so a reorder that moves it would scan JSONB bytes into a timestamp.
func TestProviderCols_IncludesConfig(t *testing.T) {
	if !strings.Contains(providerCols, "config") {
		t.Errorf("providerCols must include config, got %q", providerCols)
	}
	if !strings.HasSuffix(providerCols, "updated_by, updated_at, config") {
		t.Errorf("config must be the last column, after updated_at, got %q", providerCols)
	}
}

// TestProvider_VisionFields confirms the struct carries the two new vision
// flags through at the Go level.
func TestProvider_VisionFields(t *testing.T) {
	p := Provider{VisionCapable: true, IsDefaultVision: true}
	if !p.VisionCapable {
		t.Error("VisionCapable: got false, want true")
	}
	if !p.IsDefaultVision {
		t.Error("IsDefaultVision: got false, want true")
	}
}

// ── DB-backed tests (require TEST_DATABASE_URL; skip when absent) ─────────────
//
// These tests are deliberately skipped when no Postgres is available so the
// suite stays green in environments without a running DB.  Run them locally
// with:
//
//	TEST_DATABASE_URL=postgres://aikonos:dev-password-change-me@localhost:5432/aikonos \
//	  go test ./broker/internal/db/... -run TestLlmProvider
//
// The test applies migrations 014/015 inline via a temp schema so it doesn't
// pollute the shared DB.  Because withTenant uses a session-level SET the pool
// must be a single-connection pool or we acquire/release carefully — we follow
// the same pattern as the repo itself (Acquire + Release per call).

// NOTE: Postgres-backed tests are verified in-cluster/compose via
// scripts/verify-llm-providers.sh (CP6).  This file covers pure-logic
// invariants that run anywhere.
//
// The vision-routing tests below reuse openTestPool (defined in
// workflows_test.go, same package) and skip when TEST_DATABASE_URL is unset.

// TestLlmProvider_VisionRoundTrip verifies vision_capable/is_default_vision
// survive an Upsert + Get round-trip.
func TestLlmProvider_VisionRoundTrip(t *testing.T) {
	pool := openTestPool(t)
	repo := NewLlmProviderRepo(pool, zap.NewNop())
	ctx := context.Background()
	tenant := uuid.New().String()

	p := Provider{
		ID:            "vision-provider",
		Name:          "Vision Provider",
		Endpoint:      "https://example.test/v1",
		API:           "openai-completions",
		Models:        []Model{{ID: "gpt-vision"}},
		Enabled:       true,
		VisionCapable: true,
		UpdatedBy:     "admin@example.com",
	}
	if err := repo.Upsert(ctx, tenant, p); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := repo.Get(ctx, tenant, p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.VisionCapable {
		t.Error("VisionCapable: got false after Upsert(true)")
	}
	if got.IsDefaultVision {
		t.Error("IsDefaultVision: got true, want false (Upsert must not touch it)")
	}
}

// TestLlmProvider_SetDefaultVision verifies SetDefaultVision clears any prior
// default-vision provider before setting the new one — mirroring SetDefault.
func TestLlmProvider_SetDefaultVision(t *testing.T) {
	pool := openTestPool(t)
	repo := NewLlmProviderRepo(pool, zap.NewNop())
	ctx := context.Background()
	tenant := uuid.New().String()

	first := Provider{ID: "vision-a", Name: "A", Endpoint: "https://a.test", API: "openai-completions", VisionCapable: true}
	second := Provider{ID: "vision-b", Name: "B", Endpoint: "https://b.test", API: "openai-completions", VisionCapable: true}
	if err := repo.Upsert(ctx, tenant, first); err != nil {
		t.Fatalf("Upsert first: %v", err)
	}
	if err := repo.Upsert(ctx, tenant, second); err != nil {
		t.Fatalf("Upsert second: %v", err)
	}

	if err := repo.SetDefaultVision(ctx, tenant, first.ID); err != nil {
		t.Fatalf("SetDefaultVision(first): %v", err)
	}
	if err := repo.SetDefaultVision(ctx, tenant, second.ID); err != nil {
		t.Fatalf("SetDefaultVision(second): %v", err)
	}

	gotFirst, err := repo.Get(ctx, tenant, first.ID)
	if err != nil {
		t.Fatalf("Get first: %v", err)
	}
	if gotFirst.IsDefaultVision {
		t.Error("first provider still marked default-vision after switching to second")
	}
	gotSecond, err := repo.Get(ctx, tenant, second.ID)
	if err != nil {
		t.Fatalf("Get second: %v", err)
	}
	if !gotSecond.IsDefaultVision {
		t.Error("second provider not marked default-vision after SetDefaultVision")
	}
}

// TestLlmProvider_SetDefaultVision_NotFound verifies the ErrProviderNotFound
// contract mirrors SetDefault's.
func TestLlmProvider_SetDefaultVision_NotFound(t *testing.T) {
	pool := openTestPool(t)
	repo := NewLlmProviderRepo(pool, zap.NewNop())
	ctx := context.Background()
	tenant := uuid.New().String()

	if err := repo.SetDefaultVision(ctx, tenant, "does-not-exist"); err != ErrProviderNotFound {
		t.Errorf("SetDefaultVision(missing id): got %v, want ErrProviderNotFound", err)
	}
}

// TestLlmProvider_SetFallback verifies SetFallback clears any prior fallback
// provider before setting the new one — mirroring SetDefaultVision.
func TestLlmProvider_SetFallback(t *testing.T) {
	pool := openTestPool(t)
	repo := NewLlmProviderRepo(pool, zap.NewNop())
	ctx := context.Background()
	tenant := uuid.New().String()

	first := Provider{ID: "fallback-a", Name: "A", Endpoint: "https://a.test", API: "openai-completions"}
	second := Provider{ID: "fallback-b", Name: "B", Endpoint: "https://b.test", API: "openai-completions"}
	if err := repo.Upsert(ctx, tenant, first); err != nil {
		t.Fatalf("Upsert first: %v", err)
	}
	if err := repo.Upsert(ctx, tenant, second); err != nil {
		t.Fatalf("Upsert second: %v", err)
	}

	if err := repo.SetFallback(ctx, tenant, first.ID); err != nil {
		t.Fatalf("SetFallback(first): %v", err)
	}
	if err := repo.SetFallback(ctx, tenant, second.ID); err != nil {
		t.Fatalf("SetFallback(second): %v", err)
	}

	gotFirst, err := repo.Get(ctx, tenant, first.ID)
	if err != nil {
		t.Fatalf("Get first: %v", err)
	}
	if gotFirst.IsFallback {
		t.Error("first provider still marked fallback after switching to second")
	}
	gotSecond, err := repo.Get(ctx, tenant, second.ID)
	if err != nil {
		t.Fatalf("Get second: %v", err)
	}
	if !gotSecond.IsFallback {
		t.Error("second provider not marked fallback after SetFallback")
	}
}

// TestLlmProvider_SetFallback_NotFound verifies the ErrProviderNotFound
// contract mirrors SetDefaultVision's.
func TestLlmProvider_SetFallback_NotFound(t *testing.T) {
	pool := openTestPool(t)
	repo := NewLlmProviderRepo(pool, zap.NewNop())
	ctx := context.Background()
	tenant := uuid.New().String()

	if err := repo.SetFallback(ctx, tenant, "does-not-exist"); err != ErrProviderNotFound {
		t.Errorf("SetFallback(missing id): got %v, want ErrProviderNotFound", err)
	}
}

// setFlagDirect flips one of the three frozen boolean columns by raw UPDATE.
// Migration 046 stopped the repo from writing them, so a test that needs a
// legacy-shaped row (or wants to prove the partial unique index still guards
// them) has to reach past the repo.
func setFlagDirect(t *testing.T, pool *pgxpool.Pool, tenant, id, column string) error {
	t.Helper()
	ctx := context.Background()
	return withConnErr(ctx, pool, tenant, func(conn *pgxpool.Conn) error {
		_, err := conn.Exec(ctx,
			`UPDATE llm_providers SET `+column+`=true WHERE tenant_id=$1 AND id=$2`, tenant, id)
		return err
	})
}

// TestLlmProvider_OneFallbackPerTenant verifies the frozen is_fallback column's
// partial unique index still rejects a second true row for the same tenant.
// Both writes are raw UPDATEs: the repo no longer touches the column at all
// (migration 046), so the index is now purely a schema-level guard on legacy
// data until a cleanup migration drops it.
func TestLlmProvider_OneFallbackPerTenant(t *testing.T) {
	pool := openTestPool(t)
	repo := NewLlmProviderRepo(pool, zap.NewNop())
	ctx := context.Background()
	tenant := uuid.New().String()

	first := Provider{ID: "fallback-a", Name: "A", Endpoint: "https://a.test", API: "openai-completions"}
	second := Provider{ID: "fallback-b", Name: "B", Endpoint: "https://b.test", API: "openai-completions"}
	if err := repo.Upsert(ctx, tenant, first); err != nil {
		t.Fatalf("Upsert first: %v", err)
	}
	if err := repo.Upsert(ctx, tenant, second); err != nil {
		t.Fatalf("Upsert second: %v", err)
	}
	if err := setFlagDirect(t, pool, tenant, first.ID, "is_fallback"); err != nil {
		t.Fatalf("seed first is_fallback: %v", err)
	}

	err := setFlagDirect(t, pool, tenant, second.ID, "is_fallback")
	if err == nil {
		t.Fatal("direct UPDATE setting a second is_fallback row succeeded, want unique-constraint violation")
	}
	if !strings.Contains(err.Error(), "idx_llm_providers_one_fallback") {
		t.Errorf("expected unique-index violation naming idx_llm_providers_one_fallback, got: %v", err)
	}
}

// TestLlmProvider_OneDefaultVisionPerTenant is the is_default_vision twin of
// TestLlmProvider_OneFallbackPerTenant — same frozen-column rationale.
func TestLlmProvider_OneDefaultVisionPerTenant(t *testing.T) {
	pool := openTestPool(t)
	repo := NewLlmProviderRepo(pool, zap.NewNop())
	ctx := context.Background()
	tenant := uuid.New().String()

	first := Provider{ID: "vision-a", Name: "A", Endpoint: "https://a.test", API: "openai-completions", VisionCapable: true}
	second := Provider{ID: "vision-b", Name: "B", Endpoint: "https://b.test", API: "openai-completions", VisionCapable: true}
	if err := repo.Upsert(ctx, tenant, first); err != nil {
		t.Fatalf("Upsert first: %v", err)
	}
	if err := repo.Upsert(ctx, tenant, second); err != nil {
		t.Fatalf("Upsert second: %v", err)
	}
	if err := setFlagDirect(t, pool, tenant, first.ID, "is_default_vision"); err != nil {
		t.Fatalf("seed first is_default_vision: %v", err)
	}

	err := setFlagDirect(t, pool, tenant, second.ID, "is_default_vision")
	if err == nil {
		t.Fatal("direct UPDATE setting a second is_default_vision row succeeded, want unique-constraint violation")
	}
	if !strings.Contains(err.Error(), "idx_llm_providers_one_default_vision") {
		t.Errorf("expected unique-index violation naming idx_llm_providers_one_default_vision, got: %v", err)
	}
}

// ── llm_provider_defaults (migration 046) ─────────────────────────────────────

// TestLlmProvider_SetDefaultFor covers the three write outcomes: first set,
// re-point (upsert on the primary key), and clear.
func TestLlmProvider_SetDefaultFor(t *testing.T) {
	pool := openTestPool(t)
	repo := NewLlmProviderRepo(pool, zap.NewNop())
	ctx := context.Background()
	tenant := uuid.New().String()

	for _, id := range []string{"embed-a", "embed-b"} {
		if err := repo.Upsert(ctx, tenant, Provider{
			ID: id, Name: id, Endpoint: "https://" + id + ".test", API: "openai-completions",
			Models: []Model{{ID: id + "-m", Mode: "embedding"}},
		}); err != nil {
			t.Fatalf("Upsert %s: %v", id, err)
		}
	}

	if err := repo.SetDefaultFor(ctx, tenant, "embedding", "embed-a"); err != nil {
		t.Fatalf("SetDefaultFor(embed-a): %v", err)
	}
	if got, _ := repo.DefaultsFor(ctx, tenant); got["embedding"] != "embed-a" {
		t.Errorf("after first set: got %+v", got)
	}

	// Re-pointing must replace the row, not add a second one for the capability.
	if err := repo.SetDefaultFor(ctx, tenant, "embedding", "embed-b"); err != nil {
		t.Fatalf("SetDefaultFor(embed-b): %v", err)
	}
	got, err := repo.DefaultsFor(ctx, tenant)
	if err != nil {
		t.Fatalf("DefaultsFor: %v", err)
	}
	if got["embedding"] != "embed-b" || len(got) != 1 {
		t.Errorf("after re-point: got %+v, want exactly {embedding: embed-b}", got)
	}

	if err := repo.SetDefaultFor(ctx, tenant, "embedding", ""); err != nil {
		t.Fatalf("SetDefaultFor(clear): %v", err)
	}
	if got, _ := repo.DefaultsFor(ctx, tenant); len(got) != 0 {
		t.Errorf("after clear: got %+v, want empty", got)
	}
}

// A default naming a provider that does not exist would leave a capability
// pointing at nothing; the composite FK is what makes that impossible, and
// ErrProviderNotFound is how the RPC layer sees it.
func TestLlmProvider_SetDefaultFor_UnknownProvider(t *testing.T) {
	pool := openTestPool(t)
	repo := NewLlmProviderRepo(pool, zap.NewNop())
	ctx := context.Background()
	tenant := uuid.New().String()

	if err := repo.SetDefaultFor(ctx, tenant, CapabilityChat, "does-not-exist"); err != ErrProviderNotFound {
		t.Errorf("SetDefaultFor(missing provider): got %v, want ErrProviderNotFound", err)
	}
}

// TestLlmProvider_ListGetOverlayDefaults proves the three legacy booleans are
// computed from llm_provider_defaults on both read paths, in both directions:
// set through the legacy method, read back true; clear, read back false — even
// though the frozen columns themselves never change.
func TestLlmProvider_ListGetOverlayDefaults(t *testing.T) {
	pool := openTestPool(t)
	repo := NewLlmProviderRepo(pool, zap.NewNop())
	ctx := context.Background()
	tenant := uuid.New().String()

	p := Provider{
		ID: "overlay-a", Name: "A", Endpoint: "https://a.test", API: "openai-completions",
		Models: []Model{{ID: "m"}}, VisionCapable: true,
	}
	if err := repo.Upsert(ctx, tenant, p); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	for _, set := range []func(context.Context, string, string) error{
		repo.SetDefault, repo.SetDefaultVision, repo.SetFallback,
	} {
		if err := set(ctx, tenant, p.ID); err != nil {
			t.Fatalf("set default: %v", err)
		}
	}

	got, err := repo.Get(ctx, tenant, p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.IsDefault || !got.IsDefaultVision || !got.IsFallback {
		t.Errorf("Get overlay: got default=%v vision=%v fallback=%v, want all true",
			got.IsDefault, got.IsDefaultVision, got.IsFallback)
	}
	rows, err := repo.List(ctx, tenant)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || !rows[0].IsDefault || !rows[0].IsDefaultVision || !rows[0].IsFallback {
		t.Errorf("List overlay: got %+v", rows)
	}

	// Clearing must show up as false — a stale frozen column must never leak
	// back through as a live default.
	for _, capability := range []string{CapabilityChat, CapabilityVision, CapabilityFallback} {
		if err := repo.SetDefaultFor(ctx, tenant, capability, ""); err != nil {
			t.Fatalf("clear %s: %v", capability, err)
		}
	}
	got, err = repo.Get(ctx, tenant, p.ID)
	if err != nil {
		t.Fatalf("Get after clear: %v", err)
	}
	if got.IsDefault || got.IsDefaultVision || got.IsFallback {
		t.Errorf("after clear: got default=%v vision=%v fallback=%v, want all false",
			got.IsDefault, got.IsDefaultVision, got.IsFallback)
	}
}

// TestLlmProvider_CatalogRoundTrip pins the new persisted state end-to-end
// through Postgres: the config column plus mode/pricing/tiers inside models.
func TestLlmProvider_CatalogRoundTrip(t *testing.T) {
	pool := openTestPool(t)
	repo := NewLlmProviderRepo(pool, zap.NewNop())
	ctx := context.Background()
	tenant := uuid.New().String()

	p := Provider{
		ID: "bedrock", Name: "Bedrock", API: "aws-bedrock",
		Endpoint: "https://bedrock-runtime.eu-central-1.amazonaws.com/openai/v1",
		Config:   map[string]string{"region": "eu-central-1"},
		Models: []Model{{
			ID: "us.anthropic.claude-sonnet-4-6", Mode: "chat", ContextWindow: 200000,
			Pricing: &ModelPricing{
				Unit: PricingUnitPerMTok, InMicros: 3_000_000, OutMicros: 15_000_000,
				Tiers: []PricingTier{{MinContextTokens: 200000, InMicros: 6_000_000, OutMicros: 22_500_000}},
			},
		}},
	}
	if err := repo.Upsert(ctx, tenant, p); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := repo.Get(ctx, tenant, p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Config["region"] != "eu-central-1" {
		t.Errorf("config: got %+v", got.Config)
	}
	if len(got.Models) != 1 || got.Models[0].Mode != "chat" || got.Models[0].Pricing == nil {
		t.Fatalf("models: got %+v", got.Models)
	}
	pr := got.Models[0].Pricing
	if pr.Unit != PricingUnitPerMTok || pr.InMicros != 3_000_000 || pr.OutMicros != 15_000_000 {
		t.Errorf("pricing: got %+v", pr)
	}
	if len(pr.Tiers) != 1 || pr.Tiers[0].MinContextTokens != 200000 || pr.Tiers[0].InMicros != 6_000_000 {
		t.Errorf("tiers: got %+v", pr.Tiers)
	}
}

// A provider saved with no config must read back usable — the column is NOT
// NULL, so a nil map has to land as {} rather than SQL null.
func TestLlmProvider_NilConfigRoundTrip(t *testing.T) {
	pool := openTestPool(t)
	repo := NewLlmProviderRepo(pool, zap.NewNop())
	ctx := context.Background()
	tenant := uuid.New().String()

	p := Provider{ID: "no-config", Name: "N", Endpoint: "https://n.test", API: "openai-completions"}
	if err := repo.Upsert(ctx, tenant, p); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, err := repo.Get(ctx, tenant, p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Config) != 0 {
		t.Errorf("config: got %+v, want empty", got.Config)
	}
}
