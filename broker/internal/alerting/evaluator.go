package alerting

import (
	"fmt"
	"sync"
	"time"

	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
)

// Config controls the Evaluator's rule behaviour.
// A threshold/window/cooldown of ≤0 disables the corresponding rule entirely.
//
// Defaults that keep the existing behavior unchanged when fields are omitted:
//   - DenyRateThreshold 0 → disabled (matches prior code)
//   - EscalationDenyCount 0 → disabled
//   - UnusualBaselineWindow 0 → disabled
//   - OffHoursDestructiveCooldown 0 → disabled
//   - MaxBaselineEntries 0 → treated as unlimited (no eviction)
type Config struct {
	// ── existing rule ────────────────────────────────────────────────────────
	DenyRateThreshold int
	DenyRateWindow    time.Duration
	DenyRateCooldown  time.Duration

	// ── escalation_pattern rule ──────────────────────────────────────────────
	// EscalationDenyCount ≤ 0 disables the rule. Default: disabled.
	// Fires when an actor accumulates ≥N denials within EscalationWindow, then
	// receives an allow/approval — the classic privilege-probing signature.
	EscalationDenyCount int
	EscalationWindow    time.Duration
	EscalationCooldown  time.Duration

	// ── unusual_actor_host rule ───────────────────────────────────────────────
	// UnusualBaselineWindow ≤ 0 disables the rule. Default: disabled.
	// Fires (warning) the first time an actor touches a host or tool not seen
	// for that actor within the window. Best-effort in-memory baseline; bounded
	// by MaxBaselineEntries per tenant/actor (0 = unlimited).
	UnusualBaselineWindow time.Duration
	UnusualCooldown       time.Duration
	MaxBaselineEntries    int

	// ── off_hours_destructive rule ────────────────────────────────────────────
	// OffHoursDestructiveCooldown ≤ 0 disables the rule. Default: disabled.
	// Fires (warning) when a destructive/infrastructure ALLOW occurs outside
	// business hours (hour <7 or ≥19 UTC, or Saturday/Sunday) — mirrors the OPA
	// business-hours notion.
	OffHoursDestructiveCooldown time.Duration
}

// ── per-tenant state ──────────────────────────────────────────────────────────

// tenantState holds all in-memory rule state for one tenant.
//
// Actor-keyed maps (actorDenials, actorEscFire, offHoursFire, unusualFire)
// accumulate one entry per distinct actor that has touched the system.  When
// MaxBaselineEntries > 0 a lightweight cap evicts the single oldest entry
// whenever the map would exceed the limit — the same policy used for actorSeen.
// The bound is therefore MaxBaselineEntries distinct actors per tenant per
// broker lifetime.  The baseline is best-effort and in-memory only; it resets
// on broker restart.
type tenantState struct {
	// high_deny_rate
	window       []time.Time
	lastRateFire time.Time

	// escalation_pattern: per-actor sliding window of denial times + last fire
	actorDenials map[string][]time.Time
	actorEscFire map[string]time.Time

	// unusual_actor_host: per-actor set of seen (host, tool) pairs + timestamps
	// for eviction. Key = actor, value = map[key]lastSeen.
	actorSeen map[string]map[string]time.Time

	// off_hours_destructive: per-actor last-fire time
	offHoursFire map[string]time.Time

	// unusual_actor_host: per-actor cooldown last-fire time, separate from
	// actorEscFire to prevent aliasing when an actor id starts with "unusual:".
	unusualFire map[string]time.Time
}

// Evaluator is a pure, concurrency-safe audit event consumer that emits Alerts
// for security-relevant patterns (denials, high deny-rate, escalation pattern,
// unusual actor/host access, and off-hours destructive operations).
type Evaluator struct {
	cfg     Config
	now     func() time.Time
	mu      sync.Mutex
	tenants map[string]*tenantState
}

// NewEvaluator creates an Evaluator with time.Now as the clock.
func NewEvaluator(cfg Config) *Evaluator {
	return &Evaluator{
		cfg:     cfg,
		now:     time.Now,
		tenants: make(map[string]*tenantState),
	}
}

// withClock replaces the clock — only called from tests in this package.
func (e *Evaluator) withClock(fn func() time.Time) {
	e.now = fn
}

func newTenantState() *tenantState {
	return &tenantState{
		actorDenials: make(map[string][]time.Time),
		actorEscFire: make(map[string]time.Time),
		actorSeen:    make(map[string]map[string]time.Time),
		offHoursFire: make(map[string]time.Time),
		unusualFire:  make(map[string]time.Time),
	}
}

// Observe processes one audit event and returns any Alerts it triggers.
// It is the only public mutation point; all rule logic is contained here.
func (e *Evaluator) Observe(ev *auditv1.AuditEvent) []Alert {
	now := e.now()
	detail := buildDetail(ev)

	e.mu.Lock()
	defer e.mu.Unlock()

	st := e.tenants[ev.TenantId]
	if st == nil {
		st = newTenantState()
		e.tenants[ev.TenantId] = st
	}

	var alerts []Alert

	actor := ev.ActorUserId
	toolID, host, effectClass := extractContext(ev)

	switch ev.Decision {
	case auditv1.PolicyDecision_DENY:
		// ── denial alert — always on DENY ────────────────────────────────────
		alerts = append(alerts, Alert{
			Rule:     "denial",
			Severity: SeverityWarning,
			TenantID: ev.TenantId,
			Title:    ev.EventType,
			Detail:   detail,
			EventID:  ev.EventId,
			FiredAt:  now,
		})

		// ── high_deny_rate window maintenance ────────────────────────────────
		st.window = append(st.window, now)
		if e.cfg.DenyRateWindow > 0 {
			cutoff := now.Add(-e.cfg.DenyRateWindow)
			keep := 0
			for _, t := range st.window {
				if !t.Before(cutoff) {
					break
				}
				keep++
			}
			st.window = st.window[keep:]
		}
		if e.cfg.DenyRateThreshold > 0 && len(st.window) >= e.cfg.DenyRateThreshold {
			since := now.Sub(st.lastRateFire)
			if st.lastRateFire.IsZero() || since >= e.cfg.DenyRateCooldown {
				alerts = append(alerts, Alert{
					Rule:     "high_deny_rate",
					Severity: SeverityCritical,
					TenantID: ev.TenantId,
					Title:    ev.EventType,
					Detail:   detail,
					EventID:  ev.EventId,
					FiredAt:  now,
				})
				st.lastRateFire = now
			}
		}

		// ── escalation_pattern: record denial for actor ───────────────────────
		if e.cfg.EscalationDenyCount > 0 && actor != "" {
			if _, exists := st.actorDenials[actor]; !exists {
				capActorMap(st.actorDenials, e.cfg.MaxBaselineEntries)
			}
			st.actorDenials[actor] = append(st.actorDenials[actor], now)
		}

	case auditv1.PolicyDecision_ALLOW, auditv1.PolicyDecision_APPROVAL_REQUIRED:
		// ── escalation_pattern: check if actor has ≥N recent denials ─────────
		if e.cfg.EscalationDenyCount > 0 && actor != "" {
			denials := st.actorDenials[actor]
			if e.cfg.EscalationWindow > 0 {
				cutoff := now.Add(-e.cfg.EscalationWindow)
				fresh := denials[:0]
				for _, t := range denials {
					if !t.Before(cutoff) {
						fresh = append(fresh, t)
					}
				}
				denials = fresh
				st.actorDenials[actor] = denials
			}
			if len(denials) >= e.cfg.EscalationDenyCount {
				lastFire := st.actorEscFire[actor]
				if lastFire.IsZero() || now.Sub(lastFire) >= e.cfg.EscalationCooldown {
					alerts = append(alerts, Alert{
						Rule:     "escalation_pattern",
						Severity: SeverityCritical,
						TenantID: ev.TenantId,
						Title:    ev.EventType,
						Detail:   fmt.Sprintf("actor %s: %d denials then allow", actor, len(denials)),
						EventID:  ev.EventId,
						FiredAt:  now,
					})
					st.actorEscFire[actor] = now
					// clear the actor's denial window after alert fires so a fresh
					// pattern needs to accumulate independently
					st.actorDenials[actor] = nil
				}
			}
		}

		// ── off_hours_destructive ─────────────────────────────────────────────
		if e.cfg.OffHoursDestructiveCooldown > 0 &&
			ev.Decision == auditv1.PolicyDecision_ALLOW &&
			isDestructiveClass(effectClass) &&
			isOffHours(now) {

			lastFire := st.offHoursFire[actor]
			if lastFire.IsZero() || now.Sub(lastFire) >= e.cfg.OffHoursDestructiveCooldown {
				alerts = append(alerts, Alert{
					Rule:     "off_hours_destructive",
					Severity: SeverityWarning,
					TenantID: ev.TenantId,
					Title:    ev.EventType,
					Detail:   fmt.Sprintf("actor %s tool %s effect_class %s at %s", actor, toolID, effectClass, now.UTC().Format("15:04 MST")),
					EventID:  ev.EventId,
					FiredAt:  now,
				})
				st.offHoursFire[actor] = now
			}
		}
	}

	// ── unusual_actor_host — fires on any non-DENY (ALLOW or APPROVAL_REQUIRED)
	// for the first time an actor touches a host/tool key in the window.
	if e.cfg.UnusualBaselineWindow > 0 && ev.Decision != auditv1.PolicyDecision_DENY && actor != "" {
		seenKey := unusualKey(toolID, host)
		if seenKey != "" {
			seen := st.actorSeen[actor]
			if seen == nil {
				seen = make(map[string]time.Time)
				st.actorSeen[actor] = seen
			}

			// evict stale entries from baseline
			cutoff := now.Add(-e.cfg.UnusualBaselineWindow)
			for k, t := range seen {
				if t.Before(cutoff) {
					delete(seen, k)
				}
			}

			_, known := seen[seenKey]
			if !known {
				lastFire := st.unusualFire[actor]
				if lastFire.IsZero() || now.Sub(lastFire) >= e.cfg.UnusualCooldown {
					alerts = append(alerts, Alert{
						Rule:     "unusual_actor_host",
						Severity: SeverityWarning,
						TenantID: ev.TenantId,
						Title:    ev.EventType,
						Detail:   fmt.Sprintf("actor %s first touch of %s", actor, seenKey),
						EventID:  ev.EventId,
						FiredAt:  now,
					})
					if e.cfg.UnusualCooldown > 0 {
						st.unusualFire[actor] = now
					}
				}
				// always add to baseline (even if cooldown suppressed the alert)
				if e.cfg.MaxBaselineEntries > 0 && len(seen) >= e.cfg.MaxBaselineEntries {
					// bounded eviction: drop the oldest entry
					var oldest string
					var oldestT time.Time
					for k, t := range seen {
						if oldest == "" || t.Before(oldestT) {
							oldest, oldestT = k, t
						}
					}
					delete(seen, oldest)
				}
				seen[seenKey] = now
			} else {
				// refresh baseline timestamp so recently-seen entries survive eviction
				seen[seenKey] = now
			}
		}
	}

	return alerts
}

// ── helpers ───────────────────────────────────────────────────────────────────

// buildDetail constructs the Detail string from ResourceRef and an optional
// "reason" field in the event's Context struct.
func buildDetail(ev *auditv1.AuditEvent) string {
	reason := ""
	if ev.Context != nil {
		if v, ok := ev.Context.AsMap()["reason"]; ok {
			if s, ok := v.(string); ok {
				reason = s
			}
		}
	}
	if reason != "" {
		return fmt.Sprintf("%s: %s", ev.ResourceRef, reason)
	}
	return ev.ResourceRef
}

// extractContext pulls tool_id, host, and effect_class from the free-form
// event context struct. These fields are set by the broker's audit interceptor
// and toolproxy on tool.invoke events; absent on other event types.
func extractContext(ev *auditv1.AuditEvent) (toolID, host, effectClass string) {
	if ev.Context == nil {
		return "", "", ""
	}
	m := ev.Context.AsMap()
	if v, ok := m["tool_id"]; ok {
		if s, ok := v.(string); ok {
			toolID = s
		}
	}
	if v, ok := m["host"]; ok {
		if s, ok := v.(string); ok {
			host = s
		}
	}
	if v, ok := m["effect_class"]; ok {
		if s, ok := v.(string); ok {
			effectClass = s
		}
	}
	return
}

// unusualKey builds the baseline lookup key for an (actor, host/tool) pair.
// Returns "" when neither is present (nothing to baseline).
func unusualKey(toolID, host string) string {
	if host != "" {
		return "host:" + host
	}
	if toolID != "" {
		return "tool:" + toolID
	}
	return ""
}

// isDestructiveClass reports whether an effect class warrants off-hours
// scrutiny (destructive or infrastructure).
func isDestructiveClass(effectClass string) bool {
	return effectClass == "destructive" || effectClass == "infrastructure"
}

// capActorMap evicts one arbitrary entry from m when len(m) >= max (max <= 0
// means no cap). Call before inserting a new key so the map never exceeds max.
func capActorMap[V any](m map[string]V, max int) {
	if max <= 0 || len(m) < max {
		return
	}
	for k := range m {
		delete(m, k)
		return
	}
}

// isOffHours reports whether t falls outside business hours per the OPA
// business-hours definition: hour <7 or ≥19 UTC, or Saturday/Sunday.
func isOffHours(t time.Time) bool {
	utc := t.UTC()
	h := utc.Hour()
	wd := utc.Weekday()
	if wd == time.Saturday || wd == time.Sunday {
		return true
	}
	return h < 7 || h >= 19
}
