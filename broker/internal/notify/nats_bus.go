// broker/internal/notify/nats_bus.go
// NATS-backed Bus implementation.
package notify

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

type natsBus struct {
	conn   *nats.Conn
	logger *zap.Logger
}

func newNATSBus(url string, logger *zap.Logger) (*natsBus, error) {
	conn, err := nats.Connect(url,
		nats.Name("aikonos-broker"),
		nats.MaxReconnects(-1), // reconnect forever; the broker outlives NATS blips
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("nats connect %s: %w", url, err)
	}
	return &natsBus{conn: conn, logger: logger}, nil
}

func (b *natsBus) Enabled() bool { return true }

func (b *natsBus) Publish(_ context.Context, subject string, payload []byte) error {
	return b.conn.Publish(subject, payload)
}

func (b *natsBus) Subscribe(subject string) (Subscription, error) {
	ch := make(chan *nats.Msg, 64)
	sub, err := b.conn.ChanSubscribe(subject, ch)
	if err != nil {
		return nil, fmt.Errorf("nats subscribe %s: %w", subject, err)
	}
	out := make(chan []byte, 64)
	done := make(chan struct{})
	go func() {
		defer close(out)
		for {
			select {
			case msg, ok := <-ch:
				if !ok {
					return
				}
				select {
				case out <- msg.Data:
				case <-done:
					return
				}
			case <-done:
				return
			}
		}
	}()
	return &natsSub{sub: sub, out: out, done: done, logger: b.logger}, nil
}

func (b *natsBus) Close() error {
	// Drain flushes pending messages and unsubscribes before closing.
	return b.conn.Drain()
}

type natsSub struct {
	sub    *nats.Subscription
	out    chan []byte
	done   chan struct{}
	logger *zap.Logger
}

func (s *natsSub) Events() <-chan []byte { return s.out }

func (s *natsSub) Unsubscribe() {
	if err := s.sub.Unsubscribe(); err != nil {
		s.logger.Warn("nats: unsubscribe failed",
			zap.String("subject", s.sub.Subject),
			zap.Error(err))
	}
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}
