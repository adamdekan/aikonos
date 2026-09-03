package broker

// Non-per_mtok unit-quantity extension:
// modelCostMicros prices per_page/per_1k_queries/per_minute from the
// request's own quantity/unit, recordUsageEvent carries them through to the
// analytics row unchanged, and south GetLlmProviders surfaces the tenant's
// capability defaults map (checkpoint success criteria 1-3).

import (
	"context"
	"reflect"
	"testing"

	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/db"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ── success criterion 1: modelCostMicros unit-quantity cost math ────────────

func TestModelCostMicros_UnitQuantity(t *testing.T) {
	for _, c := range []struct {
		name string
		pr   *db.ModelPricing
		req  *brokerv1.EmitLlmUsageRequest
		want int64
	}{
		{
			name: "per_page multiplies quantity by in_micros",
			pr:   &db.ModelPricing{Unit: "per_page", InMicros: 40_000},
			req:  &brokerv1.EmitLlmUsageRequest{Quantity: 12, Unit: "per_page"},
			want: 480_000, // 12 x 40_000
		},
		{
			name: "per_1k_queries divides quantity by 1000",
			pr:   &db.ModelPricing{Unit: "per_1k_queries", InMicros: 5_000_000},
			req:  &brokerv1.EmitLlmUsageRequest{Quantity: 2_500, Unit: "per_1k_queries"},
			want: 12_500_000, // 2500/1000 x 5_000_000
		},
		{
			name: "fractional minutes round instead of truncate",
			pr:   &db.ModelPricing{Unit: "per_minute", InMicros: 100_000},
			req:  &brokerv1.EmitLlmUsageRequest{Quantity: 1.5, Unit: "per_minute"},
			want: 150_000, // 1.5 x 100_000, exact
		},
		{
			name: "fractional minutes round-half-up at the micro boundary",
			pr:   &db.ModelPricing{Unit: "per_minute", InMicros: 1},
			req:  &brokerv1.EmitLlmUsageRequest{Quantity: 1.6, Unit: "per_minute"},
			want: 2, // round(1.6) = 2, truncation would give 1
		},
		{
			name: "unit mismatch costs 0",
			pr:   &db.ModelPricing{Unit: "per_page", InMicros: 40_000},
			req:  &brokerv1.EmitLlmUsageRequest{Quantity: 12, Unit: "per_image"},
			want: 0,
		},
		{
			name: "zero quantity costs 0 even with matching unit",
			pr:   &db.ModelPricing{Unit: "per_page", InMicros: 40_000},
			req:  &brokerv1.EmitLlmUsageRequest{Quantity: 0, Unit: "per_page"},
			want: 0,
		},
		{
			name: "free unit is always 0 regardless of quantity",
			pr:   &db.ModelPricing{Unit: "free", InMicros: 999_999},
			req:  &brokerv1.EmitLlmUsageRequest{Quantity: 100, Unit: "free"},
			want: 0,
		},
		{
			// per_mtok's token path is untouched by the unit-quantity extension —
			// same case as TestCostMicrosFor_PerModelPricing's "per_mtok in/out".
			name: "per_mtok token path stays byte-identical",
			pr: &db.ModelPricing{
				Unit: db.PricingUnitPerMTok, InMicros: 3_000_000, OutMicros: 15_000_000,
			},
			req:  &brokerv1.EmitLlmUsageRequest{TokensIn: 1_000_000, TokensOut: 500_000},
			want: 10_500_000,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := modelCostMicros(c.pr, c.req); got != c.want {
				t.Errorf("modelCostMicros = %d, want %d", got, c.want)
			}
		})
	}
}

// ── success criterion 2: usage-event insert round-trip ──────────────────────

func TestRecordUsageEvent_CarriesQuantityAndUnit(t *testing.T) {
	events := &fakeUsageEventStore{}
	deps := Deps{Logger: zap.NewNop(), UsageEvents: events}

	deps.recordUsageEvent(context.Background(), &brokerv1.EmitLlmUsageRequest{
		TenantId: testTenant, Provider: "openrouter", Model: "embed-1",
		Quantity: 12, Unit: "per_page",
	}, 480_000)

	got := events.all()
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	if got[0].Quantity != 12 || got[0].Unit != "per_page" {
		t.Errorf("event quantity/unit = %v/%q, want 12/per_page", got[0].Quantity, got[0].Unit)
	}
}

// Existing token-only senders never populate quantity/unit — they must
// round-trip as the zero values (0/""), not silently drop the row.
func TestRecordUsageEvent_TokenOnlySenderYieldsZeroQuantityAndEmptyUnit(t *testing.T) {
	events := &fakeUsageEventStore{}
	deps := Deps{Logger: zap.NewNop(), UsageEvents: events}

	deps.recordUsageEvent(context.Background(), &brokerv1.EmitLlmUsageRequest{
		TenantId: testTenant, Provider: "openrouter", Model: "gpt-4o",
		TokensIn: 100, TokensOut: 200,
	}, 4_200)

	got := events.all()
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	if got[0].Quantity != 0 || got[0].Unit != "" {
		t.Errorf("token-only event quantity/unit = %v/%q, want 0/\"\"", got[0].Quantity, got[0].Unit)
	}
}

// ── success criterion 3: south GetLlmProviders carries defaults ────────────

func TestGetLlmProviders_CarriesDefaultsMap(t *testing.T) {
	store := newFakeLlmProviderStore()
	store.rows[testTenant+"/embed-a"] = fakeLlmProviderRow{
		id: "embed-a", name: "A", endpoint: "https://a.test", api: "openai-completions",
	}
	if err := store.SetDefaultFor(context.Background(), testTenant, "embedding", "embed-a"); err != nil {
		t.Fatalf("SetDefaultFor: %v", err)
	}
	svc := newSouthSvc(t, newCapturingSecrets(), store, nil)

	resp, err := svc.GetLlmProviders(gatewayCtx(testGateway), &brokerv1.GetLlmProvidersRequest{TenantId: testTenant})
	if err != nil {
		t.Fatalf("GetLlmProviders: %v", err)
	}
	if resp.Defaults["embedding"] != "embed-a" {
		t.Errorf("south defaults = %+v, want embedding -> embed-a", resp.Defaults)
	}
}

// A tenant with no defaults set still gets a non-nil (empty) map, never nil —
// the gateway's fail-open selection reads the map directly.
func TestGetLlmProviders_NoDefaultsYieldsEmptyMap(t *testing.T) {
	store := newFakeLlmProviderStore()
	svc := newSouthSvc(t, newCapturingSecrets(), store, nil)

	resp, err := svc.GetLlmProviders(gatewayCtx(testGateway), &brokerv1.GetLlmProvidersRequest{TenantId: testTenant})
	if err != nil {
		t.Fatalf("GetLlmProviders: %v", err)
	}
	if resp.Defaults == nil {
		t.Error("Defaults must be a non-nil empty map, not nil")
	}
	if !reflect.DeepEqual(resp.Defaults, map[string]string{}) {
		t.Errorf("Defaults = %+v, want empty map", resp.Defaults)
	}
}
