package netacl

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// RuleSource loads the active rules for a tenant (the DB repo, adapted).
type RuleSource interface {
	NetworkRules(ctx context.Context, tenant string) ([]Rule, error)
}

// GroupResolver returns the groups a user belongs to (OpenFGA-backed). It may
// return nil with no error when group resolution is unavailable (dev/no FGA),
// in which case GROUP-scoped rules simply do not match.
type GroupResolver func(ctx context.Context, tenant, user string) ([]string, error)

// Checker evaluates the access-list with a short-TTL cache over the rule source
// and group resolver, so the egress hot path doesn't hit Postgres/FGA per fetch.
type Checker struct {
	src    RuleSource
	groups GroupResolver
	ttl    time.Duration
	log    *zap.Logger

	mu         sync.Mutex
	ruleCache  map[string]cacheEntry[[]Rule]
	groupCache map[string]cacheEntry[[]string]
}

type cacheEntry[T any] struct {
	val T
	exp time.Time
}

// NewChecker builds a Checker; log may be nil.
func NewChecker(src RuleSource, groups GroupResolver, ttl time.Duration, log *zap.Logger) *Checker {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Checker{
		src:        src,
		groups:     groups,
		ttl:        ttl,
		log:        log,
		ruleCache:  map[string]cacheEntry[[]Rule]{},
		groupCache: map[string]cacheEntry[[]string]{},
	}
}

// Enabled reports whether a rule source is wired. When false the caller should
// fall back to today's behavior (no allow-list, SSRF guard only).
func (c *Checker) Enabled() bool { return c != nil && c.src != nil }

func (c *Checker) rules(ctx context.Context, tenant string, now time.Time) ([]Rule, error) {
	c.mu.Lock()
	if e, ok := c.ruleCache[tenant]; ok && now.Before(e.exp) {
		c.mu.Unlock()
		return e.val, nil
	}
	c.mu.Unlock()
	rules, err := c.src.NetworkRules(ctx, tenant)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.ruleCache[tenant] = cacheEntry[[]Rule]{val: rules, exp: now.Add(c.ttl)}
	c.mu.Unlock()
	return rules, nil
}

func (c *Checker) userGroups(ctx context.Context, tenant, user string, now time.Time) ([]string, error) {
	if c.groups == nil {
		return nil, nil
	}
	key := tenant + "\x00" + user
	c.mu.Lock()
	if e, ok := c.groupCache[key]; ok && now.Before(e.exp) {
		c.mu.Unlock()
		return e.val, nil
	}
	c.mu.Unlock()
	groups, err := c.groups(ctx, tenant, user)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.groupCache[key] = cacheEntry[[]string]{val: groups, exp: now.Add(c.ttl)}
	c.mu.Unlock()
	return groups, nil
}

// Decide resolves the action for (tenant, user, host). It fails closed: when
// the tenant's rules cannot be loaded it returns Deny, and when the user's
// groups cannot be resolved it returns the most restrictive action any
// membership could produce (DecideAnyGroups). Errors are not cached, so the
// next call retries. The Tool Proxy SSRF guard applies independently.
func (c *Checker) Decide(ctx context.Context, tenant, user, host string) Action {
	if !c.Enabled() {
		return Allow
	}
	now := time.Now()
	rules, err := c.rules(ctx, tenant, now)
	if err != nil {
		c.log.Warn("network access-list: rules could not be loaded, denying",
			zap.String("tenant", tenant), zap.String("host", host), zap.Error(err))
		return Deny
	}
	if !hasGroupRules(rules) {
		return Decide(rules, Principal{User: user}, host)
	}
	groups, err := c.userGroups(ctx, tenant, user, now)
	if err != nil {
		c.log.Warn("network access-list: group membership could not be resolved, applying the most restrictive group rule",
			zap.String("tenant", tenant), zap.String("host", host), zap.Error(err))
		return DecideAnyGroups(rules, user, host)
	}
	return Decide(rules, Principal{User: user, Groups: groups}, host)
}
