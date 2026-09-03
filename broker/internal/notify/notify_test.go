package notify

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestSubjectHelpers(t *testing.T) {
	if got := TaskSubject("tenant-1", "task-9"); got != "aikonos.task.tenant-1.task-9" {
		t.Errorf("TaskSubject = %q", got)
	}
	// The '.' in an email is a NATS separator, so it must be sanitised.
	if got := InboxSubject("t", "bob@example.com"); got != "aikonos.inbox.t.bob@example_com" {
		t.Errorf("InboxSubject = %q", got)
	}
	// Subject-injection characters are neutralised.
	if got := TaskSubject("a.b", "c*>d"); got != "aikonos.task.a_b.c__d" {
		t.Errorf("token sanitisation = %q", got)
	}
	if got := TaskSubject("", ""); got != "aikonos.task._._" {
		t.Errorf("empty token = %q", got)
	}
}

func TestNew_DisabledByDefault(t *testing.T) {
	b, err := New("", zap.NewNop())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b.Enabled() {
		t.Error("empty url should yield a disabled bus")
	}
	if err := b.Publish(context.Background(), "x", []byte("y")); err != nil {
		t.Errorf("disabled Publish should be a no-op, got %v", err)
	}
	if _, err := b.Subscribe("x"); err != ErrDisabled {
		t.Errorf("disabled Subscribe should return ErrDisabled, got %v", err)
	}
}

func TestMemoryBus_PubSub(t *testing.T) {
	b := NewMemoryBus()
	sub, err := b.Subscribe("aikonos.task.t.1")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Unsubscribe()

	ev := &Event{EventID: "e1", TaskID: "1", TenantID: "t", Type: "STATUS_CHANGED", OccurredAt: time.Unix(0, 0)}
	if err := PublishEvent(context.Background(), b, "aikonos.task.t.1", ev); err != nil {
		t.Fatalf("PublishEvent: %v", err)
	}

	select {
	case raw := <-sub.Events():
		var got Event
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.EventID != "e1" || got.Type != "STATUS_CHANGED" {
			t.Errorf("got %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestMemoryBus_OnlyMatchingSubject(t *testing.T) {
	b := NewMemoryBus()
	sub, _ := b.Subscribe("aikonos.task.t.1")
	defer sub.Unsubscribe()

	_ = b.Publish(context.Background(), "aikonos.task.t.2", []byte("other"))

	select {
	case <-sub.Events():
		t.Fatal("received an event for a different subject")
	case <-time.After(100 * time.Millisecond):
		// expected: nothing delivered
	}
}

func TestMemoryBus_UnsubscribeStops(t *testing.T) {
	b := NewMemoryBus()
	sub, _ := b.Subscribe("s")
	sub.Unsubscribe()
	// Channel is closed after unsubscribe.
	if _, ok := <-sub.Events(); ok {
		t.Error("expected closed channel after Unsubscribe")
	}
	// Publishing after unsubscribe must not panic.
	if err := b.Publish(context.Background(), "s", []byte("x")); err != nil {
		t.Errorf("publish after unsubscribe: %v", err)
	}
	sub.Unsubscribe() // idempotent
}
