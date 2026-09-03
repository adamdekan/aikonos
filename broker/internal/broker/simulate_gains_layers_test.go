package broker

// CP2 (C1+C2) red-first tests: SimulatePolicy must gain the three layers it
// currently skips (disabled_tools, overlay-disable, effect_class_routing) once
// it routes through the shared routeStep. Before routeStep exists these three
// tests fail — OPA alone allows the step and none of the three tenant-config /
// overlay layers ever run. After routeStep is wired in, all three deny.
//
// Reuses fakeSimPolicyEngine / newSimSvc from simulate_test.go and
// fakeOverlayStore from skill_overlay_cp3_test.go.

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// TestSimulatePolicy_CP2_DisabledToolsDenies verifies SimulatePolicy now
// denies a tool listed in the tenant's disabled_tools config, which OPA alone
// (the only layer wired before CP2) would have allowed.
func TestSimulatePolicy_CP2_DisabledToolsDenies(t *testing.T) {
	eng := &fakeSimPolicyEngine{toolDec: &policy.Decision{Allow: true}}
	svc := newSimSvc(t, eng, nil)

	cfg := newFakeConfigStore()
	cfg.values[testTenantUUID+":disabled_tools"] = "web.fetch"
	svc.deps.Config = cfg

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.SimulatePolicy(ctx, &brokerv1.SimulatePolicyRequest{
		SubjectUserId: "bob@example.com",
		ToolId:        "web.fetch",
		EffectClass:   planv1.EffectClass_READ_ONLY,
	})
	if err != nil {
		t.Fatalf("SimulatePolicy: %v", err)
	}
	if resp.Outcome != "DENY" {
		t.Errorf("outcome = %q, want DENY — disabled_tools must deny even though OPA alone allows", resp.Outcome)
	}
}

// TestSimulatePolicy_CP2_OverlayDisableDenies verifies SimulatePolicy now
// denies a tool the tenant overlay has disabled, which OPA alone would allow.
func TestSimulatePolicy_CP2_OverlayDisableDenies(t *testing.T) {
	reg := newTestToolRegistry()
	store := &fakeOverlayStore{
		rows: []db.SkillOverlay{{
			ToolID:       "doc.write",
			ExecutorKind: "builtin",
			Enabled:      false,
			EffectClass:  "write_local",
		}},
	}
	loader := newSkillOverlayLoader(reg, store)
	loader.ensureLoaded(context.Background(), testTenantUUID, zap.NewNop())

	eng := &fakeSimPolicyEngine{toolDec: &policy.Decision{Allow: true}}
	svc := newSimSvc(t, eng, nil)
	svc.deps.ToolRegistry = reg
	svc.deps.OverlayLoader = loader

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.SimulatePolicy(ctx, &brokerv1.SimulatePolicyRequest{
		SubjectUserId: "bob@example.com",
		ToolId:        "doc.write",
		EffectClass:   planv1.EffectClass_WRITE_LOCAL,
	})
	if err != nil {
		t.Fatalf("SimulatePolicy: %v", err)
	}
	if resp.Outcome != "DENY" {
		t.Errorf("outcome = %q, want DENY — overlay-disabled tool must deny even though OPA alone allows", resp.Outcome)
	}
}

// TestSimulatePolicy_CP2_EffectClassRoutingDenies verifies SimulatePolicy now
// honors the tenant's effect_class_routing config escalation, which OPA alone
// (auto-approving read_only) would not apply.
func TestSimulatePolicy_CP2_EffectClassRoutingDenies(t *testing.T) {
	eng := &fakeSimPolicyEngine{toolDec: &policy.Decision{Allow: true}}
	svc := newSimSvc(t, eng, nil)

	cfg := newFakeConfigStore()
	cfg.values[testTenantUUID+":effect_class_routing"] = "read_only:deny"
	svc.deps.Config = cfg

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.SimulatePolicy(ctx, &brokerv1.SimulatePolicyRequest{
		SubjectUserId: "bob@example.com",
		ToolId:        "doc.read",
		EffectClass:   planv1.EffectClass_READ_ONLY,
	})
	if err != nil {
		t.Fatalf("SimulatePolicy: %v", err)
	}
	if resp.Outcome != "DENY" {
		t.Errorf("outcome = %q, want DENY — effect_class_routing:deny must apply even though OPA alone allows", resp.Outcome)
	}
}
