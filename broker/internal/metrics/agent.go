// broker/internal/metrics/agent.go — agent behavioral baseline drift OTel
// metrics. Mirrors task.go's
// pattern: registered on the global OTel meter, always non-nil (a no-op
// MeterProvider returns no-op instruments), so *AgentRecorder itself never
// needs a nil guard — callers holding an *optional* AgentMetricsRecorder
// field still nil-check the field.
package metrics

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// AgentMetricsRecorder is the minimal interface baseline-drift call sites
// need. The real OTel implementation (*AgentRecorder) and a capturable fake
// for tests both satisfy it.
type AgentMetricsRecorder interface {
	RecordBaselineDrift(ctx context.Context, tenant, agent, kind string)
}

// AgentRecorder is the production OTel implementation. Obtain via
// NewAgentRecorder().
type AgentRecorder struct {
	baselineDrift metric.Int64Counter
}

// NewAgentRecorder constructs an AgentRecorder from the global OTel meter.
// Safe to call before otel.SetMeterProvider — the global meter returns no-op
// instruments until a real provider is installed, so early calls are harmless.
func NewAgentRecorder() *AgentRecorder {
	m := otel.Meter(meterName)
	baselineDrift, _ := m.Int64Counter("aikonos.broker.agent.baseline_drift",
		metric.WithDescription("Detected agent behavioral baseline drift events by tenant/agent/kind"))
	return &AgentRecorder{baselineDrift: baselineDrift}
}

// RecordBaselineDrift increments the baseline-drift counter for one detected
// deviation from an agent's learned envelope.
func (r *AgentRecorder) RecordBaselineDrift(ctx context.Context, tenant, agent, kind string) {
	r.baselineDrift.Add(ctx, 1, metric.WithAttributes(
		attribute.String("tenant", tenant),
		attribute.String("agent", agent),
		attribute.String("kind", kind),
	))
}
