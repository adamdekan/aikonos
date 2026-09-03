package broker

// CP2 tests: disabled_tools enforcement at InvokeTool (unbypassable) +
// plan-time denial in SubmitPlan.

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/capability"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/notify"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// ── Test 1: disabled tool → PermissionDenied + tool_disabled audit event ─────

// TestInvokeTool_DisabledTool_EmitsDenyAndReturnsPermissionDenied verifies that
// a tool listed in disabled_tools is denied before the capability gate with a
// tool_disabled audit event.
func TestInvokeTool_DisabledTool_EmitsDenyAndReturnsPermissionDenied(t *testing.T) {
	em, sub := buildAuditBus(t, testTenantUUID)

	cfg := newFakeConfigStore()
	cfg.values[testTenantUUID+":disabled_tools"] = "web.fetch"

	svc := NewSandboxService(Deps{
		Logger:   zap.NewNop(),
		Audit:    em,
		TenantID: "aikonos-dev",
		Config:   cfg,
	})

	_, err := svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId:   "task-disabled-1",
		ToolId:   "web.fetch",
		TenantId: testTenantUUID,
		UserId:   "alice@example.com",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Errorf("error message %q should mention disabled", err.Error())
	}

	ev := receiveAuditEvent(t, sub, "aikonos.broker.tool.denied", 2*time.Second)
	if ev.Decision != auditv1.PolicyDecision_DENY {
		t.Errorf("Decision = %v, want DENY", ev.Decision)
	}
	if ev.ResourceRef != "aikonos:tool:web.fetch" {
		t.Errorf("ResourceRef = %q, want aikonos:tool:web.fetch", ev.ResourceRef)
	}
	if ev.Context == nil {
		t.Fatal("Context must not be nil")
	}
	if reason := ev.Context.Fields["reason"].GetStringValue(); reason != "tool_disabled" {
		t.Errorf("context.reason = %q, want tool_disabled", reason)
	}
}

// ── Test 2: empty disabled_tools → check denies nothing (deny-only invariant) ─

// TestInvokeTool_EmptyDisabledTools_DoesNotDeny verifies that with an empty
// disabled_tools list the disable check does not fire. The call reaches the
// capability gate (no token → "capability token required"), proving it passed
// the disable check. The error must NOT mention "disabled".
func TestInvokeTool_EmptyDisabledTools_DoesNotDeny(t *testing.T) {
	cfg := newFakeConfigStore()
	// disabled_tools deliberately absent — nothing disabled

	m, err := capability.GenerateMinter()
	if err != nil {
		t.Fatalf("GenerateMinter: %v", err)
	}

	svc := NewSandboxService(Deps{
		Logger:     zap.NewNop(),
		TenantID:   "aikonos-dev",
		Config:     cfg,
		Capability: m,
	})

	// No capability token → capability gate fires "capability token required",
	// not the disable check.
	_, callErr := svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId:   "task-notdisabled-1",
		ToolId:   "web.fetch",
		TenantId: testTenantUUID,
		UserId:   "alice@example.com",
	})
	if status.Code(callErr) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied (capability gate), got %v", callErr)
	}
	if strings.Contains(callErr.Error(), "disabled") {
		t.Errorf("error %q mentions disabled — disable check must not fire with empty list", callErr.Error())
	}
}

// ── Test 3: nil Config store → no panic, disable check skipped ───────────────

// TestInvokeTool_NilConfig_NoPanic ensures a nil Deps.Config does not panic.
// nopConfig returns "" → ParseList returns nil → nothing disabled.
func TestInvokeTool_NilConfig_NoPanic(t *testing.T) {
	svc := NewSandboxService(Deps{
		Logger:   zap.NewNop(),
		TenantID: "aikonos-dev",
		// Config intentionally nil — nopConfig fallback must activate
	})

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("InvokeTool panicked with nil Config: %v", r)
		}
	}()

	// Ignore error — just ensure no panic from the nil Config path.
	_, _ = svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId:   "task-nilcfg-1",
		ToolId:   "web.fetch",
		TenantId: testTenantUUID,
		UserId:   "alice@example.com",
	})
}

// ── Test 4: plan-time disabled step → DENIED outcome + tool_disabled audit ───

// TestSubmitPlan_DisabledTool_DeniesStep verifies that a plan step whose ToolId
// appears in disabled_tools produces a DENIED plan outcome and a tool_disabled
// audit event, mirroring the netacl deny branch.
func TestSubmitPlan_DisabledTool_DeniesStep(t *testing.T) {
	taskStore := &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}
	pol := &fakePolicyEngine{
		planAllow: true,
		toolDec: &policy.Decision{
			Allow:        true, // OPA allows; disabled_tools overrides to deny
			PolicyRuleID: "opa:tool_invocation",
		},
	}

	cfg := newFakeConfigStore()
	cfg.values[testTenantUUID+":disabled_tools"] = "doc.write"

	bus := notify.NewMemoryBus()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	em.AttachBus(bus)

	sub, subErr := bus.Subscribe(notify.AuditSubject(testTenantUUID))
	if subErr != nil {
		t.Fatalf("Subscribe: %v", subErr)
	}
	t.Cleanup(sub.Unsubscribe)

	svc := &SandboxService{deps: Deps{
		Logger: zap.NewNop(),
		Audit:  em,
		Tasks:  taskStore,
		Config: cfg,
	}, policy: pol}

	result, err := svc.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan:   makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "doc.write", nil)}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}

	if result.Outcome != planv1.ValidationOutcome_DENIED {
		t.Errorf("outcome = %v, want DENIED", result.Outcome)
	}

	if len(taskStore.insertedRecords) != 1 {
		t.Fatalf("expected 1 inserted record, got %d", len(taskStore.insertedRecords))
	}
	rec := taskStore.insertedRecords[0]
	if rec.PolicyDecision != db.PolicyDeny {
		t.Errorf("PolicyDecision = %v, want DENY", rec.PolicyDecision)
	}
	if !strings.Contains(rec.PolicyReason, "disabled") {
		t.Errorf("PolicyReason = %q, should mention disabled", rec.PolicyReason)
	}

	ev := receiveAuditEvent(t, sub, "aikonos.broker.tool.denied", 2*time.Second)
	if ev.Decision != auditv1.PolicyDecision_DENY {
		t.Errorf("audit Decision = %v, want DENY", ev.Decision)
	}
	if ev.Context == nil {
		t.Fatal("audit Context must not be nil")
	}
	if reason := ev.Context.Fields["reason"].GetStringValue(); reason != "tool_disabled" {
		t.Errorf("audit context.reason = %q, want tool_disabled", reason)
	}
}
