package broker

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/notify"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// fakeStream implements brokerv1.BrokerService_StreamTaskEventsServer
// (grpc.ServerStreamingServer[TaskEvent]) for tests.
type fakeStream struct {
	grpc.ServerStream
	ctx  context.Context
	mu   sync.Mutex
	sent []*brokerv1.TaskEvent
}

func (f *fakeStream) Context() context.Context { return f.ctx }

func (f *fakeStream) Send(ev *brokerv1.TaskEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, ev)
	return nil
}

func (f *fakeStream) received() []*brokerv1.TaskEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*brokerv1.TaskEvent(nil), f.sent...)
}

func TestStreamTaskEvents_DisabledBus(t *testing.T) {
	svc := NewBrokerService(Deps{Logger: zap.NewNop(), Notify: mustBus(t, "")})
	err := svc.StreamTaskEvents(
		&brokerv1.StreamTaskEventsRequest{TaskId: "t1", TenantId: testTenantUUID},
		&fakeStream{ctx: context.Background()},
	)
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("disabled bus: want Unimplemented, got %v", err)
	}
}

func TestStreamTaskEvents_ForwardsEvents(t *testing.T) {
	bus := notify.NewMemoryBus()
	svc := NewBrokerService(Deps{Logger: zap.NewNop(), Notify: bus, TenantID: testTenantUUID})

	ctx, cancel := context.WithCancel(ctxWithIdentity(testTenantUUID, "alice@example.com"))
	stream := &fakeStream{ctx: ctx}

	done := make(chan error, 1)
	go func() {
		done <- svc.StreamTaskEvents(&brokerv1.StreamTaskEventsRequest{TaskId: "t1", TenantId: testTenantUUID}, stream)
	}()

	// Give the subscription a moment to register, then publish two events.
	waitFor(t, func() bool {
		publishTaskEvent(context.Background(), Deps{Logger: zap.NewNop(), Notify: bus}, testTenantUUID, "t1", "STATUS_CHANGED", map[string]any{"status": "CREATED"})
		return len(stream.received()) >= 1
	})
	publishTaskEvent(context.Background(), Deps{Logger: zap.NewNop(), Notify: bus}, testTenantUUID, "t1", "APPROVAL_NEEDED", map[string]any{"plan_id": "p1"})

	waitFor(t, func() bool { return len(stream.received()) >= 2 })

	cancel() // client disconnects
	select {
	case err := <-done:
		if status.Code(err) != codes.Canceled {
			t.Fatalf("want Canceled on client disconnect, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("StreamTaskEvents did not return after cancel")
	}

	got := stream.received()
	if len(got) < 2 {
		t.Fatalf("expected >=2 events, got %d", len(got))
	}
	// First event maps STATUS_CHANGED to its enum and round-trips the payload.
	if got[0].Type != brokerv1.TaskEventType_STATUS_CHANGED {
		t.Errorf("event 0 type = %v", got[0].Type)
	}
	if got[0].TaskId != "t1" {
		t.Errorf("event 0 task id = %q", got[0].TaskId)
	}
	if got[0].Payload == nil || got[0].Payload.Fields["status"].GetStringValue() != "CREATED" {
		t.Errorf("event 0 payload not round-tripped: %v", got[0].Payload)
	}
}

func TestStreamTaskEvents_OtherTaskNotForwarded(t *testing.T) {
	bus := notify.NewMemoryBus()
	svc := NewBrokerService(Deps{Logger: zap.NewNop(), Notify: bus, TenantID: testTenantUUID})

	ctx, cancel := context.WithCancel(ctxWithIdentity(testTenantUUID, "alice@example.com"))
	defer cancel()
	stream := &fakeStream{ctx: ctx}
	go svc.StreamTaskEvents(&brokerv1.StreamTaskEventsRequest{TaskId: "t1", TenantId: testTenantUUID}, stream)

	// Publish to a different task; the subscriber on t1 must not see it.
	time.Sleep(50 * time.Millisecond)
	publishTaskEvent(context.Background(), Deps{Logger: zap.NewNop(), Notify: bus}, testTenantUUID, "t2", "STATUS_CHANGED", nil)
	time.Sleep(100 * time.Millisecond)

	if n := len(stream.received()); n != 0 {
		t.Fatalf("expected 0 events for t1, got %d", n)
	}
}

// recordingBus wraps a notify.Bus and records every Subscribe call, so a test
// can assert a subscription never happened (e.g. because the identity gate
// rejected the request first).
type recordingBus struct {
	notify.Bus
	mu            sync.Mutex
	subscribed    bool
	subscribeArgs []string
}

func (b *recordingBus) Subscribe(subject string) (notify.Subscription, error) {
	b.mu.Lock()
	b.subscribed = true
	b.subscribeArgs = append(b.subscribeArgs, subject)
	b.mu.Unlock()
	return b.Bus.Subscribe(subject)
}

// TestStreamTaskEvents_SpoofedTenant_RejectedBeforeSubscribe is the F21
// red-first counterpart to the GetTaskState/CancelTask spoofed-tenant tests:
// a caller authenticated for testTenantUUID must not be able to stream events
// for a different, spoofed req.TenantId. Pre-change, StreamTaskEvents called
// Notify.Subscribe(notify.TaskSubject(req.TenantId, req.TaskId)) directly, so
// the spoofed tenant would have reached the bus; post-change it must be
// rejected before any subscription happens.
func TestStreamTaskEvents_SpoofedTenant_RejectedBeforeSubscribe(t *testing.T) {
	bus := &recordingBus{Bus: notify.NewMemoryBus()}
	svc := NewBrokerService(Deps{Logger: zap.NewNop(), Notify: bus, TenantID: testTenantUUID})

	// Bounded so a pre-change regression (subscribe-then-block-forever on the
	// spoofed tenant) fails fast on DeadlineExceeded instead of hanging the
	// suite, rather than silently succeeding.
	ctx, cancel := context.WithTimeout(ctxWithIdentity(testTenantUUID, "alice@example.com"), 2*time.Second)
	defer cancel()
	stream := &fakeStream{ctx: ctx}

	err := svc.StreamTaskEvents(&brokerv1.StreamTaskEventsRequest{TaskId: "t1", TenantId: spoofedTenantUUID}, stream)

	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied for a spoofed tenant_id, got %v", err)
	}
	bus.mu.Lock()
	defer bus.mu.Unlock()
	if bus.subscribed {
		t.Fatalf("bus was subscribed (subjects=%v) despite the spoofed tenant_id — pre-change behavior (req.TenantId trusted directly)", bus.subscribeArgs)
	}
}

func mustBus(t *testing.T, url string) notify.Bus {
	t.Helper()
	b, err := notify.New(url, zap.NewNop())
	if err != nil {
		t.Fatalf("notify.New: %v", err)
	}
	return b
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("condition not met within deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
