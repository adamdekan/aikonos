package connector

import (
	"context"
	"sync"
	"time"
)

// PendingAuth is the server-only context for an in-flight OAuth round-trip,
// keyed by the opaque state value. The PKCE verifier lives here, never in a
// browser-readable cookie.
type PendingAuth struct {
	Provider Provider
	Tenant   string
	User     string
	Verifier string
	Scopes   []string
}

// PendingAuthStore records and single-use-claims in-flight OAuth round-trips.
// The in-memory StateStore fits a single replica; the DB-backed implementation
// (broker/internal/db.ConnectorStateRepo via the service adapter) lets
// BeginConnectorAuth and CompleteConnectorAuth land on different replicas.
type PendingAuthStore interface {
	Put(ctx context.Context, state string, a PendingAuth) error
	// Take returns the pending auth for state and deletes it (single use),
	// scoped to tenant. Returns false if unknown, expired, or cross-tenant.
	Take(ctx context.Context, tenant, state string) (PendingAuth, bool, error)
}

type entry struct {
	auth    PendingAuth
	expires time.Time
}

// StateStore is an in-memory, single-use, TTL'd map of state → PendingAuth.
type StateStore struct {
	mu  sync.Mutex
	m   map[string]entry
	ttl time.Duration
	now func() time.Time
}

// NewStateStore builds a store with the given entry TTL.
func NewStateStore(ttl time.Duration) *StateStore {
	return &StateStore{m: make(map[string]entry), ttl: ttl, now: time.Now}
}

// Put records a pending auth under state. It also sweeps expired entries so a
// long-running broker does not accumulate abandoned flows.
func (s *StateStore) Put(_ context.Context, state string, a PendingAuth) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	s.m[state] = entry{auth: a, expires: s.now().Add(s.ttl)}
	return nil
}

// Take returns the pending auth for state and deletes it (single use). It
// returns false if the state is unknown, expired, or minted for a different
// tenant. It also sweeps other expired entries so abandoned flows' verifier
// material is not retained past its TTL.
func (s *StateStore) Take(_ context.Context, tenant, state string) (PendingAuth, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	e, ok := s.m[state]
	if !ok {
		return PendingAuth{}, false, nil
	}
	delete(s.m, state)
	if s.now().After(e.expires) || e.auth.Tenant != tenant {
		return PendingAuth{}, false, nil
	}
	return e.auth, true, nil
}

func (s *StateStore) sweepLocked() {
	now := s.now()
	for k, e := range s.m {
		if now.After(e.expires) {
			delete(s.m, k)
		}
	}
}
