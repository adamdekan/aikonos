// broker/internal/metrics/llm.go — LLM token/cost/request counters.
// Registered on the global OTel meter after initMeter in main.go.
// Instruments are always non-nil (a no-op MeterProvider returns no-op
// instruments), so callers need no nil guard.
package metrics

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const meterName = "github.com/adamdekan/aikonos/broker"

// UsageAttrs carries the label values for one EmitLlmUsage call.
type UsageAttrs struct {
	Provider string
	Model    string
	Tenant   string
	Agent    string
	// Per-direction token counts.
	TokensIn        int64
	TokensOut       int64
	TokensCacheRead  int64
	TokensCacheWrite int64
	// Computed total cost (sum over all directions, in USD).
	Cost float64
}

// UsageRecorder is the minimal interface the broker's EmitLlmUsage handler
// needs. The real OTel implementation and a capturable fake for tests both
// satisfy it.
type UsageRecorder interface {
	RecordUsage(ctx context.Context, a UsageAttrs)
}

// Recorder is the production OTel implementation. Obtain via New().
type Recorder struct {
	tokens   metric.Int64Counter
	cost     metric.Float64Counter
	requests metric.Int64Counter
}

// New constructs a Recorder from the global OTel meter. Safe to call before
// otel.SetMeterProvider — the global meter returns no-op instruments until a
// real provider is installed, so early calls are harmless.
func New() *Recorder {
	m := otel.Meter(meterName)
	tokens, _ := m.Int64Counter("llm_tokens_total",
		metric.WithDescription("Total LLM tokens by provider/model/tenant/agent/direction"))
	cost, _ := m.Float64Counter("llm_cost_total",
		metric.WithDescription("Total LLM cost in USD by provider/model/tenant/agent"))
	requests, _ := m.Int64Counter("llm_requests_total",
		metric.WithDescription("Total LLM requests (model turns) by provider/model/tenant/agent"))
	return &Recorder{tokens: tokens, cost: cost, requests: requests}
}

// RecordUsage increments all three counters for a single model turn.
// user_id is deliberately absent (unbounded cardinality — belongs on the audit
// event only, not on metrics).
func (r *Recorder) RecordUsage(ctx context.Context, a UsageAttrs) {
	base := []attribute.KeyValue{
		attribute.String("provider", a.Provider),
		attribute.String("model", a.Model),
		attribute.String("tenant", a.Tenant),
		attribute.String("agent", a.Agent),
	}

	type dirTokens struct {
		dir    string
		tokens int64
	}
	for _, dt := range []dirTokens{
		{"input", a.TokensIn},
		{"output", a.TokensOut},
		{"cache_read", a.TokensCacheRead},
		{"cache_write", a.TokensCacheWrite},
	} {
		if dt.tokens > 0 {
			attrs := append(base, attribute.String("direction", dt.dir)) //nolint:gocritic
			r.tokens.Add(ctx, dt.tokens, metric.WithAttributes(attrs...))
		}
	}

	if a.Cost > 0 {
		r.cost.Add(ctx, a.Cost, metric.WithAttributes(base...))
	}
	r.requests.Add(ctx, 1, metric.WithAttributes(base...))
}
