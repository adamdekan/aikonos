package broker

// CP1 tests: verify that SubmitPlan captures PolicyReason + DecisionTrace on
// each persisted step. Record-only — outcomes/transitions must be unchanged.
//
// Uses narrow fakes for the taskStore and planPolicyEngine interfaces so
// no Postgres or OPA process is needed.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/netacl"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// ── fake task store ───────────────────────────────────────────────────────────

type fakeTaskStore struct {
	stubTaskStore
	task            *db.Task
	getErr          error
	insertedRecords []db.PlanStepRecord
	transitions     [][2]db.TaskState
	lastApproval    *db.ApprovalRequest
}

func (f *fakeTaskStore) Get(_ context.Context, _, _ string) (*db.Task, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.task, nil
}

func (f *fakeTaskStore) Transition(_ context.Context, _, _ string, from, to db.TaskState) error {
	f.transitions = append(f.transitions, [2]db.TaskState{from, to})
	f.task.State = to
	return nil
}

func (f *fakeTaskStore) InsertPlanSteps(_ context.Context, _ string, steps []db.PlanStepRecord) error {
	f.insertedRecords = append(f.insertedRecords, steps...)
	return nil
}

func (f *fakeTaskStore) CreateApprovalRequest(_ context.Context, req *db.ApprovalRequest) (uuid.UUID, error) {
	f.lastApproval = req
	return uuid.New(), nil
}

func (f *fakeTaskStore) ListMintableSteps(_ context.Context, _, _ string) ([]db.ExecutableStep, error) {
	return nil, nil
}

func (f *fakeTaskStore) GetPlanStepTraces(_ context.Context, _, _ string) ([]db.PlanStepTrace, error) {
	return nil, nil
}

// ── fake policy engine ────────────────────────────────────────────────────────

type fakePolicyEngine struct {
	planAllow bool
	toolDec   *policy.Decision
}

func (f *fakePolicyEngine) DecidePlan(_ context.Context, _ *planv1.Plan, _ policy.PlanContext) (*policy.PlanDecision, error) {
	return &policy.PlanDecision{Allow: f.planAllow}, nil
}

func (f *fakePolicyEngine) DecideToolCall(_ context.Context, q policy.ToolQuery) (*policy.Decision, error) {
	// Mirror the OPA contract: fga_decision="deny" always produces a deny,
	// regardless of the pre-configured toolDec. This lets skill-gate tests
	// exercise the real code path without a running OPA process.
	if q.FGADecision == "deny" {
		return &policy.Decision{
			Allow: false,
			Deny:  true,
			// Keep in sync with the fga_decision=="deny" deny_reasons rule in policies/opa/tool_invocation.rego.
			Reason:       fmt.Sprintf("You do not have access to tool %q. Ask a tenant admin to grant skill access via a group.", q.ToolID),
			PolicyRuleID: "opa:tool_invocation",
		}, nil
	}
	return f.toolDec, nil
}

// ── fake ACL ──────────────────────────────────────────────────────────────────

// staticACL satisfies the aclDecider interface with a fixed action.
type staticACL struct{ action netacl.Action }

func (a *staticACL) Enabled() bool { return true }
func (a *staticACL) Decide(_ context.Context, _, _, _ string) netacl.Action {
	return a.action
}

// ── helpers ───────────────────────────────────────────────────────────────────

func newFakeTask(state db.TaskState) *db.Task {
	tid, _ := uuid.Parse(validTaskUUID())
	tid2, _ := uuid.Parse(testTenantUUID)
	return &db.Task{
		TaskID:      tid,
		TenantID:    tid2,
		OwnerUserID: "alice@example.com",
		State:       state,
	}
}

func validTaskUUID() string { return "123e4567-e89b-12d3-a456-426614174001" }

func makePlan(tenantID string, steps []*planv1.PlanStep) *planv1.Plan {
	return &planv1.Plan{
		PlanId:   "plan-1",
		TenantId: tenantID,
		Steps:    steps,
	}
}

func makeStep(seq int32, toolID string, args map[string]any) *planv1.PlanStep {
	s := &planv1.PlanStep{
		Seq:         seq,
		ToolId:      toolID,
		EffectClass: planv1.EffectClass_READ_ONLY,
	}
	if args != nil {
		s.Args, _ = structpb.NewStruct(args)
	}
	return s
}

func newSandboxSvcForPlan(tasks taskStore, pol planPolicyEngine, acl aclDecider) *SandboxService {
	em, _ := audit.NewEmitter(context.Background(), audit.Config{})
	return &SandboxService{deps: Deps{
		Logger: zap.NewNop(),
		Audit:  em,
		Tasks:  tasks,
		ACL:    acl,
	}, policy: pol}
}

func newSandboxSvcForPlanWithConfig(tasks taskStore, pol planPolicyEngine, acl aclDecider, cfg Config) *SandboxService {
	em, _ := audit.NewEmitter(context.Background(), audit.Config{})
	return &SandboxService{deps: Deps{
		Logger: zap.NewNop(),
		Audit:  em,
		Tasks:  tasks,
		ACL:    acl,
		Config: cfg,
	}, policy: pol}
}

// ── F15 test: NotFound sentinel propagates as codes.NotFound ──────────────────

// TestSubmitPlan_TaskNotFound verifies that a missing task (repo Get returning
// an error matching db.ErrNotFound) surfaces as codes.NotFound rather than the
// codes.Internal a dead pgx.ErrNoRows comparison used to produce.
func TestSubmitPlan_TaskNotFound(t *testing.T) {
	taskStore := &fakeTaskStore{getErr: db.ErrNotFound}
	svc := newSandboxSvcForPlan(taskStore, &fakePolicyEngine{}, nil)

	_, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan:   makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "doc.write", nil)}),
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("missing task: want codes.NotFound, got %v", err)
	}
}

// ── CP1 test 1: denied step captures PolicyReason + DecisionTrace ─────────────

func TestSubmitPlan_CP1_DeniedStepCapturesTrace(t *testing.T) {
	taskStore := &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}
	pol := &fakePolicyEngine{
		planAllow: true,
		toolDec: &policy.Decision{
			Allow:        false,
			Deny:         true,
			Reason:       "explicit deny reason",
			Reasons:      []string{"rule-a triggered", "rule-b triggered"},
			PolicyRuleID: "opa:tool_invocation",
		},
	}
	svc := newSandboxSvcForPlan(taskStore, pol, nil)

	_, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan:   makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "doc.write", nil)}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}

	if len(taskStore.insertedRecords) != 1 {
		t.Fatalf("expected 1 inserted record, got %d", len(taskStore.insertedRecords))
	}
	rec := taskStore.insertedRecords[0]

	if rec.PolicyReason != "explicit deny reason" {
		t.Errorf("PolicyReason = %q, want %q", rec.PolicyReason, "explicit deny reason")
	}

	trace := rec.DecisionTrace
	if trace == nil {
		t.Fatal("DecisionTrace is nil")
	}

	reasons, ok := trace["reasons"]
	if !ok {
		t.Fatal("DecisionTrace missing 'reasons'")
	}
	rslice, ok := reasons.([]string)
	if !ok {
		t.Fatalf("DecisionTrace['reasons'] type %T, want []string", reasons)
	}
	if len(rslice) != 2 || rslice[0] != "rule-a triggered" || rslice[1] != "rule-b triggered" {
		t.Errorf("DecisionTrace['reasons'] = %v, want [rule-a triggered rule-b triggered]", rslice)
	}

	capScope, ok := trace["capability_scope"]
	if !ok {
		t.Fatal("DecisionTrace missing 'capability_scope'")
	}
	// doc.write maps to doc:write in the toolregistry.
	if capScope != "doc:write" {
		t.Errorf("DecisionTrace['capability_scope'] = %v, want doc:write", capScope)
	}

	// outcome must still be DENIED (record-only invariant)
	if rec.PolicyDecision != db.PolicyDeny {
		t.Errorf("PolicyDecision = %v, want DENY", rec.PolicyDecision)
	}
}

// ── CP1 test 2: web.fetch netacl deny captures network sub-key ───────────────

func TestSubmitPlan_CP1_NetaclDenyCaptures(t *testing.T) {
	taskStore := &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}
	pol := &fakePolicyEngine{
		planAllow: true,
		toolDec: &policy.Decision{
			Allow:        true, // OPA says allow; netacl will override to deny
			PolicyRuleID: "opa:tool_invocation",
		},
	}
	svc := newSandboxSvcForPlan(taskStore, pol, &staticACL{action: netacl.Deny})

	_, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan: makePlan(testTenantUUID, []*planv1.PlanStep{
			makeStep(1, "web.fetch", map[string]any{"url": "https://evil.example.com/path"}),
		}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}

	if len(taskStore.insertedRecords) != 1 {
		t.Fatalf("expected 1 inserted record, got %d", len(taskStore.insertedRecords))
	}
	rec := taskStore.insertedRecords[0]

	trace := rec.DecisionTrace
	if trace == nil {
		t.Fatal("DecisionTrace is nil after netacl deny")
	}

	netBlock, ok := trace["network"]
	if !ok {
		t.Fatal("DecisionTrace missing 'network' key after netacl deny")
	}
	netMap, ok := netBlock.(map[string]any)
	if !ok {
		t.Fatalf("DecisionTrace['network'] type %T, want map[string]any", netBlock)
	}
	if netMap["action"] != "DENY" {
		t.Errorf("network.action = %v, want DENY", netMap["action"])
	}
	host, _ := netMap["host"].(string)
	if host != "evil.example.com" {
		t.Errorf("network.host = %q, want evil.example.com", host)
	}

	// outcome must be DENIED (record-only)
	if rec.PolicyDecision != db.PolicyDeny {
		t.Errorf("PolicyDecision = %v, want DENY", rec.PolicyDecision)
	}
}

// ── CP1 test 3: regression — allowed step outcome/transition unchanged ────────

func TestSubmitPlan_CP1_AllowedOutcomeUnchanged(t *testing.T) {
	taskStore := &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}
	pol := &fakePolicyEngine{
		planAllow: true,
		toolDec: &policy.Decision{
			Allow:        true,
			Reason:       "",
			PolicyRuleID: "opa:tool_invocation",
		},
	}
	// No capability minter → mintStepTokens returns nil map silently.
	svc := newSandboxSvcForPlan(taskStore, pol, nil)

	result, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan:   makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "doc.read", nil)}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}

	if result.Outcome != planv1.ValidationOutcome_APPROVED {
		t.Errorf("outcome = %v, want APPROVED", result.Outcome)
	}

	// transitions: CREATED→PLANNING, PLANNING→VALIDATING, VALIDATING→APPROVED
	found := false
	for _, tr := range taskStore.transitions {
		if tr[0] == db.TaskStateValidating && tr[1] == db.TaskStateApproved {
			found = true
		}
	}
	if !found {
		t.Errorf("expected VALIDATING→APPROVED transition, got %v", taskStore.transitions)
	}

	if len(taskStore.insertedRecords) != 1 {
		t.Fatalf("expected 1 inserted record, got %d", len(taskStore.insertedRecords))
	}
	rec := taskStore.insertedRecords[0]
	if rec.PolicyDecision != db.PolicyAllow {
		t.Errorf("PolicyDecision = %v, want ALLOW", rec.PolicyDecision)
	}
	// PolicyReason empty for an allow with no reasons
	if rec.PolicyReason != "" {
		t.Errorf("PolicyReason = %q, want empty for allow", rec.PolicyReason)
	}
}

// ── F23 test: plan outcome metric emitted once per SubmitPlan call ───────────

// TestSubmitPlan_RecordsPlanOutcome verifies F23: SubmitPlan records exactly
// one plan-outcome metric per call, labeled with the aggregate outcome.
func TestSubmitPlan_RecordsPlanOutcome(t *testing.T) {
	taskStore := &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}
	pol := &fakePolicyEngine{
		planAllow: true,
		toolDec: &policy.Decision{
			Allow:        true,
			PolicyRuleID: "opa:tool_invocation",
		},
	}
	em, _ := audit.NewEmitter(context.Background(), audit.Config{})
	tm := &fakeTaskMetrics{}
	svc := &SandboxService{deps: Deps{
		Logger:      zap.NewNop(),
		Audit:       em,
		Tasks:       taskStore,
		TaskMetrics: tm,
	}, policy: pol}

	_, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan:   makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "doc.read", nil)}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}
	if len(tm.planOutcomes) != 1 || tm.planOutcomes[0] != "APPROVED" {
		t.Fatalf("expected one APPROVED outcome recorded, got %v", tm.planOutcomes)
	}
}

// TestSubmitPlan_NilTaskMetrics_NoPanic verifies F23's nil-safety contract:
// SubmitPlan must not panic when Deps.TaskMetrics is unset.
func TestSubmitPlan_NilTaskMetrics_NoPanic(t *testing.T) {
	taskStore := &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}
	pol := &fakePolicyEngine{
		planAllow: true,
		toolDec: &policy.Decision{
			Allow:        true,
			PolicyRuleID: "opa:tool_invocation",
		},
	}
	svc := newSandboxSvcForPlan(taskStore, pol, nil)

	if _, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan:   makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "doc.read", nil)}),
	}); err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}
}

// ── CP2 consumer test: approval_expiry_hours from Config ─────────────────────
// Verifies that SubmitPlan reads approval_expiry_hours from the injected Config
// when creating an approval request, rather than hard-coding 24h.

func TestSubmitPlan_CP2_ApprovalExpiryFromConfig(t *testing.T) {
	taskStore := &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}
	pol := &fakePolicyEngine{
		planAllow: true,
		// NEEDS_HUMAN: OPA returns approval required
		toolDec: &policy.Decision{
			Allow:         false,
			NeedsApproval: true,
			PolicyRuleID:  "opa:tool_invocation",
		},
	}
	// fake Config returning approval_expiry_hours = 1
	cfg := newFakeConfigStore()
	cfg.values[testTenantUUID+":approval_expiry_hours"] = "1"

	svc := newSandboxSvcForPlanWithConfig(taskStore, pol, nil, cfg)

	before := time.Now()
	_, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan:   makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "doc.write", nil)}),
	})
	after := time.Now()
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}

	req := taskStore.lastApproval
	if req == nil {
		t.Fatal("no approval request created")
	}

	// ExpiresAt should be approximately now + 1h, not 24h.
	// Accept a window of [now+50min, now+70min] to be robust to slow CI.
	lo := before.Add(50 * time.Minute)
	hi := after.Add(70 * time.Minute)
	if req.ExpiresAt.Before(lo) || req.ExpiresAt.After(hi) {
		t.Errorf("ExpiresAt = %v, want in [%v, %v] (config=1h)", req.ExpiresAt, lo, hi)
	}
}
