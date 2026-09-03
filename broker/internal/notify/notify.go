// broker/internal/notify/notify.go
// Event routing over NATS.
//
// The broker publishes task and envelope lifecycle events to NATS subjects so
// that frontends can stream them (StreamTaskEvents) and recipients can be
// pushed delegation envelopes instead of polling their inbox. The transport is
// abstracted behind Bus so the broker can run with NATS, with an in-process bus
// (tests / single-node dev), or disabled.
//
//	subjects:
//	  aikonos.task.<tenant>.<task_id>     — task lifecycle events
//	  aikonos.inbox.<tenant>.<user_id>    — delegation envelopes delivered to a user
//
// Disabled fallback: when no NATS URL is configured New returns a no-op bus
// (Enabled()==false). Publishing is dropped and Subscribe yields nothing, so
// StreamTaskEvents reports Unimplemented rather than blocking — mirroring the
// OIDC/OpenFGA "unset config ⇒ feature off" posture elsewhere in the broker.
package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// Event is the JSON payload published on a subject. It mirrors the wire
// brokerv1.TaskEvent loosely; the service layer converts between the two.
type Event struct {
	EventID    string         `json:"event_id"`
	TaskID     string         `json:"task_id"`
	TenantID   string         `json:"tenant_id"`
	Type       string         `json:"type"`
	Payload    map[string]any `json:"payload,omitempty"`
	OccurredAt time.Time      `json:"occurred_at"`
}

// Subscription delivers raw event payloads until Unsubscribe is called.
type Subscription interface {
	Events() <-chan []byte
	Unsubscribe()
}

// Bus is the event transport.
type Bus interface {
	Enabled() bool
	Publish(ctx context.Context, subject string, payload []byte) error
	Subscribe(subject string) (Subscription, error)
	Close() error
}

// TaskSubject returns the subject task lifecycle events are published on.
func TaskSubject(tenantID, taskID string) string {
	return fmt.Sprintf("aikonos.task.%s.%s", token(tenantID), token(taskID))
}

// InboxSubject returns the subject delegation envelopes for a user land on.
func InboxSubject(tenantID, userID string) string {
	return fmt.Sprintf("aikonos.inbox.%s.%s", token(tenantID), token(userID))
}

// AuditSubject returns the subject audit events are published on (one per
// tenant). Observability consumers subscribe to "aikonos.audit.>".
func AuditSubject(tenantID string) string {
	return fmt.Sprintf("aikonos.audit.%s", token(tenantID))
}

// AuditWildcardSubject returns the NATS wildcard that matches all tenant audit
// subjects. Use this when subscribing to the full audit stream (e.g. alerting
// engine in production); MemoryBus requires exact subjects so tests must use
// AuditSubject or a per-tenant subject instead.
func AuditWildcardSubject() string { return "aikonos.audit.>" }

// New builds a Bus. An empty url yields the disabled no-op bus.
// logger is used by the NATS bus for operational warnings (e.g. unsubscribe failures).
func New(url string, logger *zap.Logger) (Bus, error) {
	if url == "" {
		return &noopBus{}, nil
	}
	return newNATSBus(url, logger)
}

// PublishEvent marshals e and publishes it on subject. Best-effort: callers
// should not fail their RPC if event routing is down.
func PublishEvent(ctx context.Context, b Bus, subject string, e *Event) error {
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	return b.Publish(ctx, subject, data)
}

// token sanitises a path element so it cannot inject NATS subject wildcards or
// separators ('.', '*', '>', whitespace).
func token(s string) string {
	if s == "" {
		return "_"
	}
	out := []rune(s)
	for i, r := range out {
		switch r {
		case '.', '*', '>', ' ', '\t', '\n':
			out[i] = '_'
		}
	}
	return string(out)
}

// ── disabled no-op bus ─────────────────────────────────────────────────────

type noopBus struct{}

func (*noopBus) Enabled() bool                                 { return false }
func (*noopBus) Publish(context.Context, string, []byte) error { return nil }
func (*noopBus) Subscribe(string) (Subscription, error)        { return nil, ErrDisabled }
func (*noopBus) Close() error                                  { return nil }

// ErrDisabled is returned by Subscribe on a disabled bus.
var ErrDisabled = fmt.Errorf("notify: bus is disabled (no NATS url configured)")
