package metrics

import (
	"context"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// fakeAgentRecorder captures calls for assertion in tests that inject via
// AgentMetricsRecorder rather than exercising the real OTel pipeline.
type fakeAgentRecorder struct {
	mu      sync.Mutex
	drifted []string
}

func (f *fakeAgentRecorder) RecordBaselineDrift(_ context.Context, tenant, agent, kind string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drifted = append(f.drifted, tenant+"/"+agent+"/"+kind)
}

func TestNewAgentRecorder_ReturnsNonNil(t *testing.T) {
	r := NewAgentRecorder()
	if r == nil {
		t.Fatal("NewAgentRecorder() must return a non-nil AgentRecorder")
	}
}

func TestAgentRecorder_NoopWhenMetricsDisabled(t *testing.T) {
	// Without a real MeterProvider, instruments are no-ops — this must not panic.
	r := NewAgentRecorder()
	r.RecordBaselineDrift(context.Background(), "t1", "agent-1", "rate")
}

func TestFakeAgentRecorder_SatisfiesInterface(t *testing.T) {
	// Compile-time: *fakeAgentRecorder must satisfy AgentMetricsRecorder.
	var _ AgentMetricsRecorder = (*fakeAgentRecorder)(nil)
}

func TestAgentRecorder_RecordBaselineDrift_EmitsWithAttrs(t *testing.T) {
	reader := newTestReader(t)
	r := NewAgentRecorder()
	r.RecordBaselineDrift(context.Background(), "tenant-1", "agent-1", "unknown_tool")

	rm := collect(t, reader)
	m, ok := findMetric(rm, "aikonos.broker.agent.baseline_drift")
	if !ok {
		t.Fatal("aikonos.broker.agent.baseline_drift not emitted")
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok || len(sum.DataPoints) != 1 {
		t.Fatalf("unexpected data shape: %+v", m.Data)
	}
	dp := sum.DataPoints[0]
	if dp.Value != 1 {
		t.Errorf("value = %d, want 1", dp.Value)
	}
	wantAttrs := map[string]string{"tenant": "tenant-1", "agent": "agent-1", "kind": "unknown_tool"}
	for k, want := range wantAttrs {
		v, ok := dp.Attributes.Value(attribute.Key(k))
		if !ok || v.AsString() != want {
			t.Errorf("attr %q = %v, want %q", k, v, want)
		}
	}
}
