package broker

// CP3 tests: validateSkillOverlay, lazy loader, InvokeTool overlay deny,
// SubmitPlan overlay deny. All DB-free via fake skillOverlayStore.

import (
	"context"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/capability"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	"github.com/adamdekan/aikonos/broker/internal/toolproxy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// ── fake skillOverlayStore ────────────────────────────────────────────────────

type fakeOverlayStore struct {
	rows  []db.SkillOverlay
	calls atomic.Int64 // counts ListByTenant calls
}

func (f *fakeOverlayStore) ListByTenant(_ context.Context, _ string) ([]db.SkillOverlay, error) {
	f.calls.Add(1)
	return f.rows, nil
}

// ── fake mcpConnectionRepo for connector-existence checks in validateSkillOverlay ─

type fakeMcpRepoSimple struct {
	ids []string // connector IDs that "exist" for the tenant
}

func (r *fakeMcpRepoSimple) List(_ context.Context, _ string) ([]*db.McpConnection, error) {
	return nil, nil
}
func (r *fakeMcpRepoSimple) Get(_ context.Context, _, id string) (*db.McpConnection, error) {
	for _, v := range r.ids {
		if v == id {
			return &db.McpConnection{}, nil
		}
	}
	return nil, status.Errorf(codes.NotFound, "not found")
}
func (r *fakeMcpRepoSimple) Add(_ context.Context, _ *db.McpConnection) error { return nil }
func (r *fakeMcpRepoSimple) Update(_ context.Context, _ string, _ *db.McpConnection) error {
	return nil
}
func (r *fakeMcpRepoSimple) Delete(_ context.Context, _, _ string) error { return nil }

// ── helpers ───────────────────────────────────────────────────────────────────

func brokerSvcWithOverlay(t *testing.T, store skillOverlayStore, connIDs []string) *BrokerService {
	t.Helper()
	reg := newTestToolRegistry()
	repo := &fakeMcpRepoSimple{ids: connIDs}
	return NewBrokerService(Deps{
		Logger:       zap.NewNop(),
		ToolRegistry: reg,
		SkillOverlay: store,
		Mcp:          McpDeps{Connections: repo},
		TenantID:     testTenantUUID,
	})
}

func nopAuditEmitter(t *testing.T) *audit.Emitter {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	return em
}

func cp3SubmitPlanReq(tenantID, toolID string) *brokerv1.SubmitPlanRequest {
	return &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan: &planv1.Plan{
			PlanId:   "plan-cp3",
			TenantId: tenantID,
			Steps: []*planv1.PlanStep{
				{Seq: 1, ToolId: toolID, EffectClass: planv1.EffectClass_WRITE_LOCAL},
			},
		},
	}
}

// ── (a) validateSkillOverlay ─────────────────────────────────────────────────

// Net-new id (not in baseline, not mcp) → InvalidArgument.
func TestValidateSkillOverlay_NetNewIdRejected(t *testing.T) {
	svc := brokerSvcWithOverlay(t, &fakeOverlayStore{}, nil)
	_, _, _, err := svc.validateSkillOverlay(context.Background(), testTenantUUID, "net.new.tool", "", "read_only")
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("net-new id: want InvalidArgument, got %v", err)
	}
}

// "dropbox.read" is not in staticBaseline and is not an mcp: id → rejected.
func TestValidateSkillOverlay_ManifestOnlyIdRejected(t *testing.T) {
	svc := brokerSvcWithOverlay(t, &fakeOverlayStore{}, nil)
	_, _, _, err := svc.validateSkillOverlay(context.Background(), testTenantUUID, "dropbox.read", "", "read_only")
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("manifest-only id: want InvalidArgument, got %v", err)
	}
}

// mcp id whose connector is absent → InvalidArgument.
func TestValidateSkillOverlay_McpIdMissingConnector(t *testing.T) {
	svc := brokerSvcWithOverlay(t, &fakeOverlayStore{}, []string{"other-conn"})
	_, _, _, err := svc.validateSkillOverlay(context.Background(), testTenantUUID, "mcp:missing-conn:echo", "", "write_external")
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("mcp missing connector: want InvalidArgument, got %v", err)
	}
}

// mcp id with live connector → accepted; executor_kind=mcp, scope=mcp:<conn>.
func TestValidateSkillOverlay_McpIdLiveConnector(t *testing.T) {
	connID := "conn-abc"
	svc := brokerSvcWithOverlay(t, &fakeOverlayStore{}, []string{connID})
	execKind, derivedScope, _, err := svc.validateSkillOverlay(context.Background(), testTenantUUID, "mcp:"+connID+":echo", "", "write_external")
	if err != nil {
		t.Fatalf("mcp live connector: want success, got %v", err)
	}
	if execKind != "mcp" {
		t.Errorf("executor_kind: got %q, want mcp", execKind)
	}
	if derivedScope != "mcp:"+connID {
		t.Errorf("scope: got %q, want mcp:%s", derivedScope, connID)
	}
}

// Built-in: class downgrade → InvalidArgument (propagated from ResolveOverlayClass).
// email.draft baseline is write_external (severity 2); "read_only" (severity 1) is weaker → rejected.
func TestValidateSkillOverlay_BuiltinClassDowngradeRejected(t *testing.T) {
	svc := brokerSvcWithOverlay(t, &fakeOverlayStore{}, nil)
	_, _, _, err := svc.validateSkillOverlay(context.Background(), testTenantUUID, "email.draft", "", "read_only")
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("builtin downgrade: want InvalidArgument, got %v", err)
	}
}

// Supplied scope conflicts with derived → InvalidArgument.
func TestValidateSkillOverlay_SuppliedScopeConflict(t *testing.T) {
	svc := brokerSvcWithOverlay(t, &fakeOverlayStore{}, nil)
	// doc.write derives "doc:write"; supply "web:read" instead → conflict.
	_, _, _, err := svc.validateSkillOverlay(context.Background(), testTenantUUID, "doc.write", "web:read", "write_local")
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("scope conflict: want InvalidArgument, got %v", err)
	}
}

// Built-in with matching (empty) scope and equal-or-stricter class → accepted.
func TestValidateSkillOverlay_BuiltinAccepted(t *testing.T) {
	svc := brokerSvcWithOverlay(t, &fakeOverlayStore{}, nil)
	// doc.write is a baseline tool; "write_external" is stricter than its baseline.
	execKind, derivedScope, _, err := svc.validateSkillOverlay(context.Background(), testTenantUUID, "doc.write", "", "write_external")
	if err != nil {
		t.Fatalf("builtin accepted: want success, got %v", err)
	}
	if execKind != "builtin" {
		t.Errorf("executor_kind: got %q, want builtin", execKind)
	}
	if derivedScope != "doc:write" {
		t.Errorf("scope: got %q, want doc:write", derivedScope)
	}
}

// ── (b) lazy loader ───────────────────────────────────────────────────────────

// ensureLoaded reads DB once; second call uses cache (DB reads == 1).
func TestOverlayLoader_ReadsOnce(t *testing.T) {
	store := &fakeOverlayStore{
		rows: []db.SkillOverlay{{
			ToolID:       "doc.write",
			ExecutorKind: "builtin",
			Enabled:      false,
			EffectClass:  "write_local",
		}},
	}
	reg := newTestToolRegistry()
	loader := newSkillOverlayLoader(reg, store)
	ctx := context.Background()

	loader.ensureLoaded(ctx, testTenantUUID, zap.NewNop())
	loader.ensureLoaded(ctx, testTenantUUID, zap.NewNop())

	if n := store.calls.Load(); n != 1 {
		t.Errorf("ListByTenant called %d times, want 1", n)
	}
	enabled, known := reg.IsEnabled(testTenantUUID, "doc.write")
	if !known {
		t.Error("doc.write should be known after load")
	}
	if enabled {
		t.Error("doc.write should be disabled after load")
	}
}

// invalidate + ensureLoaded triggers a second DB read.
func TestOverlayLoader_InvalidateForcesReload(t *testing.T) {
	store := &fakeOverlayStore{rows: nil}
	reg := newTestToolRegistry()
	loader := newSkillOverlayLoader(reg, store)
	ctx := context.Background()

	loader.ensureLoaded(ctx, testTenantUUID, zap.NewNop())
	loader.invalidate(testTenantUUID)
	loader.ensureLoaded(ctx, testTenantUUID, zap.NewNop())

	if n := store.calls.Load(); n != 2 {
		t.Errorf("expected 2 DB reads after invalidate, got %d", n)
	}
}

// ── (c) InvokeTool overlay disable ───────────────────────────────────────────

// Overlay row enabled=false → PermissionDenied at InvokeTool.
func TestInvokeTool_OverlayDisabledRowDenies(t *testing.T) {
	store := &fakeOverlayStore{
		rows: []db.SkillOverlay{{
			ToolID:       "doc.write",
			ExecutorKind: "builtin",
			Enabled:      false,
			EffectClass:  "write_local",
		}},
	}
	m, err := capability.GenerateMinter()
	if err != nil {
		t.Fatalf("GenerateMinter: %v", err)
	}
	reg := newTestToolRegistry()
	loader := newSkillOverlayLoader(reg, store)
	loader.ensureLoaded(context.Background(), testTenantUUID, zap.NewNop())

	svc := NewSandboxService(Deps{
		Logger:        zap.NewNop(),
		Capability:    m,
		ToolRegistry:  reg,
		SkillOverlay:  store,
		OverlayLoader: loader,
		TenantID:      testTenantUUID,
	})

	tok := mintFor(t, m, "doc:write")
	_, err = svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId:          "task-1",
		ToolId:          "doc.write",
		TenantId:        testTenantUUID,
		CapabilityToken: tok,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("overlay-disabled tool: want PermissionDenied, got %v", err)
	}
}

// Un-curated tool (no overlay row) passes through (I7: additive, not deny-by-default).
func TestInvokeTool_NoCuratedRowAllowsThrough(t *testing.T) {
	store := &fakeOverlayStore{rows: nil}
	m, err := capability.GenerateMinter()
	if err != nil {
		t.Fatalf("GenerateMinter: %v", err)
	}
	proxy := toolproxy.New(toolproxy.Config{}, zap.NewNop())
	proxy.Register("doc.write", func(_ context.Context, _ toolproxy.Request) (map[string]any, int64, error) {
		return map[string]any{"ok": true}, 1, nil
	})
	reg := newTestToolRegistry()
	loader := newSkillOverlayLoader(reg, store)
	loader.ensureLoaded(context.Background(), testTenantUUID, zap.NewNop())

	svc := NewSandboxService(Deps{
		Logger:        zap.NewNop(),
		Capability:    m,
		ToolProxy:     proxy,
		ToolRegistry:  reg,
		SkillOverlay:  store,
		OverlayLoader: loader,
		TenantID:      testTenantUUID,
	})

	tok := mintFor(t, m, "doc:write")
	resp, err := svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
		TaskId:          "task-1",
		ToolId:          "doc.write",
		TenantId:        testTenantUUID,
		CapabilityToken: tok,
	})
	if err != nil {
		t.Fatalf("un-curated tool should pass through: %v", err)
	}
	if !resp.Success {
		t.Errorf("expected success, got error %q", resp.Error)
	}
}

// ── (d) SubmitPlan overlay disable ───────────────────────────────────────────

// A DB row whose effect_class string is unrecognised must NOT be cached.
// After ensureLoaded, EffectClassForTenant must return (_, false) for that
// tool — i.e. the overlay has no authoritative entry and the registry falls
// back to the global default, not EFFECT_CLASS_UNSPECIFIED with ok=true.
func TestOverlayLoader_BadClassRowSkipped(t *testing.T) {
	store := &fakeOverlayStore{
		rows: []db.SkillOverlay{{
			ToolID:       "doc.write",
			ExecutorKind: "builtin",
			Enabled:      true,
			EffectClass:  "bogus_class",
		}},
	}
	reg := newTestToolRegistry()
	loader := newSkillOverlayLoader(reg, store)
	loader.ensureLoaded(context.Background(), testTenantUUID, zap.NewNop())

	// The bad row must be silently dropped — the overlay must have no entry
	// for this tool. IsEnabled(known=false) proves the row never reached
	// SetTenantOverlay. If it had been cached with EffectClass=UNSPECIFIED the
	// overlay would return (UNSPECIFIED, true) from EffectClassForTenant,
	// which is the routing downgrade we are guarding against; checking IsEnabled
	// is the direct proxy without coupling to global-baseline behaviour.
	_, known := reg.IsEnabled(testTenantUUID, "doc.write")
	if known {
		t.Errorf("bad-class row was cached in overlay (known=true), want known=false (row must be skipped)")
	}
}

// A step whose tool has NO overlay row at all must not be denied by the
// overlay layer at plan time (I7: overlay is additive, not deny-by-default).
func TestSubmitPlan_UncuratedStepAllowed(t *testing.T) {
	store := &fakeOverlayStore{rows: nil} // no overlay rows for any tool
	reg := newTestToolRegistry()
	loader := newSkillOverlayLoader(reg, store)
	loader.ensureLoaded(context.Background(), testTenantUUID, zap.NewNop())

	taskStore := &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}
	pol := &fakePolicyEngine{
		planAllow: true,
		toolDec:   &policy.Decision{Allow: true},
	}

	svc := NewSandboxService(Deps{
		Logger:        zap.NewNop(),
		ToolRegistry:  reg,
		SkillOverlay:  store,
		OverlayLoader: loader,
		TenantID:      testTenantUUID,
		Audit:         nopAuditEmitter(t),
		Tasks:         taskStore,
	})
	svc.policy = pol

	result, err := svc.SubmitPlan(context.Background(), cp3SubmitPlanReq(testTenantUUID, "doc.write"))
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}
	if result.Outcome == planv1.ValidationOutcome_DENIED {
		t.Errorf("un-curated step must not be DENIED by overlay layer, got DENIED")
	}
}

// Overlay row enabled=false → SubmitPlan step outcome DENIED.
func TestSubmitPlan_OverlayDisabledStepDenied(t *testing.T) {
	store := &fakeOverlayStore{
		rows: []db.SkillOverlay{{
			ToolID:       "doc.write",
			ExecutorKind: "builtin",
			Enabled:      false,
			EffectClass:  "write_local",
		}},
	}
	reg := newTestToolRegistry()
	loader := newSkillOverlayLoader(reg, store)
	loader.ensureLoaded(context.Background(), testTenantUUID, zap.NewNop())

	taskStore := &fakeTaskStore{task: newFakeTask(db.TaskStateCreated)}
	pol := &fakePolicyEngine{
		planAllow: true,
		toolDec:   &policy.Decision{Allow: true},
	}

	svc := NewSandboxService(Deps{
		Logger:        zap.NewNop(),
		ToolRegistry:  reg,
		SkillOverlay:  store,
		OverlayLoader: loader,
		TenantID:      testTenantUUID,
		Audit:         nopAuditEmitter(t),
		Tasks:         taskStore,
	})
	svc.policy = pol

	result, err := svc.SubmitPlan(context.Background(), cp3SubmitPlanReq(testTenantUUID, "doc.write"))
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}
	if result.Outcome != planv1.ValidationOutcome_DENIED {
		t.Errorf("overlay-disabled step: want DENIED, got %v", result.Outcome)
	}
}
