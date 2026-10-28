// broker/internal/revocation/revocation.go
// AG-06: the cached view of user/tenant blocks and per-principal revocation
// epochs. A revocation epoch ("not before") voids every credential issued to
// the principal before it; a block is an infinite epoch. The north
// interceptor, owner-grant verification, InvokeTool and the capability check
// read through one Checker, so a block or revoke-all lands everywhere within
// the cache TTL (immediately on the replica that wrote it: writes reset the
// cache).
package revocation

import (
	"context"
	"sync"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/db"
)

// TTL bounds how stale a cached answer may be on another replica (AG-06: at
// most 60 s).
const TTL = 30 * time.Second

// Store is the persisted state. *db.RevocationRepo satisfies it.
type Store interface {
	UserState(ctx context.Context, tenantID, subject string) (bool, time.Time, error)
	AgentNotBefore(ctx context.Context, tenantID, agentID string) (time.Time, error)
	BlockUser(ctx context.Context, tenantID, subject, reason, by string) (bool, error)
	UnblockUser(ctx context.Context, tenantID, subject, by string) (bool, error)
	UserBlocks(ctx context.Context, tenantID string) (map[string]db.UserBlock, error)
	RevokeUserSessions(ctx context.Context, tenantID, subject string, at time.Time) error
	RevokeAgentSessions(ctx context.Context, tenantID, agentID string, at time.Time) error
}

// State is one principal's revocation state.
type State struct {
	Blocked   bool      // user or tenant block: nothing issued is valid
	NotBefore time.Time // credentials issued before this are void; zero = none
}

// Revoked reports whether a credential issued at iat is void. A zero iat (the
// credential does not say) counts as revoked once any epoch is set.
func (s State) Revoked(iat time.Time) bool {
	return s.Blocked || (!s.NotBefore.IsZero() && iat.Before(s.NotBefore))
}

type entry struct {
	st  State
	exp time.Time
}

// Checker caches Store reads for TTL. A nil *Checker reports nothing revoked
// (feature not wired: tests and dev without a database).
type Checker struct {
	Store Store
	Now   func() time.Time

	mu    sync.Mutex
	cache map[string]entry
}

// New builds a Checker over store.
func New(store Store) *Checker {
	return &Checker{Store: store, Now: time.Now, cache: map[string]entry{}}
}

// User returns the block and epoch of subject in tenantID. subject "" reads
// the tenant block only (a service run has no human subject). A read error is
// returned, never cached: callers fail closed.
func (c *Checker) User(ctx context.Context, tenantID, subject string) (State, error) {
	if c == nil {
		return State{}, nil
	}
	return c.cached("u|"+tenantID+"|"+subject, func() (State, error) {
		blocked, nb, err := c.Store.UserState(ctx, tenantID, subject)
		return State{Blocked: blocked, NotBefore: nb}, err
	})
}

// Agent returns the agent's epoch (agents have no block; AG-03 owns status).
func (c *Checker) Agent(ctx context.Context, tenantID, agentID string) (State, error) {
	if c == nil || agentID == "" {
		return State{}, nil
	}
	return c.cached("a|"+tenantID+"|"+agentID, func() (State, error) {
		nb, err := c.Store.AgentNotBefore(ctx, tenantID, agentID)
		return State{NotBefore: nb}, err
	})
}

// OnBehalfOf combines the subject (or tenant) block with the later of the
// subject's and the actor's epochs: a credential minted for a run is void when
// either party revoked after it was issued.
func (c *Checker) OnBehalfOf(ctx context.Context, tenantID, subject, actor string) (State, error) {
	u, err := c.User(ctx, tenantID, subject)
	if err != nil {
		return State{}, err
	}
	a, err := c.Agent(ctx, tenantID, actor)
	if err != nil {
		return State{}, err
	}
	if a.NotBefore.After(u.NotBefore) {
		u.NotBefore = a.NotBefore
	}
	return u, nil
}

func (c *Checker) cached(key string, load func() (State, error)) (State, error) {
	now := c.now()
	c.mu.Lock()
	e, ok := c.cache[key]
	c.mu.Unlock()
	if ok && now.Before(e.exp) {
		return e.st, nil
	}
	st, err := load()
	if err != nil {
		return State{}, err
	}
	c.mu.Lock()
	// ponytail: whole-map sweep at a size bound; an LRU if principals per TTL
	// window ever reach the tens of thousands.
	if len(c.cache) > 10000 {
		for k, v := range c.cache {
			if !now.Before(v.exp) {
				delete(c.cache, k)
			}
		}
	}
	c.cache[key] = entry{st: st, exp: now.Add(TTL)}
	c.mu.Unlock()
	return st, nil
}

// reset drops every cached answer after a local write.
func (c *Checker) reset() {
	c.mu.Lock()
	c.cache = map[string]entry{}
	c.mu.Unlock()
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// BlockUser blocks subject; false when already blocked.
func (c *Checker) BlockUser(ctx context.Context, tenantID, subject, reason, by string) (bool, error) {
	defer c.reset()
	return c.Store.BlockUser(ctx, tenantID, subject, reason, by)
}

// UnblockUser lifts subject's block; false when not blocked.
func (c *Checker) UnblockUser(ctx context.Context, tenantID, subject, by string) (bool, error) {
	defer c.reset()
	return c.Store.UnblockUser(ctx, tenantID, subject, by)
}

// UserBlocks lists active user blocks (uncached; admin listing only).
func (c *Checker) UserBlocks(ctx context.Context, tenantID string) (map[string]db.UserBlock, error) {
	return c.Store.UserBlocks(ctx, tenantID)
}

// RevokeUser moves subject's epoch to now and returns it.
func (c *Checker) RevokeUser(ctx context.Context, tenantID, subject string) (time.Time, error) {
	defer c.reset()
	at := c.now().UTC()
	return at, c.Store.RevokeUserSessions(ctx, tenantID, subject, at)
}

// RevokeAgent moves the agent's epoch to now and returns it.
func (c *Checker) RevokeAgent(ctx context.Context, tenantID, agentID string) (time.Time, error) {
	defer c.reset()
	at := c.now().UTC()
	return at, c.Store.RevokeAgentSessions(ctx, tenantID, agentID, at)
}
