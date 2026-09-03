package alerting

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/notify"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fakeNotifier collects delivered alerts and can be made to return an error.
type fakeNotifier struct {
	mu     sync.Mutex
	alerts []Alert
	errOn  int // if >0, return error on the Nth call (1-based); 0 = never error
	calls  int
}

func (f *fakeNotifier) Notify(_ context.Context, a Alert) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.errOn > 0 && f.calls == f.errOn {
		return fmt.Errorf("notifier error (injected)")
	}
	f.alerts = append(f.alerts, a)
	return nil
}

func (f *fakeNotifier) received() []Alert {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]Alert, len(f.alerts))
	copy(cp, f.alerts)
	return cp
}

func makeDenyEvent(tenant string) []byte {
	ev := &auditv1.AuditEvent{
		EventId:     "evt-test",
		TenantId:    tenant,
		EventType:   "tool.invoke",
		ResourceRef: "task/1",
		Decision:    auditv1.PolicyDecision_DENY,
		OccurredAt:  timestamppb.Now(),
	}
	data, _ := json.Marshal(ev)
	return data
}

func eventually(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func TestEngine_DeliversDenialAlert(t *testing.T) {
	bus := notify.NewMemoryBus()
	subject := "aikonos.audit.t1"

	ev := NewEvaluator(Config{DenyRateThreshold: 0})
	fn := &fakeNotifier{}
	log := zap.NewNop()

	eng, err := NewEngine(bus, subject, ev, fn, log)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eng.Start(ctx)
	defer eng.Close()

	if err := bus.Publish(context.Background(), subject, makeDenyEvent("t1")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	eventually(t, time.Second, func() bool {
		return len(fn.received()) >= 1
	})

	alerts := fn.received()
	if len(alerts) < 1 {
		t.Fatalf("want ≥1 alert, got %d", len(alerts))
	}
	if alerts[0].Rule != "denial" {
		t.Errorf("rule: want denial, got %s", alerts[0].Rule)
	}
}

func TestEngine_NotifierErrorDoesNotStopEngine(t *testing.T) {
	bus := notify.NewMemoryBus()
	subject := "aikonos.audit.t1"

	ev := NewEvaluator(Config{DenyRateThreshold: 0})
	// errOn=1: first Notify call returns an error; second should still be delivered.
	fn := &fakeNotifier{errOn: 1}
	log := zap.NewNop()

	eng, err := NewEngine(bus, subject, ev, fn, log)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eng.Start(ctx)
	defer eng.Close()

	// First event — notifier returns error (alert not appended, but engine keeps running).
	if err := bus.Publish(context.Background(), subject, makeDenyEvent("t1")); err != nil {
		t.Fatalf("Publish 1: %v", err)
	}

	// Wait for the first call to be processed.
	eventually(t, time.Second, func() bool {
		fn.mu.Lock()
		defer fn.mu.Unlock()
		return fn.calls >= 1
	})

	// Second event — notifier succeeds.
	if err := bus.Publish(context.Background(), subject, makeDenyEvent("t1")); err != nil {
		t.Fatalf("Publish 2: %v", err)
	}

	eventually(t, time.Second, func() bool {
		fn.mu.Lock()
		defer fn.mu.Unlock()
		return fn.calls >= 2
	})

	fn.mu.Lock()
	totalCalls := fn.calls
	fn.mu.Unlock()
	if totalCalls < 2 {
		t.Fatalf("want notifier called ≥2 times (proves loop processed both events), got %d", totalCalls)
	}

	got := fn.received()
	if len(got) < 1 {
		t.Fatalf("want ≥1 delivered alert after error recovery, got %d", len(got))
	}
}

func TestEngine_MalformedPayloadIsSkipped(t *testing.T) {
	bus := notify.NewMemoryBus()
	subject := "aikonos.audit.t1"

	ev := NewEvaluator(Config{DenyRateThreshold: 0})
	fn := &fakeNotifier{}
	log := zap.NewNop()

	eng, err := NewEngine(bus, subject, ev, fn, log)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eng.Start(ctx)
	defer eng.Close()

	// Malformed payload — engine must not crash, must skip silently.
	if err := bus.Publish(context.Background(), subject, []byte("not-json{")); err != nil {
		t.Fatalf("Publish malformed: %v", err)
	}

	// Valid DENY event published after the malformed one.
	if err := bus.Publish(context.Background(), subject, makeDenyEvent("t1")); err != nil {
		t.Fatalf("Publish valid: %v", err)
	}

	// Engine must still deliver the alert from the valid event.
	eventually(t, time.Second, func() bool {
		return len(fn.received()) >= 1
	})

	alerts := fn.received()
	if len(alerts) < 1 {
		t.Fatalf("want ≥1 alert from valid event after malformed payload, got %d", len(alerts))
	}
	if alerts[0].Rule != "denial" {
		t.Errorf("rule: want denial, got %s", alerts[0].Rule)
	}
}
