package alerting

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/notify"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	"go.uber.org/zap"
)

// Engine subscribes to audit events on the bus and forwards any Alerts produced
// by the Evaluator to the Notifier. It is read-only with respect to the audit
// stream — errors from the Notifier are logged and never propagated.
type Engine struct {
	sub     notify.Subscription
	ev      *Evaluator
	n       Notifier
	repo    AlertRepo // nil → no persistence
	log     *zap.Logger
	queue   chan Alert
	dropped atomic.Int64
}

// alertQueueSize bounds the alerts waiting for delivery (F34): Notify (an
// SMTP send) and the persist Insert run on one worker off the audit consumer
// loop, so a slow sink cannot stall it and a flood cannot spawn a goroutine
// per alert. A full queue drops the alert and counts it.
const alertQueueSize = 256

// alertSinkTimeout bounds one Notify or Insert.
const alertSinkTimeout = 30 * time.Second

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
	return &Engine{sub: sub, ev: ev, n: n, repo: repo, log: log, queue: make(chan Alert, alertQueueSize)}, nil
}

// Start spawns the consumer goroutine and the delivery worker. They return
// when ctx is cancelled (the consumer also when the subscription closes).
func (e *Engine) Start(ctx context.Context) {
	go e.run(ctx)
	go e.deliver(ctx)
}

// deliver sends queued alerts to the notifier and the repo, one at a time.
func (e *Engine) deliver(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case a := <-e.queue:
			nctx, cancel := context.WithTimeout(ctx, alertSinkTimeout)
			if err := e.n.Notify(nctx, a); err != nil {
				e.log.Warn("alerting: notifier error", zap.String("rule", a.Rule), zap.Error(err))
			}
			cancel()
			if e.repo != nil {
				pctx, cancel := context.WithTimeout(ctx, alertSinkTimeout)
				if err := e.repo.Insert(pctx, alertToStored(ids.EventID(), a)); err != nil {
					e.log.Warn("alerting: persist failed", zap.String("rule", a.Rule), zap.Error(err))
				}
				cancel()
			}
		}
	}
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
		select {
		case e.queue <- a:
		default:
			e.log.Warn("alerting: delivery queue full, alert dropped",
				zap.String("rule", a.Rule), zap.Int64("dropped_total", e.dropped.Add(1)))
		}
	}
}

// Close unsubscribes from the bus.
func (e *Engine) Close() {
	e.sub.Unsubscribe()
}
