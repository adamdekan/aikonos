package broker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ── costMicrosFor — pure function ────────────────────────────────────────────

func TestCostMicrosFor_PreferersRequestCost(t *testing.T) {
	req := &brokerv1.EmitLlmUsageRequest{Cost: 0.0042, TokensIn: 1000, TokensOut: 2000}
	provider := db.Provider{PriceInMicrosPerMTok: 999, PriceOutMicrosPerMTok: 999}
	got := costMicrosFor(req, provider)
	const want = int64(4200)
	if got != want {
		t.Errorf("costMicrosFor = %d, want %d (Pi cost preferred over token rate)", got, want)
	}
}

// TestCostMicrosFor_RoundsNotTruncates pins costMicrosFor to round-half-up
// rather than the prior int64(x*1e6) truncation, which drifts recorded spend
// down over many calls. Both cases hit the fractional-micro boundary that
// truncation gets wrong: a value with digits past the 6th decimal, and a
// sub-micro cost that plain truncation would zero out entirely.
func TestCostMicrosFor_RoundsNotTruncates(t *testing.T) {
	cases := []struct {
		name string
		cost float64
		want int64
	}{
		{"rounds up at the 7th decimal", 1.23456789, 1234568},
		{"tiny cost truncation would zero out", 0.0000006, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := &brokerv1.EmitLlmUsageRequest{Cost: c.cost}
			got := costMicrosFor(req, db.Provider{})
			if got != c.want {
				t.Errorf("costMicrosFor(%v) = %d, want %d", c.cost, got, c.want)
			}
		})
	}
}

func TestCostMicrosFor_TokenRateFallback(t *testing.T) {
	req := &brokerv1.EmitLlmUsageRequest{Cost: 0, TokensIn: 1_000_000, TokensOut: 500_000}
	provider := db.Provider{PriceInMicrosPerMTok: 3_000_000, PriceOutMicrosPerMTok: 15_000_000}
	got := costMicrosFor(req, provider)
	// 1M in tokens @ 3_000_000 micros/MTok = 3_000_000; 0.5M out tokens @ 15_000_000/MTok = 7_500_000
	want := int64(3_000_000 + 7_500_000)
	if got != want {
		t.Errorf("costMicrosFor fallback = %d, want %d", got, want)
	}
}

func TestCostMicrosFor_UnpricedProviderIsZero(t *testing.T) {
	req := &brokerv1.EmitLlmUsageRequest{Cost: 0, TokensIn: 1_000_000, TokensOut: 1_000_000}
	provider := db.Provider{} // zero rates
	got := costMicrosFor(req, provider)
	if got != 0 {
		t.Errorf("unpriced provider: costMicrosFor = %d, want 0", got)
	}
}

// ── costMicrosFor — per-model pricing ────────────────────────────────────────

// pricedProvider is a provider whose one model carries its own pricing record,
// alongside deliberately different flat provider rates so any test that reads
// the flat fallback instead of the model record produces a visibly wrong number.
func pricedProvider(pr *db.ModelPricing) db.Provider {
	return db.Provider{
		PriceInMicrosPerMTok:  999_000_000,
		PriceOutMicrosPerMTok: 999_000_000,
		Models:                []db.Model{{ID: "gpt-4o", Pricing: pr}},
	}
}

func TestCostMicrosFor_PerModelPricing(t *testing.T) {
	perMTok := &db.ModelPricing{
		Unit: db.PricingUnitPerMTok, InMicros: 3_000_000, OutMicros: 15_000_000,
		CacheReadMicros: 300_000, CacheWriteMicros: 3_750_000,
	}
	tiered := &db.ModelPricing{
		Unit: db.PricingUnitPerMTok, InMicros: 3_000_000, OutMicros: 15_000_000,
		Tiers: []db.PricingTier{
			{MinContextTokens: 200_000, InMicros: 6_000_000, OutMicros: 22_500_000},
			{MinContextTokens: 1_000_000, InMicros: 12_000_000, OutMicros: 45_000_000},
		},
	}
	for _, c := range []struct {
		name     string
		provider db.Provider
		req      *brokerv1.EmitLlmUsageRequest
		want     int64
	}{
		{
			// 1M in @3 + 0.5M out @15 = 3_000_000 + 7_500_000
			name: "per_mtok in/out", provider: pricedProvider(perMTok),
			req:  &brokerv1.EmitLlmUsageRequest{Model: "gpt-4o", TokensIn: 1_000_000, TokensOut: 500_000},
			want: 10_500_000,
		},
		{
			// cache tokens price at their own rates, not the input rate
			name: "per_mtok with cache tokens", provider: pricedProvider(perMTok),
			req: &brokerv1.EmitLlmUsageRequest{
				Model: "gpt-4o", TokensIn: 1_000_000, TokensOut: 0,
				CacheRead: 2_000_000, CacheWrite: 1_000_000,
			},
			want: 3_000_000 + 600_000 + 3_750_000,
		},
		{
			// input side (in + cache_read) = 100k → below the first tier
			name: "below the first tier keeps base rates", provider: pricedProvider(tiered),
			req:  &brokerv1.EmitLlmUsageRequest{Model: "gpt-4o", TokensIn: 100_000, TokensOut: 100_000},
			want: 300_000 + 1_500_000,
		},
		{
			// 250k input side crosses 200k, not 1M → middle tier
			name: "crossing a tier replaces in/out rates", provider: pricedProvider(tiered),
			req:  &brokerv1.EmitLlmUsageRequest{Model: "gpt-4o", TokensIn: 250_000, TokensOut: 100_000},
			want: 1_500_000 + 2_250_000,
		},
		{
			// cache_read counts toward the tier threshold: 150k + 100k = 250k
			name: "cache_read counts toward the tier threshold", provider: pricedProvider(tiered),
			req: &brokerv1.EmitLlmUsageRequest{
				Model: "gpt-4o", TokensIn: 150_000, TokensOut: 0, CacheRead: 100_000,
			},
			want: 900_000, // 150k in @6/MTok; no cache rate configured on the tiered model
		},
		{
			name: "highest matching tier wins", provider: pricedProvider(tiered),
			req:  &brokerv1.EmitLlmUsageRequest{Model: "gpt-4o", TokensIn: 2_000_000, TokensOut: 0},
			want: 24_000_000,
		},
		{
			name:     "free unit costs nothing",
			provider: pricedProvider(&db.ModelPricing{Unit: "free"}),
			req:      &brokerv1.EmitLlmUsageRequest{Model: "gpt-4o", TokensIn: 1_000_000, TokensOut: 1_000_000},
			want:     0,
		},
		{
			// A non-token unit has no quantity on the wire yet, so it costs 0 —
			// deliberately, not by falling back to the flat token rates.
			name:     "non-token unit costs nothing rather than falling back",
			provider: pricedProvider(&db.ModelPricing{Unit: "per_image", InMicros: 40_000}),
			req:      &brokerv1.EmitLlmUsageRequest{Model: "gpt-4o", TokensIn: 1_000_000},
			want:     0,
		},
		{
			// The model does not match, so the flat provider rate is the only basis.
			name:     "unknown model falls back to flat provider rates",
			provider: db.Provider{PriceInMicrosPerMTok: 2_000_000, Models: []db.Model{{ID: "other", Pricing: perMTok}}},
			req:      &brokerv1.EmitLlmUsageRequest{Model: "gpt-4o", TokensIn: 1_000_000},
			want:     2_000_000,
		},
		{
			// A model with no pricing record of its own is the pre-catalog shape.
			name:     "model without pricing falls back to flat provider rates",
			provider: db.Provider{PriceInMicrosPerMTok: 2_000_000, Models: []db.Model{{ID: "gpt-4o"}}},
			req:      &brokerv1.EmitLlmUsageRequest{Model: "gpt-4o", TokensIn: 1_000_000},
			want:     2_000_000,
		},
		{
			name:     "request cost still outranks model pricing",
			provider: pricedProvider(perMTok),
			req:      &brokerv1.EmitLlmUsageRequest{Model: "gpt-4o", Cost: 0.0042, TokensIn: 1_000_000},
			want:     4200,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := costMicrosFor(c.req, c.provider); got != c.want {
				t.Errorf("costMicrosFor = %d, want %d", got, c.want)
			}
		})
	}
}

// A model priced "free" costs 0 by design, so it must not trip the
// "provider has no per-token pricing" warning — that warning exists to catch a
// missing config, not a deliberate zero.
func TestRecordLlmUsage_ModelPricedFreeDoesNotWarn(t *testing.T) {
	core, recorded := observer.New(zapcore.WarnLevel)
	store := newFakeLlmProviderStore()
	tenant := "free-" + uuid.NewString()
	store.rows[tenant+"/selfhosted"] = fakeLlmProviderRow{
		id: "selfhosted", name: "Ollama", endpoint: "https://x.test", api: "openai-completions",
		models: []db.Model{{ID: "llama-4", Pricing: &db.ModelPricing{Unit: "free"}}},
	}
	deps := Deps{Logger: zap.New(core), Providers: store, SpendCounters: newFakeSpendCounterStore()}

	deps.recordLlmUsage(context.Background(), &brokerv1.EmitLlmUsageRequest{
		TenantId: tenant, Provider: "selfhosted", Model: "llama-4",
		TokensIn: 1_000_000, TokensOut: 1_000_000, Cost: 0,
	})

	if n := len(recorded.FilterMessageSnippet("no per-token pricing").All()); n != 0 {
		t.Errorf("a model priced free must not warn, got %d warnings", n)
	}
}

// ── EmitLlmUsage → accumulateLlmSpend integration ───────────────────────────

func newSouthSvcWithCounters(t *testing.T, store *fakeLlmProviderStore, counters *fakeSpendCounterStore) *SandboxService {
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
		Providers:       store,
		SpendCounters:   counters,
	}
	return NewSandboxService(deps)
}

func TestEmitLlmUsage_AccumulatesRequestCost(t *testing.T) {
	counters := newFakeSpendCounterStore()
	svc := newSouthSvcWithCounters(t, newFakeLlmProviderStore(), counters)
	ctx := gatewayCtx(testGateway)

	_, err := svc.EmitLlmUsage(ctx, &brokerv1.EmitLlmUsageRequest{
		TenantId: testTenant, UserId: "alice", AgentId: "agent-1",
		Provider: "openrouter", Model: "m", TokensIn: 10, TokensOut: 20, Cost: 0.001,
	})
	if err != nil {
		t.Fatalf("EmitLlmUsage: %v", err)
	}
	waitFor(t, func() bool {
		counters.mu.Lock()
		defer counters.mu.Unlock()
		return len(counters.accumulateCalls) > 0
	})
	counters.mu.Lock()
	call := counters.accumulateCalls[0]
	counters.mu.Unlock()
	const wantCost = int64(1000)
	if call.costMicros != wantCost || call.userID != "alice" || call.agentID != "agent-1" || call.tenant != testTenant {
		t.Errorf("accumulate call wrong: %+v, want costMicros=%d", call, wantCost)
	}
}

func TestEmitLlmUsage_AccumulatesTokenRateFallbackWhenNoCost(t *testing.T) {
	store := newFakeLlmProviderStore()
	store.rows[testTenant+"/priced"] = fakeLlmProviderRow{
		id: "priced", name: "P", endpoint: "https://x.test", api: "openai-completions",
		priceInMicrosPerMTok: 2_000_000, priceOutMicrosPerMTok: 4_000_000,
	}
	counters := newFakeSpendCounterStore()
	svc := newSouthSvcWithCounters(t, store, counters)
	ctx := gatewayCtx(testGateway)

	_, err := svc.EmitLlmUsage(ctx, &brokerv1.EmitLlmUsageRequest{
		TenantId: testTenant, Provider: "priced", Model: "m",
		TokensIn: 1_000_000, TokensOut: 1_000_000, Cost: 0,
	})
	if err != nil {
		t.Fatalf("EmitLlmUsage: %v", err)
	}
	waitFor(t, func() bool {
		counters.mu.Lock()
		defer counters.mu.Unlock()
		return len(counters.accumulateCalls) > 0
	})
	counters.mu.Lock()
	call := counters.accumulateCalls[0]
	counters.mu.Unlock()
	want := int64(2_000_000 + 4_000_000) // 1M tokens each way at the given per-MTok rates
	if call.costMicros != want {
		t.Errorf("fallback accumulate cost = %d, want %d", call.costMicros, want)
	}
}

// The spend write used to run in a detached goroutine, so a process death
// between the RPC returning and the write landing silently dropped the record.
// It is synchronous now: by the time EmitLlmUsage returns, the accumulate has
// been attempted — no polling needed.
func TestEmitLlmUsage_AccumulatesSynchronously(t *testing.T) {
	counters := newFakeSpendCounterStore()
	svc := newSouthSvcWithCounters(t, newFakeLlmProviderStore(), counters)

	_, err := svc.EmitLlmUsage(gatewayCtx(testGateway), &brokerv1.EmitLlmUsageRequest{
		TenantId: testTenant, UserId: "alice", Provider: "openrouter", Model: "m",
		TokensIn: 10, TokensOut: 20, Cost: 0.002,
	})
	if err != nil {
		t.Fatalf("EmitLlmUsage: %v", err)
	}
	counters.mu.Lock()
	n := len(counters.accumulateCalls)
	counters.mu.Unlock()
	if n != 1 {
		t.Fatalf("accumulate must have landed before the RPC returned, got %d calls", n)
	}
}

// A failed spend write is logged, not returned: the gateway cannot act on it and
// must never have a model turn blocked by an accounting failure.
func TestEmitLlmUsage_AccumulateErrorDoesNotFailRPC(t *testing.T) {
	counters := newFakeSpendCounterStore()
	counters.accumulateErr = errors.New("db down")
	svc := newSouthSvcWithCounters(t, newFakeLlmProviderStore(), counters)

	resp, err := svc.EmitLlmUsage(gatewayCtx(testGateway), &brokerv1.EmitLlmUsageRequest{
		TenantId: testTenant, UserId: "alice", Provider: "openrouter", Model: "m",
		TokensIn: 10, TokensOut: 20, Cost: 0.002,
	})
	if err != nil {
		t.Fatalf("a spend-write failure must not fail the RPC, got %v", err)
	}
	if resp == nil {
		t.Fatal("want a response even when the spend write failed")
	}
}

// An unpriced provider accrues $0 forever — invisible to spend caps by config,
// not by accident. Warn once per (tenant, provider) per process: the usage path
// runs on every model turn, so an undeduped warn would be log spam.
func TestRecordLlmUsage_UnpricedProviderWarnsOnce(t *testing.T) {
	core, recorded := observer.New(zapcore.WarnLevel)
	counters := newFakeSpendCounterStore()
	// Unique tenant so the process-wide dedup map can't be pre-poisoned by
	// another test in this package.
	tenant := "unpriced-" + uuid.NewString()
	deps := Deps{
		Logger:        zap.New(core),
		Providers:     newFakeLlmProviderStore(), // no rows → unresolvable, cost 0
		SpendCounters: counters,
	}

	for i := 0; i < 3; i++ {
		deps.recordLlmUsage(context.Background(), &brokerv1.EmitLlmUsageRequest{
			TenantId: tenant, Provider: "no-price", TokensIn: 100, TokensOut: 100, Cost: 0,
		})
	}

	warns := recorded.FilterMessageSnippet("no per-token pricing").All()
	if len(warns) != 1 {
		t.Fatalf("want exactly 1 unpriced warning across 3 calls, got %d", len(warns))
	}
	if !strings.Contains(warns[0].Message, "spend is not being tracked") {
		t.Errorf("warning must name the consequence, got %q", warns[0].Message)
	}
	// All three calls must still record their tokens — the warning is diagnostic,
	// never a reason to skip the write.
	counters.mu.Lock()
	n := len(counters.accumulateCalls)
	counters.mu.Unlock()
	if n != 3 {
		t.Errorf("all 3 calls must still accumulate, got %d", n)
	}
}

// A priced provider must never trip the unpriced warning.
func TestRecordLlmUsage_PricedProviderDoesNotWarn(t *testing.T) {
	core, recorded := observer.New(zapcore.WarnLevel)
	store := newFakeLlmProviderStore()
	tenant := "priced-" + uuid.NewString()
	store.rows[tenant+"/priced"] = fakeLlmProviderRow{
		id: "priced", name: "P", endpoint: "https://x.test", api: "openai-completions",
		priceInMicrosPerMTok: 2_000_000, priceOutMicrosPerMTok: 4_000_000,
	}
	deps := Deps{Logger: zap.New(core), Providers: store, SpendCounters: newFakeSpendCounterStore()}

	deps.recordLlmUsage(context.Background(), &brokerv1.EmitLlmUsageRequest{
		TenantId: tenant, Provider: "priced", TokensIn: 1_000_000, TokensOut: 1_000_000, Cost: 0,
	})

	if n := len(recorded.FilterMessageSnippet("no per-token pricing").All()); n != 0 {
		t.Errorf("priced provider must not warn, got %d warnings", n)
	}
}

func TestEmitLlmUsage_UnresolvableProviderAccumulatesZeroCost(t *testing.T) {
	counters := newFakeSpendCounterStore()
	svc := newSouthSvcWithCounters(t, newFakeLlmProviderStore(), counters) // no rows registered
	ctx := gatewayCtx(testGateway)

	_, err := svc.EmitLlmUsage(ctx, &brokerv1.EmitLlmUsageRequest{
		TenantId: testTenant, Provider: "unknown-provider", Model: "m",
		TokensIn: 100, TokensOut: 100, Cost: 0,
	})
	if err != nil {
		t.Fatalf("EmitLlmUsage: %v", err)
	}
	waitFor(t, func() bool {
		counters.mu.Lock()
		defer counters.mu.Unlock()
		return len(counters.accumulateCalls) > 0
	})
	counters.mu.Lock()
	call := counters.accumulateCalls[0]
	counters.mu.Unlock()
	if call.costMicros != 0 {
		t.Errorf("unresolvable provider: costMicros = %d, want 0", call.costMicros)
	}
	if call.tokensIn != 100 || call.tokensOut != 100 {
		t.Errorf("tokens must still be recorded even when cost is 0: %+v", call)
	}
}

