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
	"github.com/adamdekan/aikonos/broker/internal/auth"
	"github.com/adamdekan/aikonos/broker/internal/capability"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/netacl"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	"github.com/adamdekan/aikonos/broker/internal/toolproxy"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
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
	return &SandboxService{deps: Deps{GatewaySpiffeID: testGWSpiffeID,
		DevMode: true,
		Logger:  zap.NewNop(),
		Audit:   em,
		Tasks:   tasks,
		ACL:     acl,
	}, policy: pol}
}

func newSandboxSvcForPlanWithConfig(tasks taskStore, pol planPolicyEngine, acl aclDecider, cfg Config) *SandboxService {
	em, _ := audit.NewEmitter(context.Background(), audit.Config{})
	return &SandboxService{deps: Deps{GatewaySpiffeID: testGWSpiffeID,
		DevMode: true,
		Logger:  zap.NewNop(),
		Audit:   em,
		Tasks:   tasks,
		ACL:     acl,
		Config:  cfg,
	}, policy: pol}
}

// ── F15 test: NotFound sentinel propagates as codes.NotFound ──────────────────

// TestSubmitPlan_TaskNotFound verifies that a missing task (repo Get returning
// an error matching db.ErrNotFound) surfaces as codes.NotFound rather than the
// codes.Internal a dead pgx.ErrNoRows comparison used to produce.
func TestSubmitPlan_TaskNotFound(t *testing.T) {
	taskStore := &fakeTaskStore{getErr: db.ErrNotFound}
	svc := newSandboxSvcForPlan(taskStore, &fakePolicyEngine{}, nil)

	_, err := svc.SubmitPlan(asGateway(context.Background()), &brokerv1.SubmitPlanRequest{
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

	_, err := svc.SubmitPlan(asGateway(context.Background()), &brokerv1.SubmitPlanRequest{
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
	if taskStore.task.State != db.TaskStateDenied {
		t.Errorf("task state = %s, want DENIED", taskStore.task.State)
	}
}

// A structurally refused plan ends the task instead of leaving it VALIDATING.
func TestSubmitPlan_PlanValidationDenyEndsTask(t *testing.T) {
	taskStore := &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}
	svc := newSandboxSvcForPlan(taskStore, &fakePolicyEngine{planAllow: false}, nil)

	res, err := svc.SubmitPlan(asGateway(context.Background()), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan:   makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "doc.write", nil)}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}
	if res.Outcome != planv1.ValidationOutcome_DENIED || taskStore.task.State != db.TaskStateDenied {
		t.Fatalf("outcome %s, task state %s; want DENIED/DENIED", res.Outcome, taskStore.task.State)
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

	_, err := svc.SubmitPlan(asGateway(context.Background()), &brokerv1.SubmitPlanRequest{
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

	result, err := svc.SubmitPlan(asGateway(context.Background()), &brokerv1.SubmitPlanRequest{
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
	svc := &SandboxService{deps: Deps{GatewaySpiffeID: testGWSpiffeID,
		DevMode:     true,
		Logger:      zap.NewNop(),
		Audit:       em,
		Tasks:       taskStore,
		TaskMetrics: tm,
	}, policy: pol}

	_, err := svc.SubmitPlan(asGateway(context.Background()), &brokerv1.SubmitPlanRequest{
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

	if _, err := svc.SubmitPlan(asGateway(context.Background()), &brokerv1.SubmitPlanRequest{
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
	_, err := svc.SubmitPlan(asGateway(context.Background()), &brokerv1.SubmitPlanRequest{
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

// ── H-04: SubmitPlan binds to the verified peer and the task row ─────────────

// actorCapturePolicy records the OPA actor SubmitPlan evaluates the plan as.
type actorCapturePolicy struct {
	fakePolicyEngine
	actor string
}

func (a *actorCapturePolicy) DecidePlan(ctx context.Context, p *planv1.Plan, pctx policy.PlanContext) (*policy.PlanDecision, error) {
	a.actor = pctx.ActorSpiffeID
	return a.fakePolicyEngine.DecidePlan(ctx, p, pctx)
}

func peerCtx(spiffeID string) context.Context {
	return auth.WithIdentity(context.Background(), &auth.Identity{SpiffeID: spiffeID})
}

func TestSubmitPlan_SpiffeIDDiffersFromVerifiedPeerDenied(t *testing.T) {
	taskStore := &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}
	svc := newSandboxSvcForPlan(taskStore, &fakePolicyEngine{planAllow: true}, nil)

	_, err := svc.SubmitPlan(asGateway(context.Background()), &brokerv1.SubmitPlanRequest{
		TaskId:          validTaskUUID(),
		SandboxSpiffeId: "spiffe://aikonos.com/control-plane",
		Plan:            makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "doc.read", nil)}),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
	if len(taskStore.transitions) != 0 {
		t.Fatalf("a spoofed peer must not move the task, got %+v", taskStore.transitions)
	}
}

func TestSubmitPlan_VerifiedPeerIsTheActor(t *testing.T) {
	const peer = "spiffe://aikonos.com/agent-gateway"
	taskStore := &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}
	pol := &actorCapturePolicy{fakePolicyEngine: fakePolicyEngine{planAllow: true, toolDec: &policy.Decision{Allow: true}}}
	svc := newSandboxSvcForPlan(taskStore, pol, nil)
	svc.deps.DevMode = false

	if _, err := svc.SubmitPlan(peerCtx(peer), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan:   makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "doc.read", nil)}),
	}); err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}
	if pol.actor != peer {
		t.Fatalf("OPA actor = %q, want the verified peer %q", pol.actor, peer)
	}
}

// TestSouth_NonGatewayPeerDenied (H-04): SubmitPlan, InvokeTool and
// EmitStatus answer only the agent-gateway SVID. Another trust-domain peer, or
// none (dev mode too), is refused before any task is read or moved.
func TestSouth_NonGatewayPeerDenied(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"sandbox peer": peerCtx("spiffe://aikonos.com/sandbox/a"),
		"no peer":      context.Background(),
	} {
		t.Run(name, func(t *testing.T) {
			taskStore := &fakeTaskStore{task: newFakeTask(db.TaskStateApproved)}
			svc := newSandboxSvcForPlan(taskStore, &fakePolicyEngine{planAllow: true}, nil)
			_, perr := svc.SubmitPlan(ctx, &brokerv1.SubmitPlanRequest{
				TaskId: validTaskUUID(),
				Plan:   makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "doc.read", nil)}),
			})
			_, ierr := svc.InvokeTool(ctx, &brokerv1.InvokeToolRequest{TaskId: validTaskUUID(), StepId: "1", ToolId: "web.fetch"})
			_, serr := svc.EmitStatus(ctx, &brokerv1.EmitStatusRequest{TaskId: validTaskUUID(), Status: brokerv1.TaskStatus_COMPLETED})
			for rpc, err := range map[string]error{"SubmitPlan": perr, "InvokeTool": ierr, "EmitStatus": serr} {
				if status.Code(err) != codes.PermissionDenied {
					t.Errorf("%s: want PermissionDenied, got %v", rpc, err)
				}
			}
			if len(taskStore.transitions) != 0 {
				t.Fatalf("a non-gateway peer must not move the task, got %+v", taskStore.transitions)
			}
		})
	}
}

func TestSubmitPlan_PlanTenantDiffersFromTaskDenied(t *testing.T) {
	taskStore := &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}
	svc := newSandboxSvcForPlan(taskStore, &fakePolicyEngine{planAllow: true}, nil)

	_, err := svc.SubmitPlan(peerCtx(testGWSpiffeID), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan:   makePlan("bbbbbbbb-0000-0000-0000-000000000002", []*planv1.PlanStep{makeStep(1, "doc.read", nil)}),
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
	if len(taskStore.transitions) != 0 {
		t.Fatalf("a foreign tenant must not move the task, got %+v", taskStore.transitions)
	}
}

// TestInvokeTool_SubmitPlanTokenRoundTrip: the token SubmitPlan mints for a
// step is accepted by InvokeTool on the same task — mintStepTokens and
// InvokeTool's Authorize agree on the task id and on-behalf-of end to end.
func TestInvokeTool_SubmitPlanTokenRoundTrip(t *testing.T) {
	m, err := capability.GenerateMinter()
	if err != nil {
		t.Fatalf("GenerateMinter: %v", err)
	}
	var got toolproxy.Request
	proxy := toolproxy.New(toolproxy.Config{}, zap.NewNop())
	proxy.Register("web.fetch", func(_ context.Context, req toolproxy.Request) (map[string]any, int64, error) {
		got = req
		return map[string]any{"content": "ok"}, 1, nil
	})
	task := newFakeTask(db.TaskStateCreated)
	task.CostBudget = 100
	taskStore := &fakeTaskStore{task: task}
	pol := &fakePolicyEngine{planAllow: true, toolDec: &policy.Decision{Allow: true}}
	svc := newSandboxSvcForPlan(taskStore, pol, nil)
	svc.deps.Capability, svc.deps.ToolProxy = m, proxy
	// AG-17: under identity.obo=enforce the step token's subject/actor facts
	// must match the task row, or the round-trip below is denied.
	cfg := newFakeConfigStore()
	cfg.values[testTenantUUID+":identity.obo"] = "enforce"
	svc.deps.Config = cfg

	res, err := svc.SubmitPlan(asGateway(context.Background()), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan: makePlan(testTenantUUID, []*planv1.PlanStep{
			makeStep(1, "web.fetch", map[string]any{"url": "https://example.com"}),
		}),
	})
	if err != nil || res.Outcome != planv1.ValidationOutcome_APPROVED {
		t.Fatalf("SubmitPlan: res=%v err=%v", res, err)
	}
	tok := res.CapabilityTokenIds[1]
	if tok == "" {
		t.Fatalf("no capability token for step 1: %v", res.CapabilityTokenIds)
	}
	if task.State != db.TaskStateApproved {
		t.Fatalf("task state after SubmitPlan = %s, want APPROVED", task.State)
	}

	args, _ := structpb.NewStruct(map[string]any{"url": "https://example.com"})
	resp, err := svc.InvokeTool(asGateway(context.Background()), &brokerv1.InvokeToolRequest{
		StepId: "1",
		TaskId: validTaskUUID(), ToolId: "web.fetch", CapabilityToken: tok, Args: args,
		UserId: task.OwnerUserID,
	})
	if err != nil || !resp.Success {
		t.Fatalf("InvokeTool with the SubmitPlan token: resp=%v err=%v", resp, err)
	}
	if got.ToolID != "web.fetch" || got.TaskID != validTaskUUID() || got.UserID != task.OwnerUserID {
		t.Fatalf("handler did not run as the task: %+v", got)
	}
}

// TestSubmitPlan_StepCap (F124): a plan over maxPlanSteps is refused before
// OPA sees it and before the task moves.
func TestSubmitPlan_StepCap(t *testing.T) {
	taskStore := &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}
	pol := &actorCapturePolicy{fakePolicyEngine: fakePolicyEngine{planAllow: true}}
	svc := newSandboxSvcForPlan(taskStore, pol, nil)
	steps := make([]*planv1.PlanStep, maxPlanSteps+1)
	for i := range steps {
		steps[i] = makeStep(int32(i+1), "doc.read", nil)
	}
	_, err := svc.SubmitPlan(peerCtx(testGWSpiffeID), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(), Plan: makePlan(testTenantUUID, steps),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
	if pol.actor != "" || len(taskStore.transitions) != 0 {
		t.Fatalf("an oversized plan must not reach OPA or move the task (opa actor %q, transitions %+v)", pol.actor, taskStore.transitions)
	}
}

// TestSubmitPlan_NegativeEstimatedCost (F123): a negative step estimate would
// offset the others in the plan budget sum, so the broker refuses it before
// OPA (independent of the bundle) and before the task moves, and audits it.
func TestSubmitPlan_NegativeEstimatedCost(t *testing.T) {
	taskStore := &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}
	pol := &actorCapturePolicy{fakePolicyEngine: fakePolicyEngine{planAllow: true}}
	svc := newSandboxSvcForPlan(taskStore, pol, nil)
	sink := &capturingAuditSink{}
	svc.deps.Audit = sink
	cheap, refund := makeStep(1, "doc.read", nil), makeStep(2, "doc.read", map[string]any{"path": "b"})
	cheap.EstimatedCost, refund.EstimatedCost = 1000100, -1000000
	_, err := svc.SubmitPlan(peerCtx(testGWSpiffeID), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(), Plan: makePlan(testTenantUUID, []*planv1.PlanStep{cheap, refund}),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
	if pol.actor != "" || len(taskStore.transitions) != 0 {
		t.Fatalf("a negative-cost plan must not reach OPA or move the task (opa actor %q, transitions %+v)", pol.actor, taskStore.transitions)
	}
	ev := sink.last()
	if ev == nil || ev.Decision != auditv1.PolicyDecision_DENY || ev.Context.Fields["reason"].GetStringValue() != "negative_estimated_cost" {
		t.Fatalf("want a DENY audit with reason negative_estimated_cost, got %v", ev)
	}
}
