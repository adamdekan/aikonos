package broker

// FU5 tests: n-of-m approval threshold + separation-of-duty.
//
// Uses the existing fakeTaskStore, newFakeConfigStore, fakePolicyEngine, and
// fakeFGA patterns from the package. No Postgres or real OPA needed.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// newSvcWithCfgAndFGA builds a SandboxService with Config + a real policy
// engine pointed at the supplied FGA server URL (store-1 so FGAEnabled=true).
func newSvcWithCfgAndFGA(t *testing.T, tasks taskStore, pol planPolicyEngine, cfg Config, fgaURL string) *SandboxService {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pCfg := policy.Config{
		OPAEndpoint:     "http://unused",
		OpenFGAEndpoint: fgaURL,
		OpenFGAStoreID:  "store-1",
	}
	eng, err := policy.NewEngine(context.Background(), pCfg)
	if err != nil {
		t.Fatal(err)
	}
	return &SandboxService{deps: Deps{
		Logger: zap.NewNop(),
		Audit:  em,
		Tasks:  tasks,
		Policy: eng,
		Config: cfg,
	},
		policy: pol,
		// The approval_n tests exercise the approver-set / n-of-m logic, not
		// the skill gate. Disable the skill gate so the real FGA engine (wired
		// above for approver reads) does not deny steps for missing can_invoke
		// tuples.
		skillPolicy: &fakeSkillPolicy{enabled: false},
	}
}

// approvalPol returns a fakePolicyEngine that gives NEEDS_HUMAN for any step.
func approvalPol() *fakePolicyEngine {
	return &fakePolicyEngine{
		planAllow: true,
		toolDec: &policy.Decision{
			Allow:         false,
			NeedsApproval: true,
			PolicyRuleID:  "opa:tool_invocation",
		},
	}
}

// stepUpPol returns a fakePolicyEngine that gives NEEDS_STEP_UP.
func stepUpPol() *fakePolicyEngine {
	return &fakePolicyEngine{
		planAllow: true,
		toolDec: &policy.Decision{
			Allow:         false,
			NeedsApproval: true,
			NeedsStepUp:   true,
			PolicyRuleID:  "opa:tool_invocation",
		},
	}
}

// submitOneStep calls SubmitPlan with a single doc.write step and returns the
// approval request captured by the fake task store.
func submitOneStep(t *testing.T, svc *SandboxService, store *fakeTaskStore) *db.ApprovalRequest {
	t.Helper()
	_, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan:   makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "doc.write", nil)}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}
	if store.lastApproval == nil {
		t.Fatal("no approval request created")
	}
	return store.lastApproval
}

// ── Test 1: approval_required_n=2, NEEDS_HUMAN → requires_n=2, owner excluded ─

func TestApprovalN_ConfigN2_RequesterExcluded(t *testing.T) {
	// FGA disabled (no fgaURL) → approverSet falls back to [owner].
	// SoD then excludes the owner → approverSet is empty but requiresN stays 2.
	store := &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}
	cfg := newFakeConfigStore()
	cfg.values[testTenantUUID+":approval_required_n"] = "2"
	// Use newSandboxSvcForPlanWithConfig (Policy=nil → FGA disabled path).
	svc := newSandboxSvcForPlanWithConfig(store, approvalPol(), nil, cfg)

	ar := submitOneStep(t, svc, store)

	if ar.RequiresN != 2 {
		t.Errorf("RequiresN = %d, want 2", ar.RequiresN)
	}
	// Owner must be excluded from approver set under SoD.
	for _, a := range ar.ApproverSet {
		if a == "alice@example.com" {
			t.Errorf("owner must be excluded from approverSet, but found %q", a)
		}
	}
}

// ── Test 2: config n=1, NEEDS_STEP_UP → floor=2 overrides config ─────────────

func TestApprovalN_StepUpFloor2_OverridesConfigN1(t *testing.T) {
	store := &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}
	cfg := newFakeConfigStore()
	cfg.values[testTenantUUID+":approval_required_n"] = "1" // below step-up floor
	svc := newSandboxSvcForPlanWithConfig(store, stepUpPol(), nil, cfg)

	ar := submitOneStep(t, svc, store)

	if ar.RequiresN != 2 {
		t.Errorf("NEEDS_STEP_UP floor must clamp requiresN to 2, got %d", ar.RequiresN)
	}
}

// ── Test 3: config n=3, NEEDS_HUMAN → requiresN=3 (config raises above floor) ─

func TestApprovalN_ConfigN3_AppliedAboveFloor(t *testing.T) {
	store := &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}
	cfg := newFakeConfigStore()
	cfg.values[testTenantUUID+":approval_required_n"] = "3"
	svc := newSandboxSvcForPlanWithConfig(store, approvalPol(), nil, cfg)

	ar := submitOneStep(t, svc, store)

	if ar.RequiresN != 3 {
		t.Errorf("RequiresN = %d, want 3 (config raises above floor)", ar.RequiresN)
	}
}

// ── Test 4: config n=1, NEEDS_HUMAN → requiresN=1, owner in set (legacy) ─────

func TestApprovalN_DefaultN1_OwnerInSet(t *testing.T) {
	store := &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}
	svc := newSandboxSvcForPlanWithConfig(store, approvalPol(), nil, newFakeConfigStore())

	ar := submitOneStep(t, svc, store)

	if ar.RequiresN != 1 {
		t.Errorf("RequiresN = %d, want 1", ar.RequiresN)
	}
	// n=1 → no SoD exclusion → owner must be in the set.
	found := false
	for _, a := range ar.ApproverSet {
		if a == "alice@example.com" {
			found = true
		}
	}
	if !found {
		t.Errorf("owner must remain in approverSet when n=1, got %v", ar.ApproverSet)
	}
}

// ── Test 5: FGA approvers: n=2, two non-owner approvers from FGA tuples ───────

func TestApprovalN_FGAApprovers_SoDExcludesOwner(t *testing.T) {
	// fakeFGA returns "bob" and "carol" as approvers for the task, plus the
	// owner "alice" (as written by writeTaskRelations). SoD must exclude alice.
	taskID := validTaskUUID()
	fg := &fakeFGA{
		reads: map[string][]fgaKey{
			"task:" + taskID: {
				{User: "user:alice@example.com", Relation: "approver", Object: "task:" + taskID},
				{User: "user:bob@example.com", Relation: "approver", Object: "task:" + taskID},
				{User: "user:carol@example.com", Relation: "approver", Object: "task:" + taskID},
			},
		},
	}
	srv := fg.server(t)
	defer srv.Close()

	store := &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}
	// Override task UUID to match taskID used in FGA tuples.
	tid, _ := uuid.Parse(taskID)
	store.task.TaskID = tid

	cfg := newFakeConfigStore()
	cfg.values[testTenantUUID+":approval_required_n"] = "2"

	svc := newSvcWithCfgAndFGA(t, store, approvalPol(), cfg, srv.URL)

	_, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: taskID,
		Plan:   makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "doc.write", nil)}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}
	ar := store.lastApproval
	if ar == nil {
		t.Fatal("no approval request created")
	}
	if ar.RequiresN != 2 {
		t.Errorf("RequiresN = %d, want 2", ar.RequiresN)
	}
	// alice (owner) must not be in the set.
	for _, a := range ar.ApproverSet {
		if a == "alice@example.com" {
			t.Errorf("owner alice must be excluded from approverSet under SoD, got %v", ar.ApproverSet)
		}
	}
	// bob and carol must be present.
	hasB, hasC := false, false
	for _, a := range ar.ApproverSet {
		if a == "bob@example.com" {
			hasB = true
		}
		if a == "carol@example.com" {
			hasC = true
		}
	}
	if !hasB || !hasC {
		t.Errorf("want bob and carol in approverSet, got %v", ar.ApproverSet)
	}
}

// ── Test 6: expiry from config still respected alongside n ───────────────────

func TestApprovalN_ExpiryAndNBothFromConfig(t *testing.T) {
	store := &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}
	cfg := newFakeConfigStore()
	cfg.values[testTenantUUID+":approval_required_n"] = "2"
	cfg.values[testTenantUUID+":approval_expiry_hours"] = "1"
	svc := newSandboxSvcForPlanWithConfig(store, approvalPol(), nil, cfg)

	before := time.Now()
	ar := submitOneStep(t, svc, store)
	after := time.Now()

	if ar.RequiresN != 2 {
		t.Errorf("RequiresN = %d, want 2", ar.RequiresN)
	}
	lo := before.Add(50 * time.Minute)
	hi := after.Add(70 * time.Minute)
	if ar.ExpiresAt.Before(lo) || ar.ExpiresAt.After(hi) {
		t.Errorf("ExpiresAt = %v, want in [%v, %v]", ar.ExpiresAt, lo, hi)
	}
}
