package broker

// CP2 tests: broker-side skill boundary enforcement in SubmitPlan.
//
// A gateway-driven task bound to an agent with Skills=[doc.read]:
//   - SubmitPlan with a doc.read step  → APPROVED
//   - SubmitPlan with a web.fetch step → DENIED by the broker (forged-plan case)
//
// Regression: a task with agent_id=nil (normal human north task) is completely
// unaffected — the new check is skipped and the plan still APPROVES.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// ── fakeAgentStoreForSkill ────────────────────────────────────────────────────

// fakeAgentStoreForSkill satisfies AgentStore with a single pre-seeded agent.
// getCalls counts how many times Get has been called — used by the null-agent
// regression test to assert the store is never consulted when agent_id is nil.
type fakeAgentStoreForSkill struct {
	agent    *db.Agent
	getCalls int
}

func (f *fakeAgentStoreForSkill) Create(_ context.Context, _ *db.Agent) error { return nil }
func (f *fakeAgentStoreForSkill) List(_ context.Context, _ string) ([]*db.Agent, error) {
	return nil, nil
}
func (f *fakeAgentStoreForSkill) Get(_ context.Context, _, id string) (*db.Agent, error) {
	f.getCalls++
	if f.agent != nil && f.agent.ID.String() == id {
		return f.agent, nil
	}
	return nil, db.ErrAgentNotFound
}
func (f *fakeAgentStoreForSkill) Update(_ context.Context, _ *db.Agent) error     { return nil }
func (f *fakeAgentStoreForSkill) Delete(_ context.Context, _, _ string) error     { return nil }
func (f *fakeAgentStoreForSkill) SetSoul(_ context.Context, _, _, _ string) error { return nil }

// ── helpers ───────────────────────────────────────────────────────────────────

// allowPol returns a fakePolicyEngine that approves every plan and step.
func allowPol() *fakePolicyEngine {
	return &fakePolicyEngine{
		planAllow: true,
		toolDec: &policy.Decision{
			Allow:        true,
			PolicyRuleID: "opa:allow",
		},
	}
}

// testAgentIDStr is a stable UUID string for the test agent.
const testAgentIDStr = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

// agentBoundTask returns a fakeTaskStore whose task has AgentID set.
func agentBoundTask() *fakeTaskStore {
	aid := uuid.MustParse(testAgentIDStr)
	t := newFakeTask(db.TaskStateCreated)
	t.AgentID = &aid
	t.GatewayManaged = true
	return &fakeTaskStore{task: t}
}

// unBoundTask returns a fakeTaskStore whose task has no agent binding (normal human task).
func unBoundTask() *fakeTaskStore {
	t := newFakeTask(db.TaskStateCreated)
	// AgentID is nil — normal north task, skill check must be skipped entirely.
	return &fakeTaskStore{task: t}
}

// newSvcWithAgent builds a SandboxService with an AgentStore containing one
// agent seeded with the given skills.
func newSvcWithAgent(t *testing.T, agentSkills []string, ts taskStore, pol planPolicyEngine) *SandboxService {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}

	agentStore := &fakeAgentStoreForSkill{
		agent: &db.Agent{
			ID:       uuid.MustParse(testAgentIDStr),
			TenantID: uuid.MustParse(testTenantUUID),
			Name:     "test-agent",
			Skills:   agentSkills,
		},
	}

	return &SandboxService{deps: Deps{
		Logger: zap.NewNop(),
		Audit:  em,
		Tasks:  ts,
		Agents: agentStore,
	}, policy: pol}
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestSkillBoundary_InSkill_Approved proves that a gateway task bound to an
// agent with Skills=[doc.read] is APPROVED when the plan contains only doc.read.
func TestSkillBoundary_InSkill_Approved(t *testing.T) {
	store := agentBoundTask()
	svc := newSvcWithAgent(t, []string{"doc.read"}, store, allowPol())

	result, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan:   makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "doc.read", nil)}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan unexpected error: %v", err)
	}
	if result.Outcome != planv1.ValidationOutcome_APPROVED {
		t.Errorf("doc.read in skills: want APPROVED, got %s (violations: %v)", result.Outcome, result.Violations)
	}
}

// TestSkillBoundary_OutOfSkill_Denied proves that a gateway task bound to an
// agent with Skills=[doc.read] is DENIED when the plan contains web.fetch —
// the broker refuses even though the gateway sent the plan (forged-plan case).
func TestSkillBoundary_OutOfSkill_Denied(t *testing.T) {
	store := agentBoundTask()
	svc := newSvcWithAgent(t, []string{"doc.read"}, store, allowPol())

	result, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan:   makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "web.fetch", nil)}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan unexpected error: %v", err)
	}
	if result.Outcome != planv1.ValidationOutcome_DENIED {
		t.Errorf("web.fetch not in skills: want DENIED, got %s", result.Outcome)
	}
	// The violation message must name the out-of-skills tool.
	found := false
	for _, v := range result.Violations {
		if containsTool(v, "web.fetch") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected violation mentioning 'web.fetch', got %v", result.Violations)
	}
}

// TestSkillBoundary_NullAgentID_Unaffected is the regression guard: a task
// with no agent_id (normal human north task) goes through SubmitPlan entirely
// unchanged — the skill-boundary check is skipped and the plan still APPROVES.
// The agent store must never be consulted (getCalls == 0).
func TestSkillBoundary_NullAgentID_Unaffected(t *testing.T) {
	store := unBoundTask()

	// Build the AgentStore separately so we can inspect its call count after the run.
	agentStore := &fakeAgentStoreForSkill{
		agent: &db.Agent{
			ID:       uuid.MustParse(testAgentIDStr),
			TenantID: uuid.MustParse(testTenantUUID),
			Name:     "test-agent",
			Skills:   []string{"doc.read"},
		},
	}

	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	svc := &SandboxService{deps: Deps{
		Logger: zap.NewNop(),
		Audit:  em,
		Tasks:  store,
		Agents: agentStore,
	}, policy: allowPol()}

	result, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		// web.fetch would be out-of-skills for the seeded agent — but the task
		// has no agent_id so the check must be completely skipped.
		Plan: makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "web.fetch", nil)}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan unexpected error: %v", err)
	}
	if result.Outcome != planv1.ValidationOutcome_APPROVED {
		t.Errorf("null agent_id: want APPROVED (skill check skipped), got %s", result.Outcome)
	}
	// The store must never have been consulted — the nil agent_id must short-circuit
	// before any Get call so the boundary cannot be accidentally evaluated.
	if agentStore.getCalls != 0 {
		t.Errorf("null agent_id: expected 0 Get calls on AgentStore, got %d", agentStore.getCalls)
	}
}

// newSvcWithAgentMcp builds a SandboxService whose seeded agent carries the
// given Skills and McpServers — used to exercise the mcp: branch of
// checkAgentSkills (attaching an MCP server, not listing an mcp: skill, is the
// grant).
func newSvcWithAgentMcp(t *testing.T, skills, mcpServers []string, ts taskStore, pol planPolicyEngine) *SandboxService {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	agentStore := &fakeAgentStoreForSkill{
		agent: &db.Agent{
			ID:         uuid.MustParse(testAgentIDStr),
			TenantID:   uuid.MustParse(testTenantUUID),
			Name:       "test-agent",
			Skills:     skills,
			McpServers: mcpServers,
		},
	}
	return &SandboxService{deps: Deps{
		Logger: zap.NewNop(),
		Audit:  em,
		Tasks:  ts,
		Agents: agentStore,
	}, policy: pol}
}

// TestSkillBoundary_McpAttached_Approved: an mcp: step whose connector is in the
// agent's mcp_servers is within the boundary even though no mcp: skill is listed.
func TestSkillBoundary_McpAttached_Approved(t *testing.T) {
	store := agentBoundTask()
	svc := newSvcWithAgentMcp(t, []string{"doc.read"}, []string{"conn-1"}, store, allowPol())

	result, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan:   makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "mcp:conn-1:search", nil)}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan unexpected error: %v", err)
	}
	if result.Outcome != planv1.ValidationOutcome_APPROVED {
		t.Errorf("attached mcp connector: want APPROVED, got %s (violations: %v)", result.Outcome, result.Violations)
	}
}

// TestSkillBoundary_McpNotAttached_Denied: an mcp: step for a connector the
// agent has NOT attached is outside the boundary → DENIED.
func TestSkillBoundary_McpNotAttached_Denied(t *testing.T) {
	store := agentBoundTask()
	svc := newSvcWithAgentMcp(t, []string{"doc.read"}, []string{"conn-1"}, store, allowPol())

	result, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan:   makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "mcp:conn-2:search", nil)}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan unexpected error: %v", err)
	}
	if result.Outcome != planv1.ValidationOutcome_DENIED {
		t.Errorf("unattached mcp connector: want DENIED, got %s", result.Outcome)
	}
	found := false
	for _, v := range result.Violations {
		if containsTool(v, "mcp:conn-2:search") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected violation mentioning 'mcp:conn-2:search', got %v", result.Violations)
	}
}

// TestSkillBoundary_McpMalformed_Denied: a malformed mcp id (no tool segment)
// is never within the boundary → DENIED.
func TestSkillBoundary_McpMalformed_Denied(t *testing.T) {
	store := agentBoundTask()
	svc := newSvcWithAgentMcp(t, []string{"doc.read"}, []string{"conn-1"}, store, allowPol())

	result, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan:   makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "mcp:conn-1", nil)}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan unexpected error: %v", err)
	}
	if result.Outcome != planv1.ValidationOutcome_DENIED {
		t.Errorf("malformed mcp id: want DENIED, got %s", result.Outcome)
	}
}

// containsTool is a minimal substring check for violation messages.
func containsTool(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
