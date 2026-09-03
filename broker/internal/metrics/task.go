// broker/internal/metrics/task.go — task-lifecycle OTel metrics (F23).
// Registered on the global OTel meter after initMeter in main.go, mirroring
// llm.go's pattern. Instruments are always non-nil (a no-op MeterProvider
// returns no-op instruments), so a *TaskRecorder itself never needs a nil
// guard — callers that hold an *optional* TaskMetricsRecorder field (e.g.
// db.TaskRepo, broker.Deps) still nil-check the field, since it may be unset
// in older constructors/tests that predate this feature.
package metrics

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// TaskMetricsRecorder is the minimal interface task-lifecycle call sites
// need. The real OTel implementation (*TaskRecorder) and a capturable fake
// for tests both satisfy it.
type TaskMetricsRecorder interface {
	RecordTransition(ctx context.Context, from, to, tenant string)
	RecordPlanOutcome(ctx context.Context, outcome string)
	RecordToolInvokeDuration(ctx context.Context, seconds float64, tenant, toolID string)
}

// TaskRecorder is the production OTel implementation. Obtain via
// NewTaskRecorder().
type TaskRecorder struct {
	transitions        metric.Int64Counter
	planOutcomes       metric.Int64Counter
	toolInvokeDuration metric.Float64Histogram
}

// NewTaskRecorder constructs a TaskRecorder from the global OTel meter. Safe
// to call before otel.SetMeterProvider — the global meter returns no-op
// instruments until a real provider is installed, so early calls are harmless.
func NewTaskRecorder() *TaskRecorder {
	m := otel.Meter(meterName)
	transitions, _ := m.Int64Counter("aikonos.broker.task.transitions",
		metric.WithDescription("Task state transitions by from/to/tenant"))
	planOutcomes, _ := m.Int64Counter("aikonos.broker.plan.outcomes",
		metric.WithDescription("SubmitPlan aggregate outcomes by outcome"))
	toolInvokeDuration, _ := m.Float64Histogram("aikonos.broker.tool.invoke.duration",
		metric.WithDescription("Tool invocation duration in seconds by tenant/tool_id"),
		metric.WithUnit("s"))
	return &TaskRecorder{
		transitions:        transitions,
		planOutcomes:       planOutcomes,
		toolInvokeDuration: toolInvokeDuration,
	}
}

// RecordTransition increments the transitions counter for one successful
// task state transition.
func (r *TaskRecorder) RecordTransition(ctx context.Context, from, to, tenant string) {
	r.transitions.Add(ctx, 1, metric.WithAttributes(
		attribute.String("from", from),
		attribute.String("to", to),
		attribute.String("tenant", tenant),
	))
}

// RecordPlanOutcome increments the plan-outcomes counter once per SubmitPlan
// call, labeled by the aggregate outcome (DENIED/NEEDS_STEP_UP/NEEDS_HUMAN/APPROVED).
func (r *TaskRecorder) RecordPlanOutcome(ctx context.Context, outcome string) {
	r.planOutcomes.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

// RecordToolInvokeDuration records one tool-invocation duration (seconds).
func (r *TaskRecorder) RecordToolInvokeDuration(ctx context.Context, seconds float64, tenant, toolID string) {
	r.toolInvokeDuration.Record(ctx, seconds, metric.WithAttributes(
		attribute.String("tenant", tenant),
		attribute.String("tool_id", toolID),
	))
}
