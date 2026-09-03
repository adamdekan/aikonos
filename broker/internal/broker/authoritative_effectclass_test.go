package broker

// FU2 tests: authoritative effect-class enforcement in SubmitPlan.
//
// A step declaring read_only for slack.post (authoritative=write_external) must
// route to NEEDS_HUMAN (approval), not APPROVED.  A correctly-declared step is
// unchanged.  An MCP tool keeps its plan-declared class.

import (
	"context"
	"testing"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/effectclass"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// Ensure opaLikePolicyEngine satisfies the planPolicyEngine interface.
var _ planPolicyEngine = (*opaLikePolicyEngine)(nil)

// opaLikePolicyEngine models OPA tool_invocation routing: the decision is
// derived from the effect class in the query, matching the real rego behaviour.
// This lets FU2 tests verify that SubmitPlan passes the routed (authoritative)
// class to DecideToolCall and that the outcome changes accordingly.
type opaLikePolicyEngine struct{}

func (o *opaLikePolicyEngine) DecidePlan(_ context.Context, _ *planv1.Plan, _ policy.PlanContext) (*policy.PlanDecision, error) {
	return &policy.PlanDecision{Allow: true}, nil
}

func (o *opaLikePolicyEngine) DecideToolCall(_ context.Context, q policy.ToolQuery) (*policy.Decision, error) {
	r := effectclass.RoutingForClass(q.EffectClass)
	return &policy.Decision{
		Allow:         r.Auto,
		NeedsApproval: r.RequireApproval,
		NeedsStepUp:   r.RequireStepUp,
		PolicyRuleID:  "opa:tool_invocation",
	}, nil
}

// makeStepWithClass creates a PlanStep with an explicit effect class.
func makeStepWithClass(seq int32, toolID string, ec planv1.EffectClass) *planv1.PlanStep {
	return &planv1.PlanStep{
		Seq:         seq,
		ToolId:      toolID,
		EffectClass: ec,
	}
}

// TestSubmitPlan_FU2_AuthoritativeOverridesUnderdeclared verifies that declaring
// read_only for slack.post (authoritative=write_external) routes to approval
// (NEEDS_HUMAN), not APPROVED.  The fake policy engine returns allow=true for
// the declared read_only class; the broker must escalate to approval based on
// the authoritative class.
func TestSubmitPlan_FU2_AuthoritativeOverridesUnderdeclared(t *testing.T) {
	taskStore := &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}
	svc := newSandboxSvcForPlan(taskStore, &opaLikePolicyEngine{}, nil)

	result, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan: makePlan(testTenantUUID, []*planv1.PlanStep{
			// Planner declares read_only, but slack.post is authoritatively write_external.
			makeStepWithClass(1, "slack.post", planv1.EffectClass_READ_ONLY),
		}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}

	if result.Outcome != planv1.ValidationOutcome_NEEDS_HUMAN {
		t.Errorf("outcome = %v, want NEEDS_HUMAN (approval) because slack.post is write_external", result.Outcome)
	}

	if len(taskStore.insertedRecords) != 1 {
		t.Fatalf("expected 1 inserted record, got %d", len(taskStore.insertedRecords))
	}
	rec := taskStore.insertedRecords[0]

	// Persisted EffectClass must reflect the routed (authoritative) class.
	if rec.EffectClass != planv1.EffectClass_WRITE_EXTERNAL.String() {
		t.Errorf("persisted EffectClass = %q, want %q", rec.EffectClass, planv1.EffectClass_WRITE_EXTERNAL.String())
	}

	// decision_trace must include the effect_class_override note.
	if rec.DecisionTrace == nil {
		t.Fatal("DecisionTrace is nil — expected effect_class_override note")
	}
	override, ok := rec.DecisionTrace["effect_class_override"]
	if !ok {
		t.Fatal("DecisionTrace missing 'effect_class_override'")
	}
	overrideMap, ok := override.(map[string]string)
	if !ok {
		t.Fatalf("effect_class_override type %T, want map[string]string", override)
	}
	if overrideMap["declared"] != planv1.EffectClass_READ_ONLY.String() {
		t.Errorf("override.declared = %q, want %q", overrideMap["declared"], planv1.EffectClass_READ_ONLY.String())
	}
	if overrideMap["authoritative"] != planv1.EffectClass_WRITE_EXTERNAL.String() {
		t.Errorf("override.authoritative = %q, want %q", overrideMap["authoritative"], planv1.EffectClass_WRITE_EXTERNAL.String())
	}
	if overrideMap["routed"] != planv1.EffectClass_WRITE_EXTERNAL.String() {
		t.Errorf("override.routed = %q, want %q", overrideMap["routed"], planv1.EffectClass_WRITE_EXTERNAL.String())
	}
}

// TestSubmitPlan_FU2_CorrectlyDeclaredUnchanged verifies that a step declaring
// the correct authoritative class is not modified.
func TestSubmitPlan_FU2_CorrectlyDeclaredUnchanged(t *testing.T) {
	taskStore := &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}
	svc := newSandboxSvcForPlan(taskStore, &opaLikePolicyEngine{}, nil)

	result, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan: makePlan(testTenantUUID, []*planv1.PlanStep{
			// doc.read is authoritatively read_only — correct declaration.
			makeStepWithClass(1, "doc.read", planv1.EffectClass_READ_ONLY),
		}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}

	if result.Outcome != planv1.ValidationOutcome_APPROVED {
		t.Errorf("outcome = %v, want APPROVED for correctly-declared read_only step", result.Outcome)
	}

	if len(taskStore.insertedRecords) != 1 {
		t.Fatalf("expected 1 inserted record, got %d", len(taskStore.insertedRecords))
	}
	rec := taskStore.insertedRecords[0]

	// No override note — class matched.
	if rec.DecisionTrace != nil {
		if _, hasOverride := rec.DecisionTrace["effect_class_override"]; hasOverride {
			t.Error("DecisionTrace should not have effect_class_override for a correctly-declared step")
		}
	}

	if rec.EffectClass != planv1.EffectClass_READ_ONLY.String() {
		t.Errorf("persisted EffectClass = %q, want READ_ONLY", rec.EffectClass)
	}
}

// TestSubmitPlan_FU2_MCPKeepsPlanDeclared verifies that an MCP tool's plan-declared
// class is kept unchanged (the broker has no ground truth for MCP semantics).
func TestSubmitPlan_FU2_MCPKeepsPlanDeclared(t *testing.T) {
	taskStore := &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}
	svc := newSandboxSvcForPlan(taskStore, &opaLikePolicyEngine{}, nil)

	result, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan: makePlan(testTenantUUID, []*planv1.PlanStep{
			// MCP tool declared as write_external.
			makeStepWithClass(1, "mcp:conn-abc:some-tool", planv1.EffectClass_WRITE_EXTERNAL),
		}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}

	// MCP tools have no broker-side authoritative class, so the plan-declared
	// write_external is routed unchanged. The opaLike engine routes
	// write_external to approval, so the outcome must be NEEDS_HUMAN — proving
	// the declared class actually reached DecideToolCall (not silently dropped
	// to UNSPECIFIED/deny).
	if result.Outcome != planv1.ValidationOutcome_NEEDS_HUMAN {
		t.Errorf("MCP outcome = %v, want NEEDS_HUMAN (write_external routes to approval)", result.Outcome)
	}

	if len(taskStore.insertedRecords) != 1 {
		t.Fatalf("expected 1 inserted record, got %d", len(taskStore.insertedRecords))
	}
	rec := taskStore.insertedRecords[0]

	if rec.DecisionTrace != nil {
		if _, hasOverride := rec.DecisionTrace["effect_class_override"]; hasOverride {
			t.Error("MCP tool must not have effect_class_override in trace")
		}
	}

	// Persisted class must be what the planner declared.
	if rec.EffectClass != planv1.EffectClass_WRITE_EXTERNAL.String() {
		t.Errorf("MCP persisted EffectClass = %q, want WRITE_EXTERNAL (plan-declared)", rec.EffectClass)
	}
}
