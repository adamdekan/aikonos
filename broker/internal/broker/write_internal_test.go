package broker

// FU1 regression: write_internal has zero Routing → classifyDecision maps it to
// stepDeny (fail-closed).  No real tool maps to write_internal; this pins the
// behaviour so a future routing change is intentional.

import (
	"testing"

	"github.com/adamdekan/aikonos/broker/internal/effectclass"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// TestWriteInternal_ZeroRouting asserts RoutingForRego("write_internal") is the
// zero value — no auto, no approval, no step-up.
func TestWriteInternal_ZeroRouting(t *testing.T) {
	r := effectclass.RoutingForRego("write_internal")
	if r != (effectclass.Routing{}) {
		t.Errorf("RoutingForRego(write_internal) = %+v, want zero Routing", r)
	}
}

// TestWriteInternal_ClassifyDecision_IsDeny asserts that a policy.Decision with
// all-false flags (mirroring write_internal's OPA output) classifies as stepDeny.
// This exercises the classifyDecision path documented in FU1.
func TestWriteInternal_ClassifyDecision_IsDeny(t *testing.T) {
	dec := &policy.Decision{
		Allow:         false,
		Deny:          false,
		NeedsApproval: false,
		NeedsStepUp:   false,
	}
	if got := classifyDecision(dec); got != stepDeny {
		t.Errorf("classifyDecision(all-false) = %v, want stepDeny (%v)", got, stepDeny)
	}
}

// TestWriteInternal_Severity asserts write_internal has severity 4 (fail-closed,
// strictest) so Stricter(anything, WRITE_INTERNAL) always returns WRITE_INTERNAL.
func TestWriteInternal_Severity(t *testing.T) {
	classes := []planv1.EffectClass{
		planv1.EffectClass_READ_ONLY,
		planv1.EffectClass_WRITE_LOCAL,
		planv1.EffectClass_WRITE_EXTERNAL,
		planv1.EffectClass_NETWORK_EGRESS,
		planv1.EffectClass_CREDENTIAL_ACCESS,
		planv1.EffectClass_DESTRUCTIVE,
		planv1.EffectClass_INFRASTRUCTURE,
	}
	for _, c := range classes {
		got := effectclass.Stricter(c, planv1.EffectClass_WRITE_INTERNAL)
		if got != planv1.EffectClass_WRITE_INTERNAL {
			t.Errorf("Stricter(%v, WRITE_INTERNAL) = %v, want WRITE_INTERNAL", c, got)
		}
	}
}
