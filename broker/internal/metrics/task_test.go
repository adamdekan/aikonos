package metrics

import (
	"context"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// fakeTaskRecorder captures calls for assertion in tests that inject via
// TaskMetricsRecorder rather than exercising the real OTel pipeline.
type fakeTaskRecorder struct {
	mu                  sync.Mutex
	transitions         []string
	planOutcomes        []string
	toolInvokeDurations []string
}

func (f *fakeTaskRecorder) RecordTransition(_ context.Context, from, to, tenant string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transitions = append(f.transitions, from+"->"+to+"@"+tenant)
}

func (f *fakeTaskRecorder) RecordPlanOutcome(_ context.Context, outcome string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.planOutcomes = append(f.planOutcomes, outcome)
}

func (f *fakeTaskRecorder) RecordToolInvokeDuration(_ context.Context, seconds float64, tenant, toolID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.toolInvokeDurations = append(f.toolInvokeDurations, tenant+"/"+toolID)
}

func TestNewTaskRecorder_ReturnsNonNil(t *testing.T) {
	r := NewTaskRecorder()
	if r == nil {
		t.Fatal("NewTaskRecorder() must return a non-nil TaskRecorder")
	}
}

func TestTaskRecorder_NoopWhenMetricsDisabled(t *testing.T) {
	// Without a real MeterProvider, instruments are no-ops — this must not panic.
	r := NewTaskRecorder()
	ctx := context.Background()
	r.RecordTransition(ctx, "PLANNING", "VALIDATING", "t1")
	r.RecordPlanOutcome(ctx, "APPROVED")
	r.RecordToolInvokeDuration(ctx, 0.25, "t1", "web.fetch")
}

func TestFakeTaskRecorder_SatisfiesInterface(t *testing.T) {
	// Compile-time: *fakeTaskRecorder must satisfy TaskMetricsRecorder.
	var _ TaskMetricsRecorder = (*fakeTaskRecorder)(nil)
}

// newTestReader installs an in-memory manual reader as the global
// MeterProvider and returns it, restoring the previous provider on cleanup —
// mirrors the read-back approach used to prove real emission (llm_test.go
// only proves no-panic; here we additionally prove name+attrs land).
func newTestReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	prev := otel.GetMeterProvider()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	otel.SetMeterProvider(mp)
	t.Cleanup(func() { otel.SetMeterProvider(prev) })
	return reader
}

func collect(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	return rm
}

func findMetric(rm metricdata.ResourceMetrics, name string) (metricdata.Metrics, bool) {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m, true
			}
		}
	}
	return metricdata.Metrics{}, false
}

func TestTaskRecorder_RecordTransition_EmitsWithAttrs(t *testing.T) {
	reader := newTestReader(t)
	r := NewTaskRecorder()
	r.RecordTransition(context.Background(), "PLANNING", "VALIDATING", "tenant-1")

	rm := collect(t, reader)
	m, ok := findMetric(rm, "aikonos.broker.task.transitions")
	if !ok {
		t.Fatal("aikonos.broker.task.transitions not emitted")
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok || len(sum.DataPoints) != 1 {
		t.Fatalf("unexpected data shape: %+v", m.Data)
	}
	dp := sum.DataPoints[0]
	if dp.Value != 1 {
		t.Errorf("value = %d, want 1", dp.Value)
	}
	wantAttrs := map[string]string{"from": "PLANNING", "to": "VALIDATING", "tenant": "tenant-1"}
	for k, want := range wantAttrs {
		v, ok := dp.Attributes.Value(attribute.Key(k))
		if !ok || v.AsString() != want {
			t.Errorf("attr %q = %v, want %q", k, v, want)
		}
	}
}

func TestTaskRecorder_RecordPlanOutcome_EmitsWithAttrs(t *testing.T) {
	reader := newTestReader(t)
	r := NewTaskRecorder()
	r.RecordPlanOutcome(context.Background(), "APPROVED")

	rm := collect(t, reader)
	m, ok := findMetric(rm, "aikonos.broker.plan.outcomes")
	if !ok {
		t.Fatal("aikonos.broker.plan.outcomes not emitted")
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok || len(sum.DataPoints) != 1 {
		t.Fatalf("unexpected data shape: %+v", m.Data)
	}
	dp := sum.DataPoints[0]
	v, ok := dp.Attributes.Value(attribute.Key("outcome"))
	if !ok || v.AsString() != "APPROVED" {
		t.Errorf("attr outcome = %v, want APPROVED", v)
	}
}

func TestTaskRecorder_RecordToolInvokeDuration_EmitsWithAttrs(t *testing.T) {
	reader := newTestReader(t)
	r := NewTaskRecorder()
	r.RecordToolInvokeDuration(context.Background(), 0.42, "tenant-2", "web.fetch")

	rm := collect(t, reader)
	m, ok := findMetric(rm, "aikonos.broker.tool.invoke.duration")
	if !ok {
		t.Fatal("aikonos.broker.tool.invoke.duration not emitted")
	}
	hist, ok := m.Data.(metricdata.Histogram[float64])
	if !ok || len(hist.DataPoints) != 1 {
		t.Fatalf("unexpected data shape: %+v", m.Data)
	}
	dp := hist.DataPoints[0]
	if dp.Sum != 0.42 {
		t.Errorf("sum = %v, want 0.42", dp.Sum)
	}
	wantAttrs := map[string]string{"tenant": "tenant-2", "tool_id": "web.fetch"}
	for k, want := range wantAttrs {
		v, ok := dp.Attributes.Value(attribute.Key(k))
		if !ok || v.AsString() != want {
			t.Errorf("attr %q = %v, want %q", k, v, want)
		}
	}
}
