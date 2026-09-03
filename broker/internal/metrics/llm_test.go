package metrics

import (
	"context"
	"sync"
	"testing"
)

// fakeRecorder captures RecordUsage calls for assertion in tests.
type fakeRecorder struct {
	mu    sync.Mutex
	calls []UsageAttrs
}

func (f *fakeRecorder) RecordUsage(_ context.Context, a UsageAttrs) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, a)
}

func (f *fakeRecorder) last() (UsageAttrs, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return UsageAttrs{}, false
	}
	return f.calls[len(f.calls)-1], true
}

func TestNew_ReturnsNonNil(t *testing.T) {
	r := New()
	if r == nil {
		t.Fatal("New() must return a non-nil Recorder")
	}
}

func TestRecorder_RecordUsage_NoopWhenMetricsDisabled(t *testing.T) {
	// Without a real MeterProvider, instruments are no-ops — this must not
	// panic.
	r := New()
	r.RecordUsage(context.Background(), UsageAttrs{
		Provider:        "openrouter",
		Model:           "anthropic/claude-sonnet-4-6",
		Tenant:          "t1",
		Agent:           "agent1",
		TokensIn:        100,
		TokensOut:       200,
		TokensCacheRead:  50,
		TokensCacheWrite: 10,
		Cost:            0.0042,
	})
}

func TestFakeRecorder_CapturesAttrs(t *testing.T) {
	f := &fakeRecorder{}
	f.RecordUsage(context.Background(), UsageAttrs{
		Provider:  "openrouter",
		Model:     "claude-3",
		Tenant:    "tenant-1",
		Agent:     "agent-x",
		TokensIn:  100,
		TokensOut: 200,
		Cost:      0.001,
	})
	got, ok := f.last()
	if !ok {
		t.Fatal("expected a captured call")
	}
	if got.Provider != "openrouter" || got.Model != "claude-3" || got.TokensIn != 100 || got.TokensOut != 200 {
		t.Errorf("captured attrs wrong: %+v", got)
	}
}

func TestFakeRecorder_UsageRecorderInterface(t *testing.T) {
	// Compile-time: *fakeRecorder must satisfy UsageRecorder.
	var _ UsageRecorder = (*fakeRecorder)(nil)
}
