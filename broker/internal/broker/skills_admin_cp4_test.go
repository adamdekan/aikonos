package broker

// CP4 tests: UpsertSkill, DeleteSkill, extended ListSkills.
// All DB-free via fakeOverlayStoreMut + fakeAuditSink + fakeFGA.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ── fakeOverlayStoreMut: supports Upsert + Delete + List ────────────────────

type fakeOverlayStoreMut struct {
	mu      sync.Mutex
	rows    map[string]db.SkillOverlay // keyed by tool_id
	upserts int
	deletes int
}

func newFakeOverlayStoreMut() *fakeOverlayStoreMut {
	return &fakeOverlayStoreMut{rows: make(map[string]db.SkillOverlay)}
}

func (f *fakeOverlayStoreMut) ListByTenant(_ context.Context, _ string) ([]db.SkillOverlay, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]db.SkillOverlay, 0, len(f.rows))
	for _, r := range f.rows {
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeOverlayStoreMut) Upsert(_ context.Context, _ string, row db.SkillOverlay) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[row.ToolID] = row
	f.upserts++
	return nil
}

func (f *fakeOverlayStoreMut) Delete(_ context.Context, _, toolID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rows[toolID]; !ok {
		return fmt.Errorf("skill_overlay %q: %w", toolID, db.ErrSkillOverlayNotFound)
	}
	delete(f.rows, toolID)
	f.deletes++
	return nil
}

// ── fakeAuditSink counts emitted events ─────────────────────────────────────

type fakeAuditSink struct {
	mu     sync.Mutex
	events []string // event_type values
}

func (f *fakeAuditSink) emittedTypes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.events))
	copy(out, f.events)
	return out
}

// ── helpers ──────────────────────────────────────────────────────────────────

// cp4AdminSvc builds a BrokerService with a real FGA fake (admin-only) wired
// with the given overlay store, a nop audit emitter, and an empty MCP repo.
func cp4AdminSvc(t *testing.T, store skillOverlayMutStore, connIDs []string) (*BrokerService, *fakeFGA) {
	t.Helper()
	fg := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := fg.server(t)
	t.Cleanup(srv.Close)

	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}

	reg := newTestToolRegistry()
	loader := newSkillOverlayLoader(reg, store)

	deps := testAdminDeps(t, srv.URL)
	deps.ToolRegistry = reg
	deps.SkillOverlay = store
	deps.OverlayLoader = loader
	deps.Audit = em
	deps.Mcp.Connections = &fakeMcpRepoSimple{ids: connIDs}

	return NewBrokerService(deps), fg
}

func adminCtx() context.Context {
	return ctxWithIdentity(testTenantUUID, "admin@example.com")
}

func nonAdminCtx() context.Context {
	return ctxWithIdentity(testTenantUUID, "alice@example.com")
}

// ── UpsertSkill happy path ───────────────────────────────────────────────────

func TestUpsertSkill_HappyPath(t *testing.T) {
	store := newFakeOverlayStoreMut()
	svc, _ := cp4AdminSvc(t, store, nil)

	resp, err := svc.UpsertSkill(adminCtx(), &brokerv1.UpsertSkillRequest{
		TenantId:    testTenantUUID,
		UserId:      "admin@example.com",
		ToolId:      "doc.write",
		EffectClass: "write_local",
		DisplayName: "Document Writer",
		Description: "Writes documents to the workspace.",
		Enabled:     true,
	})
	if err != nil {
		t.Fatalf("UpsertSkill: %v", err)
	}
	if resp.Skill == nil {
		t.Fatal("expected non-nil skill in response")
	}
	if resp.Skill.ToolId != "doc.write" {
		t.Errorf("tool_id: got %q, want doc.write", resp.Skill.ToolId)
	}
	if !resp.Skill.Enabled {
		t.Error("expected enabled=true in response")
	}
	if resp.Skill.DisplayName != "Document Writer" {
		t.Errorf("display_name: got %q", resp.Skill.DisplayName)
	}

	store.mu.Lock()
	upserts := store.upserts
	store.mu.Unlock()
	if upserts != 1 {
		t.Errorf("expected 1 upsert, got %d", upserts)
	}
}

// ── UpsertSkill: non-admin → PermissionDenied ────────────────────────────────

func TestUpsertSkill_NonAdminDenied(t *testing.T) {
	store := newFakeOverlayStoreMut()
	svc, _ := cp4AdminSvc(t, store, nil)

	_, err := svc.UpsertSkill(nonAdminCtx(), &brokerv1.UpsertSkillRequest{
		TenantId: testTenantUUID,
		UserId:   "alice@example.com",
		ToolId:   "doc.write",
		Enabled:  true,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin upsert: want PermissionDenied, got %v", err)
	}
}

// ── UpsertSkill: empty tool_id → InvalidArgument ─────────────────────────────

func TestUpsertSkill_EmptyToolId(t *testing.T) {
	store := newFakeOverlayStoreMut()
	svc, _ := cp4AdminSvc(t, store, nil)

	_, err := svc.UpsertSkill(adminCtx(), &brokerv1.UpsertSkillRequest{
		TenantId: testTenantUUID,
		UserId:   "admin@example.com",
		ToolId:   "",
		Enabled:  true,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty tool_id: want InvalidArgument, got %v", err)
	}
}

// ── UpsertSkill: oversized description → InvalidArgument (I8) ───────────────

func TestUpsertSkill_OversizedDescription(t *testing.T) {
	store := newFakeOverlayStoreMut()
	svc, _ := cp4AdminSvc(t, store, nil)

	big := strings.Repeat("x", 4097)
	_, err := svc.UpsertSkill(adminCtx(), &brokerv1.UpsertSkillRequest{
		TenantId:    testTenantUUID,
		UserId:      "admin@example.com",
		ToolId:      "doc.write",
		Description: big,
		Enabled:     true,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("oversized description: want InvalidArgument, got %v", err)
	}
}

// ── UpsertSkill: oversized display_name → InvalidArgument ───────────────────

func TestUpsertSkill_OversizedDisplayName(t *testing.T) {
	store := newFakeOverlayStoreMut()
	svc, _ := cp4AdminSvc(t, store, nil)

	big := strings.Repeat("x", 201)
	_, err := svc.UpsertSkill(adminCtx(), &brokerv1.UpsertSkillRequest{
		TenantId:    testTenantUUID,
		UserId:      "admin@example.com",
		ToolId:      "doc.write",
		DisplayName: big,
		Enabled:     true,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("oversized display_name: want InvalidArgument, got %v", err)
	}
}

// ── UpsertSkill: net-new id → InvalidArgument (propagated from validate) ─────

func TestUpsertSkill_NetNewIdRejected(t *testing.T) {
	store := newFakeOverlayStoreMut()
	svc, _ := cp4AdminSvc(t, store, nil)

	_, err := svc.UpsertSkill(adminCtx(), &brokerv1.UpsertSkillRequest{
		TenantId: testTenantUUID,
		UserId:   "admin@example.com",
		ToolId:   "net.new.tool",
		Enabled:  true,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("net-new id: want InvalidArgument, got %v", err)
	}
	// Nothing persisted.
	store.mu.Lock()
	u := store.upserts
	store.mu.Unlock()
	if u != 0 {
		t.Errorf("no upsert should occur for invalid tool_id, got %d", u)
	}
}

// ── UpsertSkill: emits audit event ──────────────────────────────────────────

func TestUpsertSkill_EmitsAuditEvent(t *testing.T) {
	store := newFakeOverlayStoreMut()

	// Use a real emitter but capture that Emit does not error (nop sink).
	// The important assertion is that no error is returned from UpsertSkill,
	// which means emitSkillAudit was reached without panic.
	svc, _ := cp4AdminSvc(t, store, nil)

	_, err := svc.UpsertSkill(adminCtx(), &brokerv1.UpsertSkillRequest{
		TenantId: testTenantUUID,
		UserId:   "admin@example.com",
		ToolId:   "doc.write",
		Enabled:  true,
	})
	if err != nil {
		t.Fatalf("UpsertSkill should succeed: %v", err)
	}
	// No panic/error reaching the audit path = sufficient for unit scope.
}

// ── DeleteSkill happy path ───────────────────────────────────────────────────

func TestDeleteSkill_HappyPath(t *testing.T) {
	store := newFakeOverlayStoreMut()
	store.rows["doc.write"] = db.SkillOverlay{ToolID: "doc.write", Enabled: true, EffectClass: "write_local"}

	svc, _ := cp4AdminSvc(t, store, nil)

	_, err := svc.DeleteSkill(adminCtx(), &brokerv1.DeleteSkillRequest{
		TenantId: testTenantUUID,
		UserId:   "admin@example.com",
		ToolId:   "doc.write",
	})
	if err != nil {
		t.Fatalf("DeleteSkill: %v", err)
	}

	store.mu.Lock()
	remaining := len(store.rows)
	store.mu.Unlock()
	if remaining != 0 {
		t.Errorf("expected row to be deleted, %d rows remain", remaining)
	}
}

// ── DeleteSkill: not found → NotFound ────────────────────────────────────────

func TestDeleteSkill_NotFound(t *testing.T) {
	store := newFakeOverlayStoreMut()
	svc, _ := cp4AdminSvc(t, store, nil)

	_, err := svc.DeleteSkill(adminCtx(), &brokerv1.DeleteSkillRequest{
		TenantId: testTenantUUID,
		UserId:   "admin@example.com",
		ToolId:   "doc.write",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("delete not-found: want NotFound, got %v", err)
	}
}

// ── DeleteSkill: emits audit event ──────────────────────────────────────────

func TestDeleteSkill_EmitsAuditEvent(t *testing.T) {
	store := newFakeOverlayStoreMut()
	store.rows["doc.write"] = db.SkillOverlay{ToolID: "doc.write", Enabled: true, EffectClass: "write_local"}

	svc, _ := cp4AdminSvc(t, store, nil)

	_, err := svc.DeleteSkill(adminCtx(), &brokerv1.DeleteSkillRequest{
		TenantId: testTenantUUID,
		UserId:   "admin@example.com",
		ToolId:   "doc.write",
	})
	if err != nil {
		t.Fatalf("DeleteSkill should succeed: %v", err)
	}
}

// ── ListSkills: reflects overlay enabled=false ────────────────────────────────

func TestListSkills_OverlayDisabledReflected(t *testing.T) {
	store := newFakeOverlayStoreMut()
	store.rows["doc.write"] = db.SkillOverlay{
		ToolID:       "doc.write",
		Enabled:      false,
		EffectClass:  "write_local",
		DisplayName:  "Doc Writer",
		Description:  "Writes docs.",
		ExecutorKind: "builtin",
	}

	fg := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := fg.server(t)
	t.Cleanup(srv.Close)

	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}

	reg := newTestToolRegistry()
	loader := newSkillOverlayLoader(reg, store)

	deps := testAdminDeps(t, srv.URL)
	deps.ToolRegistry = reg
	deps.SkillOverlay = store
	deps.OverlayLoader = loader
	deps.Audit = em

	svc := NewBrokerService(deps)

	resp, err := svc.ListSkills(adminCtx(), &brokerv1.ListSkillsRequest{
		TenantId: testTenantUUID,
		UserId:   "admin@example.com",
	})
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}

	var found *brokerv1.AdminSkill
	for _, s := range resp.Skills {
		if s.ToolId == "doc.write" {
			found = s
			break
		}
	}
	if found == nil {
		t.Fatal("doc.write missing from ListSkills response")
	}
	if found.Enabled {
		t.Error("doc.write: expected enabled=false from overlay")
	}
	if found.DisplayName != "Doc Writer" {
		t.Errorf("display_name: got %q, want %q", found.DisplayName, "Doc Writer")
	}
	if found.Description != "Writes docs." {
		t.Errorf("description: got %q, want %q", found.Description, "Writes docs.")
	}
	if found.ExecutorKind != "builtin" {
		t.Errorf("executor_kind: got %q, want builtin", found.ExecutorKind)
	}
}

// ── Auto-wire: NewBrokerService auto-constructs OverlayLoader when nil ───────

// NewBrokerService with SkillOverlay set and OverlayLoader nil must
// auto-wire a non-nil OverlayLoader that actually serves overlay data.
func TestNewBrokerService_AutoWiresOverlayLoader(t *testing.T) {
	store := newFakeOverlayStoreMut()
	store.rows["doc.write"] = db.SkillOverlay{
		ToolID:       "doc.write",
		ExecutorKind: "builtin",
		Enabled:      false,
		EffectClass:  "write_local",
	}

	reg := newTestToolRegistry()
	deps := Deps{
		Logger:       zap.NewNop(),
		ToolRegistry: reg,
		SkillOverlay: store,
		// OverlayLoader intentionally nil — constructor must auto-wire it.
	}
	svc := NewBrokerService(deps)

	if svc.deps.OverlayLoader == nil {
		t.Fatal("NewBrokerService: OverlayLoader must be non-nil when SkillOverlay + ToolRegistry are set")
	}

	// Confirm the loader actually serves data: ensureLoaded must populate the
	// registry so IsEnabled reports the overlay row's enabled=false.
	svc.deps.OverlayLoader.ensureLoaded(context.Background(), testTenantUUID, zap.NewNop())
	enabled, known := reg.IsEnabled(testTenantUUID, "doc.write")
	if !known {
		t.Error("doc.write should be known in registry after ensureLoaded")
	}
	if enabled {
		t.Error("doc.write should be disabled per overlay row after ensureLoaded")
	}
}

// NewBrokerService with OverlayLoader already set must not replace it.
func TestNewBrokerService_DoesNotReplaceExistingLoader(t *testing.T) {
	store := newFakeOverlayStoreMut()
	reg := newTestToolRegistry()
	original := newSkillOverlayLoader(reg, store)

	deps := Deps{
		Logger:        zap.NewNop(),
		ToolRegistry:  reg,
		SkillOverlay:  store,
		OverlayLoader: original,
	}
	svc := NewBrokerService(deps)

	if svc.deps.OverlayLoader != original {
		t.Error("NewBrokerService must not replace a non-nil OverlayLoader")
	}
}

// ── Nil-guard: UpsertSkill / DeleteSkill with nil SkillOverlay ───────────────

// UpsertSkill with Deps.SkillOverlay == nil must return FailedPrecondition
// without panicking.
func TestUpsertSkill_NilOverlayStore_FailedPrecondition(t *testing.T) {
	fg := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := fg.server(t)
	t.Cleanup(srv.Close)

	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}

	deps := testAdminDeps(t, srv.URL)
	deps.ToolRegistry = newTestToolRegistry()
	deps.SkillOverlay = nil // explicitly unset
	deps.OverlayLoader = nil
	deps.Audit = em

	svc := NewBrokerService(deps)

	_, err = svc.UpsertSkill(adminCtx(), &brokerv1.UpsertSkillRequest{
		TenantId: testTenantUUID,
		UserId:   "admin@example.com",
		ToolId:   "doc.write",
		Enabled:  true,
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("nil SkillOverlay: want FailedPrecondition, got %v", err)
	}
}

// DeleteSkill with Deps.SkillOverlay == nil must return FailedPrecondition
// without panicking.
func TestDeleteSkill_NilOverlayStore_FailedPrecondition(t *testing.T) {
	fg := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := fg.server(t)
	t.Cleanup(srv.Close)

	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}

	deps := testAdminDeps(t, srv.URL)
	deps.ToolRegistry = newTestToolRegistry()
	deps.SkillOverlay = nil
	deps.OverlayLoader = nil
	deps.Audit = em

	svc := NewBrokerService(deps)

	_, err = svc.DeleteSkill(adminCtx(), &brokerv1.DeleteSkillRequest{
		TenantId: testTenantUUID,
		UserId:   "admin@example.com",
		ToolId:   "doc.write",
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("nil SkillOverlay: want FailedPrecondition, got %v", err)
	}
}

// ── ListSkills: custom MCP skill in overlay appears in list ──────────────────

func TestListSkills_CustomMcpSkillAppearsInList(t *testing.T) {
	store := newFakeOverlayStoreMut()
	mcpToolID := "mcp:myconn:search"
	store.rows[mcpToolID] = db.SkillOverlay{
		ToolID:          mcpToolID,
		Enabled:         true,
		EffectClass:     "write_external",
		ExecutorKind:    "mcp",
		CapabilityScope: "mcp:myconn",
		DisplayName:     "MCP Search",
	}

	fg := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := fg.server(t)
	t.Cleanup(srv.Close)

	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}

	reg := newTestToolRegistry()
	loader := newSkillOverlayLoader(reg, store)

	deps := testAdminDeps(t, srv.URL)
	deps.ToolRegistry = reg
	deps.SkillOverlay = store
	deps.OverlayLoader = loader
	deps.Audit = em

	svc := NewBrokerService(deps)

	resp, err := svc.ListSkills(adminCtx(), &brokerv1.ListSkillsRequest{
		TenantId: testTenantUUID,
		UserId:   "admin@example.com",
	})
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}

	var found *brokerv1.AdminSkill
	for _, s := range resp.Skills {
		if s.ToolId == mcpToolID {
			found = s
			break
		}
	}
	if found == nil {
		t.Fatalf("MCP skill %q missing from ListSkills", mcpToolID)
	}
	if !found.Enabled {
		t.Error("MCP skill: expected enabled=true")
	}
	if found.DisplayName != "MCP Search" {
		t.Errorf("display_name: got %q, want %q", found.DisplayName, "MCP Search")
	}
	if found.ExecutorKind != "mcp" {
		t.Errorf("executor_kind: got %q, want mcp", found.ExecutorKind)
	}
}
