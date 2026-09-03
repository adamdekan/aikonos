package broker

// CP2 tests: GetDecisionTrace handler — non-admin denial, trace assembly, empty.
// Uses fakeFGA (from roles_test.go) and the taskStore fake extended with
// GetPlanStepTraces (see submit_ifaces.go update).

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func newDecisionSvc(t *testing.T, fgaURL string, traces []db.PlanStepTrace) *BrokerService {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	deps := testAdminDeps(t, fgaURL)
	deps.Audit = em
	deps.Tasks = &fakeTaskStoreWithTraces{traces: traces}
	svc := NewBrokerService(deps)
	return svc
}

// fakeTaskStoreWithTraces extends fakeTaskStore with GetPlanStepTraces.
type fakeTaskStoreWithTraces struct {
	fakeTaskStore
	traces    []db.PlanStepTrace
	traceErr  error
	callCount int
}

func (f *fakeTaskStoreWithTraces) GetPlanStepTraces(_ context.Context, _, _ string) ([]db.PlanStepTrace, error) {
	f.callCount++
	return f.traces, f.traceErr
}

// ── Test 1: non-admin → PermissionDenied; repo NOT called ────────────────────

func TestGetDecisionTrace_NonAdmin_Denied(t *testing.T) {
	fg := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := fg.server(t)
	defer srv.Close()

	store := &fakeTaskStoreWithTraces{}
	em, _ := audit.NewEmitter(context.Background(), audit.Config{})
	deps := testAdminDeps(t, srv.URL)
	deps.Audit = em
	deps.Tasks = store
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.GetDecisionTrace(ctx, &brokerv1.GetDecisionTraceRequest{TaskId: "some-task-id"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin should get PermissionDenied, got %v", err)
	}
	if store.callCount != 0 {
		t.Errorf("GetPlanStepTraces must not be called when admin check fails, got %d calls", store.callCount)
	}
}

// ── Test 2: admin, 2 traces → correct StepTraces assembled ───────────────────

func TestGetDecisionTrace_Admin_AssemblesGates(t *testing.T) {
	fg := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := fg.server(t)
	defer srv.Close()

	traces := []db.PlanStepTrace{
		{
			Seq:            1,
			ToolID:         "doc.write",
			EffectClass:    "WRITE_LOCAL",
			PolicyDecision: "DENY",
			PolicyRuleID:   "opa:tool_invocation",
			PolicyReason:   "write tools require approval",
			Justification:  "write a doc",
			DecisionTrace: map[string]any{
				"reasons":          []any{"rule-x triggered", "rule-y triggered"},
				"capability_scope": "doc:write",
			},
		},
		{
			Seq:            2,
			ToolID:         "web.fetch",
			EffectClass:    "READ_ONLY",
			PolicyDecision: "ALLOW",
			PolicyRuleID:   "opa:tool_invocation",
			PolicyReason:   "",
			Justification:  "fetch a url",
			DecisionTrace: map[string]any{
				"network": map[string]any{
					"host":   "example.com",
					"action": "ALLOW",
					"reason": "allow-list match",
				},
				"capability_scope": "web:read",
			},
		},
	}

	svc := newDecisionSvc(t, srv.URL, traces)
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")

	resp, err := svc.GetDecisionTrace(ctx, &brokerv1.GetDecisionTraceRequest{TaskId: "task-abc"})
	if err != nil {
		t.Fatalf("GetDecisionTrace: %v", err)
	}
	if resp.TaskId != "task-abc" {
		t.Errorf("TaskId = %q, want task-abc", resp.TaskId)
	}
	if len(resp.Steps) != 2 {
		t.Fatalf("want 2 steps, got %d", len(resp.Steps))
	}

	// ── step 1 (DENY) ──
	s1 := resp.Steps[0]
	if s1.Seq != 1 {
		t.Errorf("step[0].Seq = %d, want 1", s1.Seq)
	}
	if s1.ToolId != "doc.write" {
		t.Errorf("step[0].ToolId = %q, want doc.write", s1.ToolId)
	}
	if s1.Decision != "DENY" {
		t.Errorf("step[0].Decision = %q, want DENY", s1.Decision)
	}

	var arGate *brokerv1.GateDecision
	for _, g := range s1.Gates {
		if g.Gate == "access_routing" {
			arGate = g
		}
	}
	if arGate == nil {
		t.Fatal("step[0] missing access_routing gate")
	}
	if arGate.RuleId != "opa:tool_invocation" {
		t.Errorf("access_routing.RuleId = %q, want opa:tool_invocation", arGate.RuleId)
	}
	if arGate.Reason != "write tools require approval" {
		t.Errorf("access_routing.Reason = %q, want 'write tools require approval'", arGate.Reason)
	}

	// ── step 2 (ALLOW with network + capability) ──
	s2 := resp.Steps[1]
	if s2.Seq != 2 {
		t.Errorf("step[1].Seq = %d, want 2", s2.Seq)
	}

	var netGate, capGate *brokerv1.GateDecision
	for _, g := range s2.Gates {
		switch g.Gate {
		case "network":
			netGate = g
		case "capability":
			capGate = g
		}
	}
	if netGate == nil {
		t.Fatal("step[1] missing network gate")
	}
	if netGate.Detail != "example.com" {
		t.Errorf("network.Detail = %q, want example.com", netGate.Detail)
	}
	if capGate == nil {
		t.Fatal("step[1] missing capability gate")
	}
	if capGate.Detail != "web:read" {
		t.Errorf("capability.Detail = %q, want web:read", capGate.Detail)
	}
}

// ── Test 3: admin, no traces → empty steps, no error ─────────────────────────

func TestGetDecisionTrace_Admin_NoSteps_ReturnsEmpty(t *testing.T) {
	fg := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := fg.server(t)
	defer srv.Close()

	svc := newDecisionSvc(t, srv.URL, nil)
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")

	resp, err := svc.GetDecisionTrace(ctx, &brokerv1.GetDecisionTraceRequest{TaskId: "task-xyz"})
	if err != nil {
		t.Fatalf("unexpected error for empty trace: %v", err)
	}
	if len(resp.Steps) != 0 {
		t.Errorf("want 0 steps, got %d", len(resp.Steps))
	}
}

// ── Test 4: admin, store error → codes.Internal ───────────────────────────────

func TestGetDecisionTrace_StoreError_ReturnsInternal(t *testing.T) {
	fg := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := fg.server(t)
	defer srv.Close()

	em, _ := audit.NewEmitter(context.Background(), audit.Config{})
	deps := testAdminDeps(t, srv.URL)
	deps.Audit = em
	deps.Tasks = &fakeTaskStoreWithTraces{traceErr: errors.New("db connection lost")}
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.GetDecisionTrace(ctx, &brokerv1.GetDecisionTraceRequest{TaskId: "task-err"})
	if status.Code(err) != codes.Internal {
		t.Fatalf("store error should yield codes.Internal, got %v", err)
	}
}

// Ensure BrokerService embeds a Deps.tasks field usable from tests.
// (compile-time check that fakeTaskStoreWithTraces satisfies taskStore)
var _ taskStore = (*fakeTaskStoreWithTraces)(nil)
var _ = zap.NewNop // keep import used
