// broker/internal/ratelimit/limiter.go
// In-memory fixed-per-minute-window rate limiter for per-agent/provider/tenant
// enforcement (window = unix-seconds/60, not a sliding window — bursts at a
// minute boundary are a tuning concern, not a correctness bug). Policies are
// hot-reloadable via SetPolicies. Counters are atomic int64 values stored in a
// sync.Map keyed by (tenant, agent, provider, metric, minuteTS). State is
// in-process and per-replica, not shared across brokers — this is safe only
// because deployment is single-broker (docs/00-aikonos-architecture.md's
// one-broker-per-deployment invariant, enforced at startup by the advisory
// lock in broker/cmd/broker/singleton.go); a multi-replica deployment would
// need a shared store.
package ratelimit

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Policy describes one rate limit rule. RPM/TPM encoding:
//
//	-1 = unconstrained (no limit applied)
//	 0 = deny-all (every request rejected)
//	>0 = enforce limit
type Policy struct {
	TenantID string
	AgentID  string // "" = applies to all agents
	Provider string // "" = applies to all providers
	RPM      int32
	TPM      int32
}

type windowKey struct {
	tenantID   string
	agentID    string
	provider   string
	metricType string // "rpm" or "tpm"
	minuteTS   int64  // unix / 60
}

// Limiter is a thread-safe in-memory rate limiter. Create via New().
type Limiter struct {
	mu       sync.RWMutex
	policies []Policy
	windows  sync.Map // windowKey → *int64
	stop     chan struct{}
	meter    metric.Int64Counter
}

// New constructs a Limiter and starts the background window-eviction goroutine.
// meter may be nil; when nil, no metrics are recorded.
func New(meter metric.Int64Counter) *Limiter {
	l := &Limiter{
		stop:  make(chan struct{}),
		meter: meter,
	}
	go l.evictLoop()
	return l
}

// evictLoop removes stale per-minute windows every 30 seconds.
// A window is stale when its minute is more than 2 minutes in the past.
func (l *Limiter) evictLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			cutoff := time.Now().Unix()/60 - 2
			l.windows.Range(func(k, _ any) bool {
				if k.(windowKey).minuteTS < cutoff {
					l.windows.Delete(k)
				}
				return true
			})
		}
	}
}

// Close stops the background eviction goroutine.
func (l *Limiter) Close() {
	close(l.stop)
}

// SetPolicies atomically replaces the in-memory policy set.
// Passing nil clears all policies (no-limit state).
func (l *Limiter) SetPolicies(policies []Policy) {
	l.mu.Lock()
	l.policies = policies
	l.mu.Unlock()
}

// getOrCreateCounter returns the atomic counter for the given window key,
// creating a zero-valued counter if none exists.
func (l *Limiter) getOrCreateCounter(key windowKey) *int64 {
	var zero int64
	actual, _ := l.windows.LoadOrStore(key, &zero)
	return actual.(*int64)
}

// CheckToolInvocation evaluates all applicable RPM policies against the
// current per-agent and global-tenant counts and, only when every policy
// allows it, increments both counters for this call. A denied call therefore
// never consumes quota — the earlier increment-then-check order let a retry
// storm against a closed limit keep itself saturated by burning quota on
// calls that were rejected anyway.
//
// This check-then-increment order is itself racy: two concurrent calls can
// both read the same pre-increment count, both decide "allowed", and both
// increment — a momentary one-over-limit overshoot under concurrency. That is
// accepted deliberately (same tolerance as the fixed-window approximation
// documented above) rather than adding a CAS/retry loop or a mutex around the
// whole read-then-add span: the limiter is a fairness/abuse backstop, not a
// hard quota, and the overshoot is bounded to the number of concurrent
// racers, not unbounded like the previous always-consume bug.
func (l *Limiter) CheckToolInvocation(ctx context.Context, tenantID, agentID string) error {
	minuteTS := time.Now().Unix() / 60

	agentKey := windowKey{tenantID, agentID, "", "rpm", minuteTS}
	agentCount := atomic.LoadInt64(l.getOrCreateCounter(agentKey))

	globalKey := windowKey{tenantID, "", "", "rpm", minuteTS}
	globalCount := atomic.LoadInt64(l.getOrCreateCounter(globalKey))

	l.mu.RLock()
	policies := l.policies
	l.mu.RUnlock()

	for _, p := range policies {
		if p.TenantID != tenantID {
			continue
		}
		if p.RPM == -1 {
			continue // unconstrained
		}
		// Only evaluate RPM policies with no provider scope.
		if p.Provider != "" {
			continue
		}
		var count int64
		if p.AgentID == "" {
			// Global-tenant policy.
			count = globalCount
		} else if p.AgentID == agentID {
			// Per-agent policy.
			count = agentCount
		} else {
			continue // different agent
		}
		// count is this call's pre-increment value; count+1 is what the
		// counter would become if this call is allowed (mirrors the old
		// increment-then-compare `count > RPM` check, without the increment).
		if p.RPM == 0 || count+1 > int64(p.RPM) {
			l.recordHit(ctx, tenantID, agentID, "rpm", false)
			return status.Errorf(codes.ResourceExhausted, "rate limit exceeded: rpm")
		}
	}

	atomic.AddInt64(l.getOrCreateCounter(agentKey), 1)
	atomic.AddInt64(l.getOrCreateCounter(globalKey), 1)
	l.recordHit(ctx, tenantID, agentID, "rpm", true)
	return nil
}

// recordHit emits one enforcement-action count via the injected meter.
// No-op when the meter is nil (tests, or metrics disabled). No user_id label
// — unbounded cardinality belongs on the audit trail, not on metrics
// (mirrors broker/internal/metrics's RecordUsage convention).
func (l *Limiter) recordHit(ctx context.Context, tenantID, agentID, limitType string, allowed bool) {
	if l.meter == nil {
		return
	}
	result := "denied"
	if allowed {
		result = "allowed"
	}
	l.meter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("tenant", tenantID),
		attribute.String("agent", agentID),
		attribute.String("limit_type", limitType),
		attribute.String("result", result),
	))
}

// RecordLlmTokens increments TPM counters for the agent, provider, and
// global-tenant windows. Call after a successful LLM response.
func (l *Limiter) RecordLlmTokens(_ context.Context, tenantID, agentID, provider string, tokens int64) {
	minuteTS := time.Now().Unix() / 60
	atomic.AddInt64(l.getOrCreateCounter(windowKey{tenantID, agentID, "", "tpm", minuteTS}), tokens)
	atomic.AddInt64(l.getOrCreateCounter(windowKey{tenantID, "", provider, "tpm", minuteTS}), tokens)
	atomic.AddInt64(l.getOrCreateCounter(windowKey{tenantID, "", "", "tpm", minuteTS}), tokens)
}

// CheckLlmRequest checks TPM policies for all three applicable scopes
// (per-agent, per-provider, global-tenant). Returns (false, limitType) on
// the first violation; (true, "") when all policies pass.
func (l *Limiter) CheckLlmRequest(_ context.Context, tenantID, agentID, provider string) (bool, string) {
	minuteTS := time.Now().Unix() / 60
	agentTokens := atomic.LoadInt64(l.getOrCreateCounter(windowKey{tenantID, agentID, "", "tpm", minuteTS}))
	providerTokens := atomic.LoadInt64(l.getOrCreateCounter(windowKey{tenantID, "", provider, "tpm", minuteTS}))
	globalTokens := atomic.LoadInt64(l.getOrCreateCounter(windowKey{tenantID, "", "", "tpm", minuteTS}))

	l.mu.RLock()
	policies := l.policies
	l.mu.RUnlock()

	for _, p := range policies {
		if p.TenantID != tenantID {
			continue
		}
		if p.TPM == -1 {
			continue // unconstrained
		}
		switch {
		case p.AgentID != "" && p.AgentID == agentID && p.Provider == "":
			if p.TPM == 0 || agentTokens >= int64(p.TPM) {
				return false, "tpm_agent"
			}
		case p.AgentID == "" && p.Provider != "" && p.Provider == provider:
			if p.TPM == 0 || providerTokens >= int64(p.TPM) {
				return false, "tpm_provider"
			}
		case p.AgentID == "" && p.Provider == "":
			if p.TPM == 0 || globalTokens >= int64(p.TPM) {
				return false, "tpm_global"
			}
		}
	}
	return true, ""
}
