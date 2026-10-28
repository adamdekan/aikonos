package revocation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/db"
)

type fakeStore struct {
	blocked bool
	userNB  time.Time
	agentNB time.Time
	err     error
	reads   int
}

func (f *fakeStore) UserState(context.Context, string, string) (bool, time.Time, error) {
	f.reads++
	return f.blocked, f.userNB, f.err
}
func (f *fakeStore) AgentNotBefore(context.Context, string, string) (time.Time, error) {
	f.reads++
	return f.agentNB, f.err
}
func (f *fakeStore) BlockUser(context.Context, string, string, string, string) (bool, error) {
	f.blocked = true
	return true, nil
}
func (f *fakeStore) UnblockUser(context.Context, string, string, string) (bool, error) {
	f.blocked = false
	return true, nil
}
func (f *fakeStore) UserBlocks(context.Context, string) (map[string]db.UserBlock, error) {
	return nil, nil
}
func (f *fakeStore) RevokeUserSessions(_ context.Context, _, _ string, at time.Time) error {
	f.userNB = at
	return nil
}
func (f *fakeStore) RevokeAgentSessions(_ context.Context, _, _ string, at time.Time) error {
	f.agentNB = at
	return nil
}

// A block written on another replica (straight to the store) is seen once the
// cached answer ages out, at most TTL (<= 60 s) later; a block written through
// this Checker is seen at once.
func TestChecker_BlockVisibleWithinTTL(t *testing.T) {
	if TTL > time.Minute {
		t.Fatalf("TTL %v exceeds the 60 s AG-06 bound", TTL)
	}
	ctx := context.Background()
	now := time.Unix(1_000_000, 0)
	store := &fakeStore{}
	c := New(store)
	c.Now = func() time.Time { return now }

	if st, _ := c.User(ctx, "t", "bob"); st.Blocked {
		t.Fatal("not blocked yet")
	}
	store.blocked = true // another replica
	if st, _ := c.User(ctx, "t", "bob"); st.Blocked {
		t.Fatal("cached answer should still be served inside the TTL")
	}
	now = now.Add(TTL)
	if st, _ := c.User(ctx, "t", "bob"); !st.Blocked {
		t.Fatal("block must be visible once the TTL has passed")
	}

	if _, err := c.UnblockUser(ctx, "t", "bob", "admin"); err != nil {
		t.Fatal(err)
	}
	if st, _ := c.User(ctx, "t", "bob"); st.Blocked {
		t.Fatal("a local write must reset the cache")
	}
}

// OnBehalfOf takes the later epoch of subject and actor; a read error is
// returned and not cached.
func TestChecker_OnBehalfOfAndErrors(t *testing.T) {
	ctx := context.Background()
	u, a := time.Unix(100, 0), time.Unix(200, 0)
	store := &fakeStore{userNB: u, agentNB: a}
	c := New(store)
	st, err := c.OnBehalfOf(ctx, "t", "bob", "agent")
	if err != nil || !st.NotBefore.Equal(a) {
		t.Fatalf("got %+v %v, want actor epoch", st, err)
	}
	if !st.Revoked(time.Unix(150, 0)) || st.Revoked(time.Unix(200, 0)) || !st.Revoked(time.Time{}) {
		t.Fatal("Revoked boundary wrong")
	}

	c2 := New(&fakeStore{err: errors.New("db down")})
	if _, err := c2.User(ctx, "t", "bob"); err == nil {
		t.Fatal("want error")
	}
	if len(c2.cache) != 0 {
		t.Fatal("errors must not be cached")
	}

	var nilChecker *Checker
	if st, err := nilChecker.OnBehalfOf(ctx, "t", "bob", "a"); err != nil || st.Blocked || !st.NotBefore.IsZero() {
		t.Fatal("nil checker revokes nothing")
	}
}
