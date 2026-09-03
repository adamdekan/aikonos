package broker

// skill_gate_test.go — unit tests for the user skill gate in SubmitPlan.
//
// Contract (deny-by-default skill gate):
//   - personal task + FGA enabled + no grant    → step denied, plan DENIED, deny reason names the tool
//   - personal task + FGA enabled + grant        → APPROVED (read_only auto-class)
//   - agent-bound task (AgentID != nil)          → CheckFGA(can_invoke, skill:…) NOT called
//   - FGA disabled (FGAEnabled() false)          → CheckFGA NOT called, behaves as today
//   - CheckFGA returns error                     → step denied (fail closed)

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// ── fakeSkillPolicy ───────────────────────────────────────────────────────────

// fakeSkillPolicy is also used by approval_n_test.go to disable the skill gate
// while exercising the n-of-m approver-set logic.
type fakeSkillPolicy struct {
	enabled        bool
	granted        bool
	err            error
	checkFGACalls  int
	lastUser       string
	lastRelation   string
	lastObject     string
}

func (f *fakeSkillPolicy) FGAEnabled() bool { return f.enabled }

func (f *fakeSkillPolicy) CheckFGA(_ context.Context, user, relation, object string) (bool, error) {
	f.checkFGACalls++
	f.lastUser = user
	f.lastRelation = relation
	f.lastObject = object
	return f.granted, f.err
}

// ── helper ────────────────────────────────────────────────────────────────────

// newSvcWithSkillPolicy builds a SandboxService wired with the given
// planPolicyEngine (OPA fake) and skillGatePolicy (FGA fake).
func newSvcWithSkillPolicy(ts taskStore, pol planPolicyEngine, sp skillGatePolicy) *SandboxService {
	em, _ := audit.NewEmitter(context.Background(), audit.Config{})
	return &SandboxService{deps: Deps{
		Logger: zap.NewNop(),
		Audit:  em,
		Tasks:  ts,
	}, policy: pol, skillPolicy: sp}
}

// personalTask returns a fakeTaskStore whose task has no AgentID (personal north task).
func personalTask() *fakeTaskStore {
	return &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}
}

// ── Test 1: personal + FGA enabled + no grant → DENIED, reason names the tool ─

func TestUserSkillGate_NoGrant_Denied(t *testing.T) {
	sp := &fakeSkillPolicy{enabled: true, granted: false}
	ts := personalTask()
	// OPA would allow (read_only) — skill gate must override to deny.
	pol := &fakePolicyEngine{
		planAllow: true,
		toolDec:   &policy.Decision{Allow: true, PolicyRuleID: "opa:tool_invocation"},
	}
	svc := newSvcWithSkillPolicy(ts, pol, sp)

	result, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan:   makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "web.fetch", nil)}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}

	if result.Outcome != planv1.ValidationOutcome_DENIED {
		t.Errorf("outcome = %v, want DENIED", result.Outcome)
	}

	// deny reason must mention the tool id
	if len(result.StepDecisions) == 0 {
		t.Fatal("expected at least one step decision")
	}
	reason := result.StepDecisions[0].DenyReason
	if !strings.Contains(reason, "web.fetch") {
		t.Errorf("deny reason %q does not mention tool id 'web.fetch'", reason)
	}

	// FGA must have been consulted exactly once with the right relation/object
	if sp.checkFGACalls != 1 {
		t.Errorf("CheckFGA call count = %d, want 1", sp.checkFGACalls)
	}
	if sp.lastRelation != "can_invoke" {
		t.Errorf("CheckFGA relation = %q, want can_invoke", sp.lastRelation)
	}
	if sp.lastObject != "skill:web.fetch" {
		t.Errorf("CheckFGA object = %q, want skill:web.fetch", sp.lastObject)
	}
	if !strings.HasPrefix(sp.lastUser, "user:") {
		t.Errorf("CheckFGA user = %q, want user:<id> prefix", sp.lastUser)
	}
}

// ── Test 2: personal + FGA enabled + grant → APPROVED ────────────────────────

func TestUserSkillGate_WithGrant_Approved(t *testing.T) {
	sp := &fakeSkillPolicy{enabled: true, granted: true}
	ts := personalTask()
	pol := &fakePolicyEngine{
		planAllow: true,
		toolDec:   &policy.Decision{Allow: true, PolicyRuleID: "opa:tool_invocation"},
	}
	svc := newSvcWithSkillPolicy(ts, pol, sp)

	result, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan:   makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "web.fetch", nil)}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}

	if result.Outcome != planv1.ValidationOutcome_APPROVED {
		t.Errorf("outcome = %v, want APPROVED", result.Outcome)
	}
	if sp.checkFGACalls != 1 {
		t.Errorf("CheckFGA call count = %d, want 1", sp.checkFGACalls)
	}
}

// ── Test 3: agent-bound task → CheckFGA NOT called ───────────────────────────

func TestUserSkillGate_AgentBound_SkipsFGACheck(t *testing.T) {
	sp := &fakeSkillPolicy{enabled: true, granted: false} // would deny if consulted
	aid := uuid.MustParse(testAgentIDStr)
	t2 := newFakeTask(db.TaskStateCreated)
	t2.AgentID = &aid
	ts := &fakeTaskStore{task: t2}

	agentStore := &fakeAgentStoreForSkill{
		agent: &db.Agent{
			ID:       aid,
			TenantID: uuid.MustParse(testTenantUUID),
			Name:     "test-agent",
			Skills:   []string{"web.fetch"},
		},
	}

	em, _ := audit.NewEmitter(context.Background(), audit.Config{})
	svc := &SandboxService{deps: Deps{
		Logger: zap.NewNop(),
		Audit:  em,
		Tasks:  ts,
		Agents: agentStore,
	}, policy: allowPol(), skillPolicy: sp}

	result, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan:   makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "web.fetch", nil)}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}

	// Agent has web.fetch in Skills → CP2 passes; OPA allows → APPROVED.
	if result.Outcome != planv1.ValidationOutcome_APPROVED {
		t.Errorf("agent-bound task: outcome = %v, want APPROVED", result.Outcome)
	}
	// FGA skill gate must NOT have been consulted — agent-bound tasks use the
	// CP2 agent.Skills list, not per-user group membership.
	if sp.checkFGACalls != 0 {
		t.Errorf("agent-bound task: CheckFGA must not be called, got %d calls", sp.checkFGACalls)
	}
}

// ── Test 4: FGA disabled → CheckFGA NOT called, behaves as today ─────────────

func TestUserSkillGate_FGADisabled_PassThrough(t *testing.T) {
	sp := &fakeSkillPolicy{enabled: false, granted: false} // disabled; would deny if consulted
	ts := personalTask()
	pol := &fakePolicyEngine{
		planAllow: true,
		toolDec:   &policy.Decision{Allow: true, PolicyRuleID: "opa:tool_invocation"},
	}
	svc := newSvcWithSkillPolicy(ts, pol, sp)

	result, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan:   makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "web.fetch", nil)}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}

	if result.Outcome != planv1.ValidationOutcome_APPROVED {
		t.Errorf("FGA disabled: outcome = %v, want APPROVED (pass-through)", result.Outcome)
	}
	if sp.checkFGACalls != 0 {
		t.Errorf("FGA disabled: CheckFGA must not be called, got %d calls", sp.checkFGACalls)
	}
}

// ── Test 5: CheckFGA returns error → fail closed (step denied) ───────────────

// TestUserSkillGate_WithGrant_ApprovalClass verifies that the skill gate
// passing (grant present) does not swallow the OPA approval requirement:
// an approval-class tool (write_external) must still reach NEEDS_HUMAN /
// AWAITING_APPROVAL even when the user's skill is granted.
func TestUserSkillGate_WithGrant_ApprovalClass(t *testing.T) {
	sp := &fakeSkillPolicy{enabled: true, granted: true}
	ts := personalTask()
	// OPA says this write_external step needs human approval.
	pol := &fakePolicyEngine{
		planAllow: true,
		toolDec: &policy.Decision{
			Allow:         false,
			NeedsApproval: true,
			PolicyRuleID:  "opa:tool_invocation",
		},
	}
	svc := newSvcWithSkillPolicy(ts, pol, sp)

	result, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan:   makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "email.draft", nil)}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}

	// Skill gate passes → OPA's approval requirement must be preserved.
	if result.Outcome != planv1.ValidationOutcome_NEEDS_HUMAN {
		t.Errorf("outcome = %v, want NEEDS_HUMAN (approval-class tool with skill granted)", result.Outcome)
	}
	// Task must be awaiting approval, not denied.
	if ts.task.State != db.TaskStateAwaitingApproval {
		t.Errorf("task state = %v, want AwaitingApproval", ts.task.State)
	}
	// Skill gate must have been consulted (grant path).
	if sp.checkFGACalls != 1 {
		t.Errorf("CheckFGA call count = %d, want 1", sp.checkFGACalls)
	}
}

// ── Test 6: personal task + mcp: tool → per-user skill check SKIPPED ──────────
//
// mcp tools are authorized by the connector permitted_agent/permitted_group grant
// at InvokeTool, not by a per-user skill object. The object "skill:mcp:<conn>:<tool>"
// is an invalid OpenFGA object (colons in the id) and would 400 → fail closed,
// denying every MCP tool from an interactive (personal) session. The gate must
// not consult FGA for mcp: tools.
func TestUserSkillGate_PersonalMcpTool_SkipsFGACheck(t *testing.T) {
	sp := &fakeSkillPolicy{enabled: true, granted: false} // would deny if consulted
	ts := personalTask()
	pol := &fakePolicyEngine{
		planAllow: true,
		toolDec:   &policy.Decision{Allow: true, PolicyRuleID: "opa:tool_invocation"},
	}
	svc := newSvcWithSkillPolicy(ts, pol, sp)

	result, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan:   makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "mcp:d0235c1c-d5db-45ae-95e1-8f38f6e682a9:echo", nil)}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}

	if result.Outcome != planv1.ValidationOutcome_APPROVED {
		t.Errorf("personal mcp tool: outcome = %v, want APPROVED (FGA skill gate skipped, OPA allows)", result.Outcome)
	}
	if sp.checkFGACalls != 0 {
		t.Errorf("personal mcp tool: CheckFGA must not be called, got %d calls", sp.checkFGACalls)
	}
}

func TestUserSkillGate_FGAError_FailClosed(t *testing.T) {
	sp := &fakeSkillPolicy{
		enabled: true,
		granted: false,
		err:     errors.New("openfga unreachable"),
	}
	ts := personalTask()
	pol := &fakePolicyEngine{
		planAllow: true,
		toolDec:   &policy.Decision{Allow: true, PolicyRuleID: "opa:tool_invocation"},
	}
	svc := newSvcWithSkillPolicy(ts, pol, sp)

	result, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan:   makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "doc.write", nil)}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}

	if result.Outcome != planv1.ValidationOutcome_DENIED {
		t.Errorf("FGA error: outcome = %v, want DENIED (fail closed)", result.Outcome)
	}
}
