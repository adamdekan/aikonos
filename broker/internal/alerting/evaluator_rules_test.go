package alerting

import (
	"testing"
	"time"

	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// makeEventFull constructs an AuditEvent with actor, host/tool in context, and an occurred_at.
func makeEventFull(tenant, actor, tool, host string, decision auditv1.PolicyDecision, ts time.Time, effectClass string) *auditv1.AuditEvent {
	ctx := map[string]interface{}{}
	if tool != "" {
		ctx["tool_id"] = tool
	}
	if host != "" {
		ctx["host"] = host
	}
	if effectClass != "" {
		ctx["effect_class"] = effectClass
	}
	s, _ := structpb.NewStruct(ctx)
	return &auditv1.AuditEvent{
		EventId:     "evt-x",
		TenantId:    tenant,
		EventType:   "tool.invoke",
		ResourceRef: "task/1",
		ActorUserId: actor,
		Decision:    decision,
		OccurredAt:  timestamppb.New(ts),
		Context:     s,
	}
}

// ── escalation_pattern ────────────────────────────────────────────────────────

func TestEscalation_NDeniesThenAllow(t *testing.T) {
	base := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	cur := base
	clk := func() time.Time { return cur }

	e := NewEvaluator(Config{
		DenyRateThreshold:           0,
		EscalationDenyCount:         3,
		EscalationWindow:            time.Minute,
		EscalationCooldown:          5 * time.Minute,
		UnusualBaselineWindow:       0, // disable other rules
		OffHoursDestructiveCooldown: 0,
	})
	e.withClock(clk)

	actor := "alice@example.com"

	// 3 denials for actor
	for i := 0; i < 3; i++ {
		cur = base.Add(time.Duration(i) * time.Second)
		ev := makeEventFull("t1", actor, "web.fetch", "", auditv1.PolicyDecision_DENY, cur, "")
		alerts := e.Observe(ev)
		for _, a := range alerts {
			if a.Rule == "escalation_pattern" {
				t.Errorf("escalation_pattern must not fire on denial %d (only on subsequent allow)", i)
			}
		}
	}

	// An allow for the same actor within the window → escalation alert
	cur = base.Add(10 * time.Second)
	ev := makeEventFull("t1", actor, "web.fetch", "", auditv1.PolicyDecision_ALLOW, cur, "")
	alerts := e.Observe(ev)
	found := false
	for _, a := range alerts {
		if a.Rule == "escalation_pattern" {
			found = true
			if a.Severity != SeverityCritical {
				t.Errorf("escalation_pattern severity: want critical, got %s", a.Severity)
			}
			if a.TenantID != "t1" {
				t.Errorf("escalation_pattern tenant: want t1, got %s", a.TenantID)
			}
		}
	}
	if !found {
		t.Error("want escalation_pattern alert after N denials + allow for same actor")
	}
}

func TestEscalation_NDeniesThenAllowDifferentActor(t *testing.T) {
	base := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	cur := base
	clk := func() time.Time { return cur }

	e := NewEvaluator(Config{
		DenyRateThreshold:   0,
		EscalationDenyCount: 3,
		EscalationWindow:    time.Minute,
		EscalationCooldown:  5 * time.Minute,
	})
	e.withClock(clk)

	// 3 denials for alice
	for i := 0; i < 3; i++ {
		cur = base.Add(time.Duration(i) * time.Second)
		ev := makeEventFull("t1", "alice@example.com", "web.fetch", "", auditv1.PolicyDecision_DENY, cur, "")
		e.Observe(ev) //nolint
	}

	// allow for bob — must NOT trigger escalation_pattern
	cur = base.Add(5 * time.Second)
	ev := makeEventFull("t1", "bob@example.com", "web.fetch", "", auditv1.PolicyDecision_ALLOW, cur, "")
	alerts := e.Observe(ev)
	for _, a := range alerts {
		if a.Rule == "escalation_pattern" {
			t.Error("escalation_pattern must not fire for different actor")
		}
	}
}

func TestEscalation_WindowExpiry(t *testing.T) {
	base := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	cur := base
	clk := func() time.Time { return cur }

	e := NewEvaluator(Config{
		DenyRateThreshold:   0,
		EscalationDenyCount: 2,
		EscalationWindow:    30 * time.Second,
		EscalationCooldown:  5 * time.Minute,
	})
	e.withClock(clk)

	actor := "alice@example.com"

	// 2 denials at t=0
	for i := 0; i < 2; i++ {
		cur = base.Add(time.Duration(i) * time.Second)
		ev := makeEventFull("t1", actor, "web.fetch", "", auditv1.PolicyDecision_DENY, cur, "")
		e.Observe(ev) //nolint
	}

	// allow at t=60s — denials expired (window=30s)
	cur = base.Add(60 * time.Second)
	ev := makeEventFull("t1", actor, "web.fetch", "", auditv1.PolicyDecision_ALLOW, cur, "")
	alerts := e.Observe(ev)
	for _, a := range alerts {
		if a.Rule == "escalation_pattern" {
			t.Error("escalation_pattern must not fire after denial window expires")
		}
	}
}

func TestEscalation_CooldownSuppressesRefire(t *testing.T) {
	base := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	cur := base
	clk := func() time.Time { return cur }

	e := NewEvaluator(Config{
		DenyRateThreshold:   0,
		EscalationDenyCount: 2,
		EscalationWindow:    time.Minute,
		EscalationCooldown:  5 * time.Minute,
	})
	e.withClock(clk)

	actor := "alice@example.com"

	// 2 denials then allow → alert fires
	for i := 0; i < 2; i++ {
		cur = base.Add(time.Duration(i) * time.Second)
		ev := makeEventFull("t1", actor, "web.fetch", "", auditv1.PolicyDecision_DENY, cur, "")
		e.Observe(ev) //nolint
	}
	cur = base.Add(3 * time.Second)
	ev := makeEventFull("t1", actor, "web.fetch", "", auditv1.PolicyDecision_ALLOW, cur, "")
	alerts := e.Observe(ev)
	fired := false
	for _, a := range alerts {
		if a.Rule == "escalation_pattern" {
			fired = true
		}
	}
	if !fired {
		t.Fatal("want initial escalation_pattern alert")
	}

	// same pattern again within cooldown
	for i := 0; i < 2; i++ {
		cur = base.Add(time.Duration(10+i) * time.Second)
		ev := makeEventFull("t1", actor, "web.fetch", "", auditv1.PolicyDecision_DENY, cur, "")
		e.Observe(ev) //nolint
	}
	cur = base.Add(20 * time.Second)
	ev2 := makeEventFull("t1", actor, "web.fetch", "", auditv1.PolicyDecision_ALLOW, cur, "")
	for _, a := range e.Observe(ev2) {
		if a.Rule == "escalation_pattern" {
			t.Error("escalation_pattern must not refire within cooldown")
		}
	}
}

func TestEscalation_Disabled(t *testing.T) {
	e := NewEvaluator(Config{DenyRateThreshold: 0, EscalationDenyCount: 0})
	actor := "alice@example.com"
	base := time.Now()
	for i := 0; i < 5; i++ {
		ev := makeEventFull("t1", actor, "web.fetch", "", auditv1.PolicyDecision_DENY, base, "")
		e.Observe(ev) //nolint
	}
	ev := makeEventFull("t1", actor, "web.fetch", "", auditv1.PolicyDecision_ALLOW, base, "")
	for _, a := range e.Observe(ev) {
		if a.Rule == "escalation_pattern" {
			t.Error("escalation_pattern must be disabled when EscalationDenyCount=0")
		}
	}
}

// ── unusual_actor_host ────────────────────────────────────────────────────────

func TestUnusualActorHost_FirstTimeToolFiresAlert(t *testing.T) {
	base := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	cur := base
	clk := func() time.Time { return cur }

	e := NewEvaluator(Config{
		DenyRateThreshold:     0,
		UnusualBaselineWindow: time.Hour,
		UnusualCooldown:       5 * time.Minute,
		MaxBaselineEntries:    100,
	})
	e.withClock(clk)

	actor := "alice@example.com"
	ev := makeEventFull("t1", actor, "web.fetch", "api.example.com", auditv1.PolicyDecision_ALLOW, cur, "")
	alerts := e.Observe(ev)
	found := false
	for _, a := range alerts {
		if a.Rule == "unusual_actor_host" {
			found = true
			if a.Severity != SeverityWarning {
				t.Errorf("unusual_actor_host severity: want warning, got %s", a.Severity)
			}
		}
	}
	if !found {
		t.Error("want unusual_actor_host alert on first-time tool/host for actor")
	}
}

func TestUnusualActorHost_SeenAgainNoAlert(t *testing.T) {
	base := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	cur := base
	clk := func() time.Time { return cur }

	e := NewEvaluator(Config{
		DenyRateThreshold:     0,
		UnusualBaselineWindow: time.Hour,
		UnusualCooldown:       0, // no cooldown so the only suppressor is baseline
		MaxBaselineEntries:    100,
	})
	e.withClock(clk)

	actor := "alice@example.com"
	host := "api.example.com"
	tool := "web.fetch"

	// first time — alert expected (establishes baseline)
	ev := makeEventFull("t1", actor, tool, host, auditv1.PolicyDecision_ALLOW, cur, "")
	e.Observe(ev) //nolint

	// second time — no alert
	cur = base.Add(time.Minute)
	ev2 := makeEventFull("t1", actor, tool, host, auditv1.PolicyDecision_ALLOW, cur, "")
	for _, a := range e.Observe(ev2) {
		if a.Rule == "unusual_actor_host" {
			t.Error("unusual_actor_host must not fire when host/tool already in baseline")
		}
	}
}

func TestUnusualActorHost_DifferentActorIsolated(t *testing.T) {
	base := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	cur := base
	clk := func() time.Time { return cur }

	e := NewEvaluator(Config{
		DenyRateThreshold:     0,
		UnusualBaselineWindow: time.Hour,
		UnusualCooldown:       0,
		MaxBaselineEntries:    100,
	})
	e.withClock(clk)

	// alice sees the host first
	ev := makeEventFull("t1", "alice@example.com", "web.fetch", "api.example.com", auditv1.PolicyDecision_ALLOW, cur, "")
	e.Observe(ev) //nolint

	// bob seeing the same host for the first time should still alert
	cur = base.Add(time.Second)
	ev2 := makeEventFull("t1", "bob@example.com", "web.fetch", "api.example.com", auditv1.PolicyDecision_ALLOW, cur, "")
	found := false
	for _, a := range e.Observe(ev2) {
		if a.Rule == "unusual_actor_host" {
			found = true
		}
	}
	if !found {
		t.Error("unusual_actor_host must fire for bob even though alice already visited the host")
	}
}

func TestUnusualActorHost_Disabled(t *testing.T) {
	e := NewEvaluator(Config{DenyRateThreshold: 0, UnusualBaselineWindow: 0})
	ev := makeEventFull("t1", "alice", "web.fetch", "host.example.com", auditv1.PolicyDecision_ALLOW, time.Now(), "")
	for _, a := range e.Observe(ev) {
		if a.Rule == "unusual_actor_host" {
			t.Error("unusual_actor_host must be disabled when UnusualBaselineWindow=0")
		}
	}
}

// TestUnusualActorHost_CooldownNoAliasingWithEscalation ensures that an actor
// whose id begins with "unusual:" does not alias into the escalation cooldown
// map — the two rules use separate maps since the fix.
func TestUnusualActorHost_CooldownNoAliasingWithEscalation(t *testing.T) {
	base := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	cur := base
	clk := func() time.Time { return cur }

	// Both rules enabled; cooldown long enough to suppress re-fires.
	e := NewEvaluator(Config{
		DenyRateThreshold:     0,
		EscalationDenyCount:   2,
		EscalationWindow:      time.Minute,
		EscalationCooldown:    10 * time.Minute,
		UnusualBaselineWindow: time.Hour,
		UnusualCooldown:       10 * time.Minute,
		MaxBaselineEntries:    100,
	})
	e.withClock(clk)

	// An actor whose id would have aliased with the old "unusual:"+actor key.
	prefixActor := "unusual:probe@example.com"
	normalActor := "probe@example.com"

	// Fire unusual_actor_host cooldown for prefixActor on its first host.
	ev1 := makeEventFull("t1", prefixActor, "web.fetch", "api.example.com", auditv1.PolicyDecision_ALLOW, cur, "")
	e.Observe(ev1) //nolint

	// normalActor gets 2 denials then an allow → should get escalation_pattern.
	for i := 0; i < 2; i++ {
		cur = base.Add(time.Duration(i+1) * time.Second)
		evd := makeEventFull("t1", normalActor, "web.fetch", "", auditv1.PolicyDecision_DENY, cur, "")
		e.Observe(evd) //nolint
	}
	cur = base.Add(5 * time.Second)
	evAllow := makeEventFull("t1", normalActor, "web.fetch", "", auditv1.PolicyDecision_ALLOW, cur, "")
	alerts := e.Observe(evAllow)
	foundEsc := false
	for _, a := range alerts {
		if a.Rule == "escalation_pattern" && a.TenantID == "t1" {
			foundEsc = true
		}
	}
	if !foundEsc {
		t.Error("escalation_pattern must fire for normalActor; cooldown aliasing would have suppressed it")
	}
}

// ── off_hours_destructive ─────────────────────────────────────────────────────

func TestOffHoursDestructive_WeekdayOffHours(t *testing.T) {
	// Monday 2024-01-01 at 06:00 UTC — hour=6 < 7 → off-hours
	ts := time.Date(2024, 1, 1, 6, 0, 0, 0, time.UTC) // Monday
	cur := ts
	e := NewEvaluator(Config{
		DenyRateThreshold:           0,
		OffHoursDestructiveCooldown: 5 * time.Minute,
	})
	e.withClock(func() time.Time { return cur })

	ev := makeEventFull("t1", "alice", "doc.write", "", auditv1.PolicyDecision_ALLOW, ts, "destructive")
	alerts := e.Observe(ev)
	found := false
	for _, a := range alerts {
		if a.Rule == "off_hours_destructive" {
			found = true
			if a.Severity != SeverityWarning {
				t.Errorf("off_hours_destructive severity: want warning, got %s", a.Severity)
			}
		}
	}
	if !found {
		t.Error("want off_hours_destructive alert on weekday off-hours destructive allow")
	}
}

func TestOffHoursDestructive_WeekdayLateNight(t *testing.T) {
	// Monday 2024-01-01 at 23:00 UTC — hour=23 ≥ 19 → off-hours
	ts := time.Date(2024, 1, 1, 23, 0, 0, 0, time.UTC)
	cur := ts
	e := NewEvaluator(Config{
		DenyRateThreshold:           0,
		OffHoursDestructiveCooldown: 5 * time.Minute,
	})
	e.withClock(func() time.Time { return cur })

	ev := makeEventFull("t1", "alice", "doc.write", "", auditv1.PolicyDecision_ALLOW, ts, "infrastructure")
	found := false
	for _, a := range e.Observe(ev) {
		if a.Rule == "off_hours_destructive" {
			found = true
		}
	}
	if !found {
		t.Error("want off_hours_destructive alert at 23:00 with infrastructure effect class")
	}
}

func TestOffHoursDestructive_Weekend(t *testing.T) {
	// Saturday 2024-01-06 at 12:00 UTC — weekend → off-hours
	ts := time.Date(2024, 1, 6, 12, 0, 0, 0, time.UTC) // Saturday
	cur := ts
	e := NewEvaluator(Config{
		DenyRateThreshold:           0,
		OffHoursDestructiveCooldown: 5 * time.Minute,
	})
	e.withClock(func() time.Time { return cur })

	ev := makeEventFull("t1", "alice", "doc.write", "", auditv1.PolicyDecision_ALLOW, ts, "destructive")
	found := false
	for _, a := range e.Observe(ev) {
		if a.Rule == "off_hours_destructive" {
			found = true
		}
	}
	if !found {
		t.Error("want off_hours_destructive alert on weekend destructive allow")
	}
}

func TestOffHoursDestructive_BusinessHoursNoAlert(t *testing.T) {
	// Monday 2024-01-01 at 10:00 UTC — business hours
	ts := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	cur := ts
	e := NewEvaluator(Config{
		DenyRateThreshold:           0,
		OffHoursDestructiveCooldown: 5 * time.Minute,
	})
	e.withClock(func() time.Time { return cur })

	ev := makeEventFull("t1", "alice", "doc.write", "", auditv1.PolicyDecision_ALLOW, ts, "destructive")
	for _, a := range e.Observe(ev) {
		if a.Rule == "off_hours_destructive" {
			t.Error("off_hours_destructive must not fire during business hours")
		}
	}
}

func TestOffHoursDestructive_NonDestructiveNoAlert(t *testing.T) {
	// Off-hours but read_only effect class → no alert
	ts := time.Date(2024, 1, 1, 3, 0, 0, 0, time.UTC)
	cur := ts
	e := NewEvaluator(Config{
		DenyRateThreshold:           0,
		OffHoursDestructiveCooldown: 5 * time.Minute,
	})
	e.withClock(func() time.Time { return cur })

	ev := makeEventFull("t1", "alice", "doc.read", "", auditv1.PolicyDecision_ALLOW, ts, "read_only")
	for _, a := range e.Observe(ev) {
		if a.Rule == "off_hours_destructive" {
			t.Error("off_hours_destructive must not fire for non-destructive effect class")
		}
	}
}

func TestOffHoursDestructive_DenyDecisionNoAlert(t *testing.T) {
	// Off-hours destructive but DENY decision → no alert (already caught by denial rule)
	ts := time.Date(2024, 1, 1, 3, 0, 0, 0, time.UTC)
	cur := ts
	e := NewEvaluator(Config{
		DenyRateThreshold:           0,
		OffHoursDestructiveCooldown: 5 * time.Minute,
	})
	e.withClock(func() time.Time { return cur })

	ev := makeEventFull("t1", "alice", "doc.write", "", auditv1.PolicyDecision_DENY, ts, "destructive")
	for _, a := range e.Observe(ev) {
		if a.Rule == "off_hours_destructive" {
			t.Error("off_hours_destructive must only fire on ALLOW, not DENY")
		}
	}
}

func TestOffHoursDestructive_CooldownSuppressesRefire(t *testing.T) {
	base := time.Date(2024, 1, 1, 3, 0, 0, 0, time.UTC) // off-hours
	cur := base
	clk := func() time.Time { return cur }

	e := NewEvaluator(Config{
		DenyRateThreshold:           0,
		OffHoursDestructiveCooldown: 5 * time.Minute,
	})
	e.withClock(clk)

	actor := "alice@example.com"
	ev := makeEventFull("t1", actor, "doc.write", "", auditv1.PolicyDecision_ALLOW, base, "destructive")

	// first fire
	fired := false
	for _, a := range e.Observe(ev) {
		if a.Rule == "off_hours_destructive" {
			fired = true
		}
	}
	if !fired {
		t.Fatal("want initial off_hours_destructive alert")
	}

	// same actor within cooldown
	cur = base.Add(2 * time.Minute)
	for _, a := range e.Observe(ev) {
		if a.Rule == "off_hours_destructive" {
			t.Error("off_hours_destructive must not refire within cooldown for same actor")
		}
	}

	// after cooldown expires
	cur = base.Add(6 * time.Minute)
	fired2 := false
	for _, a := range e.Observe(ev) {
		if a.Rule == "off_hours_destructive" {
			fired2 = true
		}
	}
	if !fired2 {
		t.Error("off_hours_destructive must fire again after cooldown expires")
	}
}

func TestOffHoursDestructive_Disabled(t *testing.T) {
	ts := time.Date(2024, 1, 1, 3, 0, 0, 0, time.UTC)
	e := NewEvaluator(Config{DenyRateThreshold: 0, OffHoursDestructiveCooldown: 0})
	e.withClock(func() time.Time { return ts })

	ev := makeEventFull("t1", "alice", "doc.write", "", auditv1.PolicyDecision_ALLOW, ts, "destructive")
	for _, a := range e.Observe(ev) {
		if a.Rule == "off_hours_destructive" {
			t.Error("off_hours_destructive must be disabled when cooldown=0")
		}
	}
}
