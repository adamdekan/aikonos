package broker

// simulate_test.go — unit tests for SimulatePolicy (CP2).
// Fakes: fakeSimPolicyEngine (DecideToolCall + CheckFGA), fakeACL (aclDecider).
// FGA admin gate reuses fakeFGA + testAdminDeps from roles_test.go.
//
// Eight cases:
//  1. Non-admin → PermissionDenied; evaluators NOT called.
//  2. Admin, policy fake allows, no host → outcome ALLOW; access_routing(ALLOW) + capability gate.
//  3. Admin, policy fake NeedsApproval → outcome NEEDS_HUMAN; access_routing carries rule_id+reason.
//  4. Admin, host + ACL fake Deny (policy ALLOW) → outcome DENY; network gate DENY.
//  5. Admin, fga_object + CheckFGA false → access gate DENY; DecideToolCall does NOT see FGADecision.
//  6. Side-effect: task store (noopTaskStore) + minter never called (no persistence, no mint).
//  7. DecideToolCall error → codes.Internal ("policy evaluation failed").
//  8. DecideToolCall returns (nil, nil) → codes.Internal (never default ALLOW).

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/netacl"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// ── fakes ─────────────────────────────────────────────────────────────────────

type fakeSimPolicyEngine struct {
	toolDec       *policy.Decision
	toolErr       error
	fgaAllowed    bool
	fgaErr        error
	toolCallCount int
	checkFGACount int
	lastQuery     policy.ToolQuery // capture for assertions
}

func (f *fakeSimPolicyEngine) DecideToolCall(_ context.Context, q policy.ToolQuery) (*policy.Decision, error) {
	f.toolCallCount++
	f.lastQuery = q
	return f.toolDec, f.toolErr
}

func (f *fakeSimPolicyEngine) CheckFGA(_ context.Context, _, _, _ string) (bool, error) {
	f.checkFGACount++
	return f.fgaAllowed, f.fgaErr
}

// fakeACLDecider satisfies aclDecider with a fixed action.
type fakeACLDecider struct {
	action  netacl.Action
	enabled bool
	called  bool
}

func (a *fakeACLDecider) Enabled() bool { return a.enabled }
func (a *fakeACLDecider) Decide(_ context.Context, _, _, _ string) netacl.Action {
	a.called = true
	return a.action
}

// noopTaskStore fails the test if any taskStore method is called. It fully
// satisfies the taskStore interface so it can be wired into Deps.Tasks; any
// call proves SimulatePolicy touched the store unexpectedly.
type noopTaskStore struct{ t *testing.T }

func (n *noopTaskStore) Get(_ context.Context, _, _ string) (*db.Task, error) {
	n.t.Fatalf("SimulatePolicy must not touch the task store: Get")
	return nil, nil
}
func (n *noopTaskStore) Transition(_ context.Context, _, _ string, _, _ db.TaskState) error {
	n.t.Fatalf("SimulatePolicy must not touch the task store: Transition")
	return nil
}
func (n *noopTaskStore) Create(_ context.Context, _ *db.Task) error {
	n.t.Fatalf("SimulatePolicy must not touch the task store: Create")
	return nil
}
func (n *noopTaskStore) IncrementCost(_ context.Context, _, _ string, _ int64) error {
	n.t.Fatalf("SimulatePolicy must not touch the task store: IncrementCost")
	return nil
}
func (n *noopTaskStore) InsertPlanSteps(_ context.Context, _ string, _ []db.PlanStepRecord) error {
	n.t.Fatalf("SimulatePolicy must not touch the task store: InsertPlanSteps")
	return nil
}
func (n *noopTaskStore) CreateApprovalRequest(_ context.Context, _ *db.ApprovalRequest) (uuid.UUID, error) {
	n.t.Fatalf("SimulatePolicy must not touch the task store: CreateApprovalRequest")
	return uuid.UUID{}, nil
}
func (n *noopTaskStore) ListMintableSteps(_ context.Context, _, _ string) ([]db.ExecutableStep, error) {
	n.t.Fatalf("SimulatePolicy must not touch the task store: ListMintableSteps")
	return nil, nil
}
func (n *noopTaskStore) GetPlanStepTraces(_ context.Context, _, _ string) ([]db.PlanStepTrace, error) {
	n.t.Fatalf("SimulatePolicy must not touch the task store: GetPlanStepTraces")
	return nil, nil
}
func (n *noopTaskStore) GetPendingApprovalByTask(_ context.Context, _, _ string) (*db.ApprovalRequest, error) {
	n.t.Fatalf("SimulatePolicy must not touch the task store: GetPendingApprovalByTask")
	return nil, nil
}
func (n *noopTaskStore) ResolveApproval(_ context.Context, _, _, _ string, _ bool, _ string) (db.ApprovalState, bool, error) {
	n.t.Fatalf("SimulatePolicy must not touch the task store: ResolveApproval")
	return "", false, nil
}
func (n *noopTaskStore) ListInbox(_ context.Context, _, _ string, _ bool) ([]*db.Envelope, error) {
	n.t.Fatalf("SimulatePolicy must not touch the task store: ListInbox")
	return nil, nil
}
func (n *noopTaskStore) GetEnvelope(_ context.Context, _, _ string) (*db.Envelope, error) {
	n.t.Fatalf("SimulatePolicy must not touch the task store: GetEnvelope")
	return nil, nil
}
func (n *noopTaskStore) RespondEnvelope(_ context.Context, _, _, _ string, _ bool) (db.EnvelopeState, error) {
	n.t.Fatalf("SimulatePolicy must not touch the task store: RespondEnvelope")
	return "", nil
}
func (n *noopTaskStore) CreateEnvelope(_ context.Context, _ *db.Envelope, _ db.EnvelopeState) (uuid.UUID, error) {
	n.t.Fatalf("SimulatePolicy must not touch the task store: CreateEnvelope")
	return uuid.UUID{}, nil
}
func (n *noopTaskStore) DismissEnvelope(_ context.Context, _, _, _ string) (db.EnvelopeState, error) {
	n.t.Fatalf("SimulatePolicy must not touch the task store: DismissEnvelope")
	return "", nil
}
func (n *noopTaskStore) CountEnvelopesByPayloadRef(_ context.Context, _, _ string) (int, error) {
	n.t.Fatalf("SimulatePolicy must not touch the task store: CountEnvelopesByPayloadRef")
	return 0, nil
}

// compile-time check that noopTaskStore satisfies taskStore
var _ taskStore = (*noopTaskStore)(nil)

// newSimSvc builds a BrokerService wired with the given sim engine + ACL.
// Uses fakeFGA to satisfy requireTenantAdmin (the admin check uses deps.Policy
// directly, not the simPolicy override — so we still need a real policy.Engine
// pointing at a fakeFGA server).
func newSimSvc(t *testing.T, simEng simPolicyEngine, acl aclDecider) *BrokerService {
	t.Helper()
	fg := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := fg.server(t)
	t.Cleanup(srv.Close)

	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	deps := testAdminDeps(t, srv.URL)
	deps.Audit = em
	deps.ACL = acl
	svc := NewBrokerService(deps)
	svc.simPolicy = simEng
	return svc
}

// ── Test 1: non-admin → PermissionDenied; evaluators not called ──────────────

func TestSimulatePolicy_NonAdmin_Denied(t *testing.T) {
	eng := &fakeSimPolicyEngine{toolDec: &policy.Decision{Allow: true}}
	svc := newSimSvc(t, eng, nil)

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.SimulatePolicy(ctx, &brokerv1.SimulatePolicyRequest{
		SubjectUserId: "bob@example.com",
		ToolId:        "web.fetch",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin: want PermissionDenied, got %v", err)
	}
	if eng.toolCallCount != 0 {
		t.Errorf("DecideToolCall must not be called when admin check fails, got %d calls", eng.toolCallCount)
	}
	if eng.checkFGACount != 0 {
		t.Errorf("CheckFGA must not be called when admin check fails, got %d calls", eng.checkFGACount)
	}
}

// ── Test 2: admin, policy allows, no host → ALLOW + capability gate ───────────

func TestSimulatePolicy_Admin_Allow_NoHost(t *testing.T) {
	eng := &fakeSimPolicyEngine{
		toolDec: &policy.Decision{Allow: true, PolicyRuleID: "opa:tool_invocation"},
	}
	svc := newSimSvc(t, eng, nil)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.SimulatePolicy(ctx, &brokerv1.SimulatePolicyRequest{
		SubjectUserId: "bob@example.com",
		ToolId:        "web.fetch",
		EffectClass:   planv1.EffectClass_READ_ONLY,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Outcome != "ALLOW" {
		t.Errorf("outcome = %q, want ALLOW", resp.Outcome)
	}

	var arGate, capGate *brokerv1.GateDecision
	for _, g := range resp.Gates {
		switch g.Gate {
		case "access_routing":
			arGate = g
		case "capability":
			capGate = g
		}
	}
	if arGate == nil {
		t.Fatal("missing access_routing gate")
	}
	if arGate.Outcome != "ALLOW" {
		t.Errorf("access_routing.Outcome = %q, want ALLOW", arGate.Outcome)
	}
	if capGate == nil {
		t.Fatal("missing capability gate")
	}
	// web.fetch → web:read scope
	if capGate.Detail != "web:read" {
		t.Errorf("capability.Detail = %q, want web:read", capGate.Detail)
	}
}

// ── Test 3: admin, policy NeedsApproval → NEEDS_HUMAN; rule_id + reason ──────

func TestSimulatePolicy_Admin_NeedsApproval(t *testing.T) {
	eng := &fakeSimPolicyEngine{
		toolDec: &policy.Decision{
			NeedsApproval: true,
			Reason:        "effect class requires human approval",
			PolicyRuleID:  "opa:tool_invocation",
		},
	}
	svc := newSimSvc(t, eng, nil)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.SimulatePolicy(ctx, &brokerv1.SimulatePolicyRequest{
		SubjectUserId: "bob@example.com",
		ToolId:        "doc.write",
		EffectClass:   planv1.EffectClass_WRITE_LOCAL,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Outcome != "NEEDS_HUMAN" {
		t.Errorf("outcome = %q, want NEEDS_HUMAN", resp.Outcome)
	}

	var arGate *brokerv1.GateDecision
	for _, g := range resp.Gates {
		if g.Gate == "access_routing" {
			arGate = g
		}
	}
	if arGate == nil {
		t.Fatal("missing access_routing gate")
	}
	if arGate.RuleId != "opa:tool_invocation" {
		t.Errorf("access_routing.RuleId = %q, want opa:tool_invocation", arGate.RuleId)
	}
	if arGate.Reason != "effect class requires human approval" {
		t.Errorf("access_routing.Reason = %q", arGate.Reason)
	}
}

// ── Test 4: admin, host + ACL Deny (policy ALLOW) → outcome DENY + network gate

func TestSimulatePolicy_Admin_NetACLDeny(t *testing.T) {
	eng := &fakeSimPolicyEngine{
		toolDec: &policy.Decision{Allow: true},
	}
	acl := &fakeACLDecider{action: netacl.Deny, enabled: true}
	svc := newSimSvc(t, eng, acl)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.SimulatePolicy(ctx, &brokerv1.SimulatePolicyRequest{
		SubjectUserId: "bob@example.com",
		ToolId:        "web.fetch",
		Host:          "evil.example.com",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Outcome != "DENY" {
		t.Errorf("outcome = %q, want DENY", resp.Outcome)
	}

	var netGate *brokerv1.GateDecision
	for _, g := range resp.Gates {
		if g.Gate == "network" {
			netGate = g
		}
	}
	if netGate == nil {
		t.Fatal("missing network gate")
	}
	if netGate.Outcome != "DENY" {
		t.Errorf("network.Outcome = %q, want DENY", netGate.Outcome)
	}
	if netGate.Detail != "evil.example.com" {
		t.Errorf("network.Detail = %q, want evil.example.com", netGate.Detail)
	}
	if !acl.called {
		t.Error("ACL.Decide should have been called")
	}
}

// ── Test 5: admin, fga_object + CheckFGA false → access gate DENY; DecideToolCall does NOT see FGADecision
//
// Per CP2 #3: the simulator builds the ToolQuery to match SubmitPlan's plan-time call, which does
// not set FGADecision. The FGA can_access check is informational only (models the InvokeTool-time
// check); it must NOT feed DecideToolCall.

func TestSimulatePolicy_Admin_FGADeny(t *testing.T) {
	eng := &fakeSimPolicyEngine{
		fgaAllowed: false,
		toolDec:    &policy.Decision{Allow: true},
	}
	svc := newSimSvc(t, eng, nil)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.SimulatePolicy(ctx, &brokerv1.SimulatePolicyRequest{
		SubjectUserId: "bob@example.com",
		ToolId:        "web.fetch",
		FgaObject:     "document:secret-doc",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// access gate must show DENY (informational InvokeTool-time check)
	var accessGate *brokerv1.GateDecision
	for _, g := range resp.Gates {
		if g.Gate == "access" {
			accessGate = g
		}
	}
	if accessGate == nil {
		t.Fatal("missing access gate")
	}
	if accessGate.Outcome != "DENY" {
		t.Errorf("access.Outcome = %q, want DENY", accessGate.Outcome)
	}
	if accessGate.Detail != "document:secret-doc" {
		t.Errorf("access.Detail = %q, want document:secret-doc", accessGate.Detail)
	}

	// DecideToolCall must NOT have seen FGADecision (parity with SubmitPlan plan-time call)
	if eng.lastQuery.FGADecision != "" {
		t.Errorf("DecideToolCall.FGADecision = %q, want empty (not fed at plan-time)", eng.lastQuery.FGADecision)
	}
	if eng.checkFGACount != 1 {
		t.Errorf("CheckFGA call count = %d, want 1", eng.checkFGACount)
	}
}

// ── Test 8: DecideToolCall returns (nil, nil) → codes.Internal (never default ALLOW) ──────────

func TestSimulatePolicy_NilDecision_Internal(t *testing.T) {
	eng := &fakeSimPolicyEngine{
		toolDec: nil, // (nil, nil) — no error, no decision
		toolErr: nil,
	}
	svc := newSimSvc(t, eng, nil)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.SimulatePolicy(ctx, &brokerv1.SimulatePolicyRequest{
		SubjectUserId: "bob@example.com",
		ToolId:        "web.fetch",
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("nil decision: want codes.Internal, got %v", err)
	}
}

// ── Test 6: side-effect assertion — no DB / minter calls ─────────────────────

func TestSimulatePolicy_NoSideEffects(t *testing.T) {
	eng := &fakeSimPolicyEngine{
		toolDec: &policy.Decision{Allow: true},
	}
	// Wire a noopTaskStore into Deps.Tasks: any taskStore call fails the test
	// structurally (t.Fatalf), proving SimulatePolicy never touches the store.
	// Capability = nil ensures no token minting is attempted.
	fg := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := fg.server(t)
	t.Cleanup(srv.Close)

	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	deps := testAdminDeps(t, srv.URL)
	deps.Audit = em
	deps.Capability = nil             // no minter
	deps.Tasks = &noopTaskStore{t: t} // structural guard: any call → t.Fatalf
	svc := NewBrokerService(deps)
	svc.simPolicy = eng

	_ = zap.NewNop() // keep import used

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.SimulatePolicy(ctx, &brokerv1.SimulatePolicyRequest{
		SubjectUserId: "bob@example.com",
		ToolId:        "web.fetch",
	})
	if err != nil {
		t.Fatalf("unexpected error (should be side-effect-free): %v", err)
	}
	if resp.Outcome != "ALLOW" {
		t.Errorf("outcome = %q, want ALLOW", resp.Outcome)
	}
}

// ── Test 7: DecideToolCall error → codes.Internal ─────────────────────────────

func TestSimulatePolicy_DecideToolCallError_Internal(t *testing.T) {
	eng := &fakeSimPolicyEngine{
		toolErr: errors.New("opa unreachable"),
	}
	svc := newSimSvc(t, eng, nil)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.SimulatePolicy(ctx, &brokerv1.SimulatePolicyRequest{
		SubjectUserId: "bob@example.com",
		ToolId:        "web.fetch",
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("DecideToolCall error: want codes.Internal, got %v", err)
	}
}
