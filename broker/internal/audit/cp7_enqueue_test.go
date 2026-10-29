package audit

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
)

// TestEnqueueBackpressure verifies the bounded backpressure contract:
//   - a full/unbuffered queue blocks up to dropTimeout then returns false
//   - a queue with space returns true immediately
//
// We build Emitter struct literals so NewEmitter (which dials MinIO) is never
// called. Only the fields touched by enqueue need to be set.
func TestEnqueueBackpressure(t *testing.T) {
	job := uploadJob{event: &auditv1.AuditEvent{EventId: "evt-1", TenantId: "t1", EventType: "test.event"}, done: make(chan error, 1)}

	t.Run("full queue drops after timeout", func(t *testing.T) {
		// Unbuffered channel, no reader — always full.
		e := &Emitter{
			queue:       make(chan uploadJob),
			dropTimeout: 10 * time.Millisecond,
			logger:      zap.NewNop(),
		}
		start := time.Now()
		ok := e.enqueue(context.Background(), job)
		elapsed := time.Since(start)
		if ok {
			t.Fatal("expected enqueue to return false on full queue")
		}
		if elapsed < 5*time.Millisecond {
			t.Fatalf("dropped too fast (%v) — timer not running", elapsed)
		}
	})

	t.Run("non-full queue returns true immediately", func(t *testing.T) {
		// Buffered channel with room — enqueue wins the select instantly.
		e := &Emitter{
			queue:       make(chan uploadJob, 1),
			dropTimeout: 10 * time.Millisecond,
			logger:      zap.NewNop(),
		}
		ok := e.enqueue(context.Background(), job)
		if !ok {
			t.Fatal("expected enqueue to return true when queue has capacity")
		}
	})
}
