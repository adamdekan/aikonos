package broker

// FU3 tests: effect_class_routing config escalation in SubmitPlan.
//
// Rules under test:
//   - write_local:approval → an otherwise-APPROVED write_local step becomes NEEDS_HUMAN.
//   - config that would relax a step-up class (e.g. credential_access:approval) is a no-op.
//   - malformed config is ignored: no panic, no effect on routing.
//
// Reuses opaLikePolicyEngine (defined in authoritative_effectclass_test.go) and
// the fakeTaskStore / newSandboxSvcForPlanWithConfig helpers from the same package.

import (
	"context"
	"testing"

	"github.com/adamdekan/aikonos/broker/internal/db"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// TestSubmitPlan_FU3_WritLocalApprovalEscalates verifies that configuring
// write_local:approval turns an otherwise-APPROVED write_local step into NEEDS_HUMAN.
func TestSubmitPlan_FU3_WritLocalApprovalEscalates(t *testing.T) {
	taskStore := &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}

	// effect_class_routing: write_local → approval
	cfg := newFakeConfigStore()
	cfg.values[testTenantUUID+":effect_class_routing"] = "write_local:approval"

	svc := newSandboxSvcForPlanWithConfig(taskStore, &opaLikePolicyEngine{}, nil, cfg)

	result, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan: makePlan(testTenantUUID, []*planv1.PlanStep{
			// doc.write is authoritatively write_local → OPA returns allow=true.
			// The config knob must escalate to NEEDS_HUMAN.
			makeStepWithClass(1, "doc.write", planv1.EffectClass_WRITE_LOCAL),
		}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}

	if result.Outcome != planv1.ValidationOutcome_NEEDS_HUMAN {
		t.Errorf("outcome = %v, want NEEDS_HUMAN — config write_local:approval must escalate", result.Outcome)
	}

	if len(taskStore.insertedRecords) != 1 {
		t.Fatalf("expected 1 inserted record, got %d", len(taskStore.insertedRecords))
	}
	rec := taskStore.insertedRecords[0]

	// decision_trace must include the effect_class_override note with source=config.
	if rec.DecisionTrace == nil {
		t.Fatal("DecisionTrace is nil — expected effect_class_override note from config")
	}
	override, ok := rec.DecisionTrace["effect_class_override"]
	if !ok {
		t.Fatal("DecisionTrace missing 'effect_class_override'")
	}
	overrideMap, ok := override.(map[string]any)
	if !ok {
		t.Fatalf("effect_class_override type %T, want map[string]any", override)
	}
	if overrideMap["source"] != "config" {
		t.Errorf("override.source = %v, want \"config\"", overrideMap["source"])
	}
}

// TestSubmitPlan_FU3_RelaxNoOp verifies that a config posture that would relax an
// already-stricter decision is silently ignored (monotonic-stricter invariant).
// credential_access has step-up routing (severity 3); config approval (severity 2)
// must not lower the decision.
func TestSubmitPlan_FU3_RelaxNoOp(t *testing.T) {
	taskStore := &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}

	// config: credential_access:approval — would relax step-up to approval; must be ignored.
	cfg := newFakeConfigStore()
	cfg.values[testTenantUUID+":effect_class_routing"] = "credential_access:approval"

	svc := newSandboxSvcForPlanWithConfig(taskStore, &opaLikePolicyEngine{}, nil, cfg)

	result, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan: makePlan(testTenantUUID, []*planv1.PlanStep{
			makeStepWithClass(1, "doc.read", planv1.EffectClass_CREDENTIAL_ACCESS),
		}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}

	// opaLikePolicyEngine returns NeedsApproval+NeedsStepUp for credential_access.
	// The config posture is less strict, so it must have no effect.
	if result.Outcome != planv1.ValidationOutcome_NEEDS_STEP_UP {
		t.Errorf("outcome = %v, want NEEDS_STEP_UP — relaxing config must be a no-op", result.Outcome)
	}

	// No effect_class_override with source=config since nothing escalated.
	if len(taskStore.insertedRecords) == 1 {
		rec := taskStore.insertedRecords[0]
		if rec.DecisionTrace != nil {
			if ov, ok := rec.DecisionTrace["effect_class_override"].(map[string]any); ok {
				if ov["source"] == "config" {
					t.Error("effect_class_override with source=config must not appear when config would relax")
				}
			}
		}
	}
}

// TestSubmitPlan_FU3_MalformedConfigIgnored verifies that a malformed
// effect_class_routing value causes no panic and has no effect on routing.
func TestSubmitPlan_FU3_MalformedConfigIgnored(t *testing.T) {
	taskStore := &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}

	cfg := newFakeConfigStore()
	cfg.values[testTenantUUID+":effect_class_routing"] = "totally:bogus:garbage,,notaclass:approval"

	svc := newSandboxSvcForPlanWithConfig(taskStore, &opaLikePolicyEngine{}, nil, cfg)

	// doc.read is read_only → OPA returns allow=true → should remain APPROVED.
	result, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan: makePlan(testTenantUUID, []*planv1.PlanStep{
			makeStepWithClass(1, "doc.read", planv1.EffectClass_READ_ONLY),
		}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}

	if result.Outcome != planv1.ValidationOutcome_APPROVED {
		t.Errorf("outcome = %v, want APPROVED — malformed config must be ignored", result.Outcome)
	}
}

// TestSubmitPlan_FU3_DenyEscalation verifies that deny posture turns an
// auto-approved step into DENIED.
func TestSubmitPlan_FU3_DenyEscalation(t *testing.T) {
	taskStore := &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}

	cfg := newFakeConfigStore()
	cfg.values[testTenantUUID+":effect_class_routing"] = "read_only:deny"

	svc := newSandboxSvcForPlanWithConfig(taskStore, &opaLikePolicyEngine{}, nil, cfg)

	result, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan: makePlan(testTenantUUID, []*planv1.PlanStep{
			makeStepWithClass(1, "doc.read", planv1.EffectClass_READ_ONLY),
		}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}

	if result.Outcome != planv1.ValidationOutcome_DENIED {
		t.Errorf("outcome = %v, want DENIED — read_only:deny config must deny the step", result.Outcome)
	}
}
