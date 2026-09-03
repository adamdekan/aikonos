// broker/internal/notify/memory_bus.go
// In-process Bus — real pub/sub within one broker process. Used by tests and
// usable for single-node dev where standing up NATS isn't worth it.
package notify

import (
	"context"
	"sync"
)

// MemoryBus is a goroutine-safe in-process event bus. Subscribers registered on
// a subject receive every payload published to that exact subject after they
// subscribe. Slow subscribers drop messages rather than block publishers.
type MemoryBus struct {
	mu   sync.Mutex
	subs map[string]map[*memorySub]struct{}
}

func NewMemoryBus() *MemoryBus {
	return &MemoryBus{subs: make(map[string]map[*memorySub]struct{})}
}

func (b *MemoryBus) Enabled() bool { return true }

func (b *MemoryBus) Publish(_ context.Context, subject string, payload []byte) error {
	b.mu.Lock()
	targets := make([]*memorySub, 0, len(b.subs[subject]))
	for s := range b.subs[subject] {
		targets = append(targets, s)
	}
	b.mu.Unlock()

	for _, s := range targets {
		// Copy so a subscriber can't mutate another's view.
		cp := append([]byte(nil), payload...)
		select {
		case s.out <- cp:
		default: // drop for a slow consumer; events are best-effort
		}
	}
	return nil
}

func (b *MemoryBus) Subscribe(subject string) (Subscription, error) {
	s := &memorySub{bus: b, subject: subject, out: make(chan []byte, 64)}
	b.mu.Lock()
	if b.subs[subject] == nil {
		b.subs[subject] = make(map[*memorySub]struct{})
	}
	b.subs[subject][s] = struct{}{}
	b.mu.Unlock()
	return s, nil
}

func (b *MemoryBus) Close() error { return nil }

type memorySub struct {
	bus     *MemoryBus
	subject string
	out     chan []byte
	once    sync.Once
}

func (s *memorySub) Events() <-chan []byte { return s.out }

func (s *memorySub) Unsubscribe() {
	s.once.Do(func() {
		s.bus.mu.Lock()
		if set := s.bus.subs[s.subject]; set != nil {
			delete(set, s)
			if len(set) == 0 {
				delete(s.bus.subs, s.subject)
			}
		}
		s.bus.mu.Unlock()
		close(s.out)
	})
}
