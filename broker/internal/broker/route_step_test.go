package broker

// route_step_test.go — CP2 (C1+C2): table-driven test pinning the 5-layer
// escalation order (OPA → netacl → disabled_tools → overlay-disable →
// effect_class_routing) directly against routeStep. Both SubmitPlan and
// SimulatePolicy call routeStep — see TestRouteStep_BothCallersDelegate below
// — so pinning the order here pins it for both callers without duplicating
// full RPC fixtures per layer per caller.
//
// Per-layer, per-caller coverage already exists elsewhere and is unchanged:
// SubmitPlan in service_submitplan_test.go / tool_disable_test.go /
// skill_overlay_cp3_test.go / effect_class_routing_test.go; SimulatePolicy in
// simulate_test.go / simulate_gains_layers_test.go.

import (
	"context"
	"os"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/netacl"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

func TestRouteStep_LayerOrder(t *testing.T) {
	const tool = "doc.write"

	cases := []struct {
		name       string
		buildInput func() routeStepInput
		wantCat    stepCategory
		check      func(t *testing.T, tr routeTrace)
	}{
		{
			name: "opa_deny_short_circuits",
			buildInput: func() routeStepInput {
				return routeStepInput{
					Engine:      &fakePolicyEngine{toolDec: &policy.Decision{Deny: true, Reason: "opa says no"}},
					Reg:         newTestToolRegistry(),
					Cfg:         newFakeConfigStore(),
					ToolID:      tool,
					TenantID:    testTenantUUID,
					EffectClass: planv1.EffectClass_WRITE_LOCAL,
				}
			},
			wantCat: stepDeny,
			check: func(t *testing.T, tr routeTrace) {
				if tr.NetaclAction != "" || tr.DisabledToolsHit || tr.OverlayDisableHit || tr.ConfigOverride != nil {
					t.Errorf("no later layer should have fired: %+v", tr)
				}
			},
		},
		{
			name: "netacl_deny_overrides_allow",
			buildInput: func() routeStepInput {
				return routeStepInput{
					Engine:      &fakePolicyEngine{toolDec: &policy.Decision{Allow: true}},
					Reg:         newTestToolRegistry(),
					Cfg:         newFakeConfigStore(),
					ACL:         &staticACL{action: netacl.Deny},
					ToolID:      "web.fetch",
					TenantID:    testTenantUUID,
					EffectClass: planv1.EffectClass_READ_ONLY,
					NetaclHost:  "evil.example.com",
				}
			},
			wantCat: stepDeny,
			check: func(t *testing.T, tr routeTrace) {
				if !tr.NetaclMutated || tr.NetaclAction != netacl.Deny || tr.NetaclHost != "evil.example.com" {
					t.Errorf("netacl deny not captured: %+v", tr)
				}
			},
		},
		{
			name: "netacl_ask_escalates_allow_to_approval",
			buildInput: func() routeStepInput {
				return routeStepInput{
					Engine:      &fakePolicyEngine{toolDec: &policy.Decision{Allow: true}},
					Reg:         newTestToolRegistry(),
					Cfg:         newFakeConfigStore(),
					ACL:         &staticACL{action: netacl.Ask},
					ToolID:      "web.fetch",
					TenantID:    testTenantUUID,
					EffectClass: planv1.EffectClass_READ_ONLY,
					NetaclHost:  "unknown.example.com",
				}
			},
			wantCat: stepApproval,
			check: func(t *testing.T, tr routeTrace) {
				if !tr.NetaclMutated || tr.NetaclAction != netacl.Ask {
					t.Errorf("netacl ask escalation not captured: %+v", tr)
				}
			},
		},
		{
			// Order-sensitive: the "if dec.Allow" guard inside the netacl Ask
			// branch must see OPA's raw output (dec.Allow=false here), not some
			// later layer's state — proving netacl runs right after OPA, before
			// anything else could touch dec.
			name: "netacl_ask_noop_when_opa_already_approval",
			buildInput: func() routeStepInput {
				return routeStepInput{
					Engine:      &fakePolicyEngine{toolDec: &policy.Decision{NeedsApproval: true, Reason: "opa wants approval"}},
					Reg:         newTestToolRegistry(),
					Cfg:         newFakeConfigStore(),
					ACL:         &staticACL{action: netacl.Ask},
					ToolID:      "web.fetch",
					TenantID:    testTenantUUID,
					EffectClass: planv1.EffectClass_READ_ONLY,
					NetaclHost:  "unknown.example.com",
				}
			},
			wantCat: stepApproval,
			check: func(t *testing.T, tr routeTrace) {
				if tr.NetaclMutated {
					t.Errorf("netacl ask must be a no-op when OPA didn't Allow: %+v", tr)
				}
				if tr.NetaclAction != netacl.Ask || tr.NetaclHost == "" {
					t.Errorf("netacl check should still be recorded as checked (not mutated): %+v", tr)
				}
			},
		},
		{
			name: "disabled_tools_denies",
			buildInput: func() routeStepInput {
				cfg := newFakeConfigStore()
				cfg.values[testTenantUUID+":disabled_tools"] = tool
				return routeStepInput{
					Engine:      &fakePolicyEngine{toolDec: &policy.Decision{Allow: true}},
					Reg:         newTestToolRegistry(),
					Cfg:         cfg,
					ToolID:      tool,
					TenantID:    testTenantUUID,
					EffectClass: planv1.EffectClass_WRITE_LOCAL,
				}
			},
			wantCat: stepDeny,
			check: func(t *testing.T, tr routeTrace) {
				if !tr.DisabledToolsHit {
					t.Errorf("DisabledToolsHit should be true: %+v", tr)
				}
			},
		},
		{
			name: "overlay_disable_denies",
			buildInput: func() routeStepInput {
				reg := newTestToolRegistry()
				store := &fakeOverlayStore{rows: []db.SkillOverlay{{
					ToolID: tool, ExecutorKind: "builtin", Enabled: false, EffectClass: "write_local",
				}}}
				loader := newSkillOverlayLoader(reg, store)
				loader.ensureLoaded(context.Background(), testTenantUUID, zap.NewNop())
				return routeStepInput{
					Engine:             &fakePolicyEngine{toolDec: &policy.Decision{Allow: true}},
					Reg:                reg,
					Cfg:                newFakeConfigStore(),
					ToolID:             tool,
					TenantID:           testTenantUUID,
					EffectClass:        planv1.EffectClass_WRITE_LOCAL,
					OverlayGateEnabled: true,
				}
			},
			wantCat: stepDeny,
			check: func(t *testing.T, tr routeTrace) {
				if !tr.OverlayDisableHit {
					t.Errorf("OverlayDisableHit should be true: %+v", tr)
				}
			},
		},
		{
			name: "effect_class_routing_escalates_allow",
			buildInput: func() routeStepInput {
				cfg := newFakeConfigStore()
				cfg.values[testTenantUUID+":effect_class_routing"] = "read_only:approval"
				return routeStepInput{
					Engine:      &fakePolicyEngine{toolDec: &policy.Decision{Allow: true}},
					Reg:         newTestToolRegistry(),
					Cfg:         cfg,
					ToolID:      "doc.read",
					TenantID:    testTenantUUID,
					EffectClass: planv1.EffectClass_READ_ONLY,
				}
			},
			wantCat: stepApproval,
			check: func(t *testing.T, tr routeTrace) {
				if tr.ConfigOverride == nil || tr.ConfigOverride["source"] != "config" {
					t.Errorf("ConfigOverride should be set: %+v", tr)
				}
			},
		},
		{
			// Order-proving: layer 5 must run AFTER layer 3, so a weaker config
			// posture (severity 2) correctly no-ops against the already-forced
			// deny (severity 4) from disabled_tools — proving effect_class_routing
			// sees layer 3's mutation, not raw OPA. Swapping layers 3 and 5 in
			// route_step.go flips ConfigOverride from nil to non-nil here.
			name: "effect_class_routing_noop_after_disabled_tools_already_denied",
			buildInput: func() routeStepInput {
				cfg := newFakeConfigStore()
				cfg.values[testTenantUUID+":disabled_tools"] = tool
				cfg.values[testTenantUUID+":effect_class_routing"] = "read_only:approval"
				return routeStepInput{
					Engine:      &fakePolicyEngine{toolDec: &policy.Decision{Allow: true}},
					Reg:         newTestToolRegistry(),
					Cfg:         cfg,
					ToolID:      tool,
					TenantID:    testTenantUUID,
					EffectClass: planv1.EffectClass_READ_ONLY,
				}
			},
			wantCat: stepDeny,
			check: func(t *testing.T, tr routeTrace) {
				if !tr.DisabledToolsHit {
					t.Errorf("DisabledToolsHit should be true: %+v", tr)
				}
				if tr.ConfigOverride != nil {
					t.Errorf("ConfigOverride must stay nil — a weaker config posture is a no-op once disabled_tools already denied (this is the order proof): %+v", tr)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec, tr, err := routeStep(context.Background(), tc.buildInput())
			if err != nil {
				t.Fatalf("routeStep: %v", err)
			}
			if got := classifyDecision(dec); got != tc.wantCat {
				t.Errorf("classifyDecision = %v, want %v (dec=%+v)", got, tc.wantCat, dec)
			}
			tc.check(t, tr)
		})
	}
}

// TestRouteStep_BothCallersDelegate is a lightweight source-grep guard
// (same idiom as C4's planned pool.Acquire grep test): it fails if SubmitPlan
// or SimulatePolicy stop calling routeStep and drift back to independently
// inlined escalation logic — the exact bug C1 exists to fix. This is what
// lets TestRouteStep_LayerOrder above stand in for "the order is locked for
// both callers": there is exactly one routeStep, and both call it.
func TestRouteStep_BothCallersDelegate(t *testing.T) {
	for _, f := range []string{"service_plan.go", "simulate.go"} {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if !strings.Contains(string(body), "routeStep(ctx, routeStepInput{") {
			t.Errorf("%s no longer calls routeStep — SubmitPlan and SimulatePolicy must share the same escalation pipeline", f)
		}
	}
}
