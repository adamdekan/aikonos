package alerting

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/notify"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	"go.uber.org/zap"
)

// Engine subscribes to audit events on the bus and forwards any Alerts produced
// by the Evaluator to the Notifier. It is read-only with respect to the audit
// stream — errors from the Notifier are logged and never propagated.
type Engine struct {
	sub  notify.Subscription
	ev   *Evaluator
	n    Notifier
	repo AlertRepo // nil → no persistence
	log  *zap.Logger
}

// NewEngine subscribes to subject on bus and returns a ready Engine.
// subject must be an exact match understood by the bus implementation
// (NATS supports ">"; MemoryBus requires exact subjects).
// Deprecated: prefer NewEngineWithRepo; pass nil repo for the same behavior.
func NewEngine(bus notify.Bus, subject string, ev *Evaluator, n Notifier, log *zap.Logger) (*Engine, error) {
	return NewEngineWithRepo(bus, subject, ev, n, nil, log)
}

// NewEngineWithRepo is like NewEngine but also persists each fired alert via
// repo (async, fire-and-forget). Pass nil repo to disable persistence.
func NewEngineWithRepo(bus notify.Bus, subject string, ev *Evaluator, n Notifier, repo AlertRepo, log *zap.Logger) (*Engine, error) {
	sub, err := bus.Subscribe(subject)
	if err != nil {
		return nil, fmt.Errorf("alerting engine: subscribe %q: %w", subject, err)
	}
	return &Engine{sub: sub, ev: ev, n: n, repo: repo, log: log}, nil
}

// Start spawns the consumer goroutine. It returns when ctx is cancelled or the
// subscription channel is closed.
func (e *Engine) Start(ctx context.Context) {
	go e.run(ctx)
}

func (e *Engine) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case raw, ok := <-e.sub.Events():
			if !ok {
				return
			}
			e.handle(ctx, raw)
		}
	}
}

func (e *Engine) handle(ctx context.Context, raw []byte) {
	var ev auditv1.AuditEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		e.log.Warn("alerting: failed to unmarshal audit event", zap.Error(err))
		return
	}

	for _, a := range e.ev.Observe(&ev) {
		if err := e.n.Notify(ctx, a); err != nil {
			e.log.Warn("alerting: notifier error", zap.String("rule", a.Rule), zap.Error(err))
		}
		// Async persist — fire-and-forget like the audit MinIO sink.
		if e.repo != nil {
			go func(alert Alert) {
				sa := alertToStored(ids.EventID(), alert)
				if err := e.repo.Insert(context.Background(), sa); err != nil {
					e.log.Warn("alerting: persist failed", zap.String("rule", alert.Rule), zap.Error(err))
				}
			}(a)
		}
	}
}

// Close unsubscribes from the bus.
func (e *Engine) Close() {
	e.sub.Unsubscribe()
}
