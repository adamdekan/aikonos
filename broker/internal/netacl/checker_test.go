package netacl

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type fakeSource struct {
	rules []Rule
	err   error
	calls atomic.Int32
}

func (f *fakeSource) NetworkRules(context.Context, string) ([]Rule, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return f.rules, nil
}

var errDown = errors.New("connection refused")

func TestChecker_RuleLoadErrorDenies(t *testing.T) {
	src := &fakeSource{err: errDown}
	c := NewChecker(src, nil, time.Minute, nil)
	if got := c.Decide(context.Background(), "t1", "carol@example.com", "example.org"); got != Deny {
		t.Fatalf("unreadable rules must deny, got %s", got)
	}
	// The error is not cached: once the source recovers, its rules apply.
	src.err = nil
	if got := c.Decide(context.Background(), "t1", "carol@example.com", "example.org"); got != Allow {
		t.Fatalf("after recovery, no rules → Allow, got %s", got)
	}
	if n := src.calls.Load(); n != 2 {
		t.Fatalf("want 2 source calls (error not cached), got %d", n)
	}
}

func TestChecker_GroupErrorAppliesStrictestGroupRule(t *testing.T) {
	src := &fakeSource{rules: []Rule{
		r(ScopeTenant, "", Deny, "*"),
		r(ScopeTenant, "", Allow, "public.example"),
		r(ScopeGroup, "research", Allow, "arxiv.example"),
		r(ScopeGroup, "contractors", Deny, "public.example"),
	}}
	failing := func(context.Context, string, string) ([]string, error) { return nil, errDown }
	c := NewChecker(src, failing, time.Minute, nil)
	ctx := context.Background()
	if got := c.Decide(ctx, "t1", "carol@example.com", "arxiv.example"); got != Deny {
		t.Errorf("a group ALLOW must not apply without a resolved membership, got %s", got)
	}
	if got := c.Decide(ctx, "t1", "carol@example.com", "public.example"); got != Deny {
		t.Errorf("a group DENY must still apply when membership is unknown, got %s", got)
	}
}

func TestChecker_GroupsResolvedOnlyWhenGroupRulesExist(t *testing.T) {
	src := &fakeSource{rules: []Rule{r(ScopeTenant, "", Deny, "blocked.example")}}
	var calls atomic.Int32
	resolver := func(context.Context, string, string) ([]string, error) {
		calls.Add(1)
		return nil, errDown
	}
	c := NewChecker(src, resolver, time.Minute, nil)
	if got := c.Decide(context.Background(), "t1", "carol@example.com", "ok.example"); got != Allow {
		t.Errorf("tenant rules decide without groups, got %s", got)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("no GROUP rules: the resolver must not be called, got %d calls", n)
	}
}

func TestChecker_GroupMembershipApplies(t *testing.T) {
	src := &fakeSource{rules: []Rule{
		r(ScopeTenant, "", Deny, "*"),
		r(ScopeGroup, "research", Allow, "arxiv.example"),
	}}
	resolver := func(context.Context, string, string) ([]string, error) { return []string{"research"}, nil }
	c := NewChecker(src, resolver, time.Minute, nil)
	if got := c.Decide(context.Background(), "t1", "carol@example.com", "arxiv.example"); got != Allow {
		t.Errorf("a resolved member gets the group ALLOW, got %s", got)
	}
}
