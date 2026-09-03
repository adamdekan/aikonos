package alerting

import (
	"testing"
	"time"

	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func makeEvent(tenant, eventType, resourceRef string, decision auditv1.PolicyDecision) *auditv1.AuditEvent {
	return &auditv1.AuditEvent{
		EventId:     "evt-1",
		TenantId:    tenant,
		EventType:   eventType,
		ResourceRef: resourceRef,
		Decision:    decision,
		OccurredAt:  timestamppb.Now(),
	}
}

func makeEventWithContext(tenant, eventType, resourceRef, reason string, decision auditv1.PolicyDecision) *auditv1.AuditEvent {
	ev := makeEvent(tenant, eventType, resourceRef, decision)
	s, _ := structpb.NewStruct(map[string]interface{}{"reason": reason})
	ev.Context = s
	return ev
}

// clock returns a func() time.Time fixed at t, advancing by d on each call.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func stepClock(start time.Time, step time.Duration) func() time.Time {
	cur := start
	return func() time.Time {
		t := cur
		cur = cur.Add(step)
		return t
	}
}

// ---- DENY → denial alert ----

func TestObserve_DenyYieldsDenialAlert(t *testing.T) {
	e := NewEvaluator(Config{DenyRateThreshold: 0})
	ev := makeEvent("tenant-a", "tool.invoke", "task/123", auditv1.PolicyDecision_DENY)
	ev.EventId = "eid-42"

	alerts := e.Observe(ev)

	if len(alerts) != 1 {
		t.Fatalf("want 1 alert, got %d", len(alerts))
	}
	a := alerts[0]
	if a.Rule != "denial" {
		t.Errorf("rule: want denial, got %s", a.Rule)
	}
	if a.Severity != SeverityWarning {
		t.Errorf("severity: want warning, got %s", a.Severity)
	}
	if a.TenantID != "tenant-a" {
		t.Errorf("tenant: want tenant-a, got %s", a.TenantID)
	}
	if a.Title != "tool.invoke" {
		t.Errorf("title: want tool.invoke, got %s", a.Title)
	}
	if a.EventID != "eid-42" {
		t.Errorf("event id: want eid-42, got %s", a.EventID)
	}
	if a.FiredAt.IsZero() {
		t.Error("FiredAt must not be zero")
	}
}

// ---- Detail carries ResourceRef and reason from context ----

func TestObserve_DenyDetailIncludesResourceRefAndReason(t *testing.T) {
	e := NewEvaluator(Config{DenyRateThreshold: 0})
	ev := makeEventWithContext("t1", "tool.invoke", "task/99", "insufficient scope", auditv1.PolicyDecision_DENY)

	alerts := e.Observe(ev)

	if len(alerts) == 0 {
		t.Fatal("expected alert")
	}
	detail := alerts[0].Detail
	if detail == "" {
		t.Error("Detail must not be empty")
	}
	// Must include the resource ref
	if !containsStr(detail, "task/99") {
		t.Errorf("Detail %q does not contain resource ref", detail)
	}
	// Must include the reason
	if !containsStr(detail, "insufficient scope") {
		t.Errorf("Detail %q does not contain reason", detail)
	}
}

func TestObserve_DenyDetailNoContext(t *testing.T) {
	e := NewEvaluator(Config{DenyRateThreshold: 0})
	ev := makeEvent("t1", "tool.invoke", "task/77", auditv1.PolicyDecision_DENY)

	alerts := e.Observe(ev)

	if len(alerts) == 0 {
		t.Fatal("expected alert")
	}
	// Must include the resource ref even without context
	if !containsStr(alerts[0].Detail, "task/77") {
		t.Errorf("Detail %q does not contain resource ref", alerts[0].Detail)
	}
}

// ---- Non-DENY decisions yield no alert and no window mutation ----

func TestObserve_AllowYieldsNoAlert(t *testing.T) {
	e := NewEvaluator(Config{DenyRateThreshold: 3, DenyRateWindow: time.Minute})
	ev := makeEvent("t1", "tool.invoke", "r1", auditv1.PolicyDecision_ALLOW)
	alerts := e.Observe(ev)
	if len(alerts) != 0 {
		t.Errorf("ALLOW: want 0 alerts, got %d", len(alerts))
	}
	// Window must be empty — send 3 more ALLOWs and still no rate alert
	for i := 0; i < 5; i++ {
		if got := e.Observe(ev); len(got) != 0 {
			t.Errorf("ALLOW iteration %d: want 0 alerts, got %d", i, len(got))
		}
	}
}

func TestObserve_ApprovalRequiredYieldsNoAlert(t *testing.T) {
	e := NewEvaluator(Config{DenyRateThreshold: 1, DenyRateWindow: time.Minute})
	ev := makeEvent("t1", "tool.invoke", "r1", auditv1.PolicyDecision_APPROVAL_REQUIRED)
	if got := e.Observe(ev); len(got) != 0 {
		t.Errorf("APPROVAL_REQUIRED: want 0 alerts, got %d", len(got))
	}
}

func TestObserve_StepUpRequiredYieldsNoAlert(t *testing.T) {
	e := NewEvaluator(Config{DenyRateThreshold: 1, DenyRateWindow: time.Minute})
	ev := makeEvent("t1", "tool.invoke", "r1", auditv1.PolicyDecision_STEP_UP_REQUIRED)
	if got := e.Observe(ev); len(got) != 0 {
		t.Errorf("STEP_UP_REQUIRED: want 0 alerts, got %d", len(got))
	}
}

// ---- Threshold crossing within window → rate alert ----

func TestObserve_ThresholdCrossing(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	// advance 1s per call so all events land within a 60s window
	clk := stepClock(base, time.Second)

	e := NewEvaluator(Config{
		DenyRateThreshold: 3,
		DenyRateWindow:    time.Minute,
		DenyRateCooldown:  5 * time.Minute,
	})
	e.withClock(clk)

	ev := makeEvent("t1", "tool.invoke", "r1", auditv1.PolicyDecision_DENY)

	// first two denies: denial alert only
	for i := 0; i < 2; i++ {
		alerts := e.Observe(ev)
		for _, a := range alerts {
			if a.Rule == "high_deny_rate" {
				t.Errorf("iteration %d: unexpected rate alert", i)
			}
		}
	}

	// third deny: denial + rate alert
	alerts := e.Observe(ev)
	hasDenial, hasRate := false, false
	for _, a := range alerts {
		if a.Rule == "denial" {
			hasDenial = true
		}
		if a.Rule == "high_deny_rate" {
			hasRate = true
			if a.Severity != SeverityCritical {
				t.Errorf("rate alert severity: want critical, got %s", a.Severity)
			}
		}
	}
	if !hasDenial {
		t.Error("want denial alert on 3rd deny")
	}
	if !hasRate {
		t.Error("want high_deny_rate alert on 3rd deny")
	}
}

// ---- Cooldown suppresses re-fire ----

func TestObserve_CooldownSuppressesRefire(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	cur := base
	clk := func() time.Time { return cur }

	e := NewEvaluator(Config{
		DenyRateThreshold: 3,
		DenyRateWindow:    time.Minute,
		DenyRateCooldown:  5 * time.Minute,
	})
	e.withClock(clk)

	ev := makeEvent("t1", "tool.invoke", "r1", auditv1.PolicyDecision_DENY)

	// fire threshold: 3 events close together → rate alert fires, lastRateFire = base+2s
	for i := 0; i < 3; i++ {
		cur = base.Add(time.Duration(i) * time.Second)
		e.Observe(ev) //nolint
	}

	// 4th deny within cooldown (10s after base) — no rate alert even though window ≥ threshold
	cur = base.Add(10 * time.Second)
	alerts := e.Observe(ev)
	for _, a := range alerts {
		if a.Rule == "high_deny_rate" {
			t.Error("rate alert must not re-fire within cooldown")
		}
	}

	// Advance past the window (> 1min) and past the cooldown (> 5min from lastRateFire≈base+2s).
	// Send 3 fresh denies within the new window to cross threshold again.
	refireBase := base.Add(6 * time.Minute)
	fired := false
	for i := 0; i < 3; i++ {
		cur = refireBase.Add(time.Duration(i) * time.Second)
		for _, a := range e.Observe(ev) {
			if a.Rule == "high_deny_rate" {
				fired = true
			}
		}
	}
	if !fired {
		t.Error("rate alert must fire again after cooldown expires once threshold is crossed")
	}
}

// ---- Window expiry resets the count ----

func TestObserve_WindowExpiryResetsCount(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	cur := base
	clk := func() time.Time { return cur }

	e := NewEvaluator(Config{
		DenyRateThreshold: 3,
		DenyRateWindow:    time.Minute,
		DenyRateCooldown:  10 * time.Minute,
	})
	e.withClock(clk)

	ev := makeEvent("t1", "tool.invoke", "r1", auditv1.PolicyDecision_DENY)

	// 2 denies at t=0
	for i := 0; i < 2; i++ {
		e.Observe(ev) //nolint
	}

	// advance past the window — those 2 events are now stale
	cur = base.Add(2 * time.Minute)

	// send 2 more — window only holds 2 fresh ones, threshold=3 not yet crossed
	for i := 0; i < 2; i++ {
		alerts := e.Observe(ev)
		for _, a := range alerts {
			if a.Rule == "high_deny_rate" {
				t.Error("rate alert must not fire: stale events pruned, count < threshold")
			}
		}
	}
}

// ---- Per-tenant isolation ----

func TestObserve_PerTenantIsolation(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	cur := base
	clk := func() time.Time { return cur }

	e := NewEvaluator(Config{
		DenyRateThreshold: 3,
		DenyRateWindow:    time.Minute,
		DenyRateCooldown:  5 * time.Minute,
	})
	e.withClock(clk)

	evA := makeEvent("tenant-a", "tool.invoke", "r1", auditv1.PolicyDecision_DENY)
	evB := makeEvent("tenant-b", "tool.invoke", "r1", auditv1.PolicyDecision_DENY)

	// Drive tenant-a to threshold
	for i := 0; i < 3; i++ {
		cur = base.Add(time.Duration(i) * time.Second)
		e.Observe(evA) //nolint
	}

	// Now tenant-b observes one deny — must NOT get a rate alert
	cur = base.Add(4 * time.Second)
	alerts := e.Observe(evB)
	for _, a := range alerts {
		if a.Rule == "high_deny_rate" {
			t.Error("tenant-b must not get rate alert due to tenant-a's denies")
		}
	}
}

// ---- Threshold ≤ 0 disables the rate rule ----

func TestObserve_ThresholdZeroDisablesRateRule(t *testing.T) {
	e := NewEvaluator(Config{
		DenyRateThreshold: 0,
		DenyRateWindow:    time.Minute,
	})

	ev := makeEvent("t1", "tool.invoke", "r1", auditv1.PolicyDecision_DENY)
	for i := 0; i < 100; i++ {
		for _, a := range e.Observe(ev) {
			if a.Rule == "high_deny_rate" {
				t.Error("rate rule must be disabled when threshold=0")
			}
		}
	}
}

func TestObserve_ThresholdNegativeDisablesRateRule(t *testing.T) {
	e := NewEvaluator(Config{
		DenyRateThreshold: -5,
		DenyRateWindow:    time.Minute,
	})

	ev := makeEvent("t1", "tool.invoke", "r1", auditv1.PolicyDecision_DENY)
	for i := 0; i < 10; i++ {
		for _, a := range e.Observe(ev) {
			if a.Rule == "high_deny_rate" {
				t.Error("rate rule must be disabled when threshold<0")
			}
		}
	}
}

// ---- helpers ----

func containsStr(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && func() bool {
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}())
}
