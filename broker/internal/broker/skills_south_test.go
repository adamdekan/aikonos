package broker

// skills_south_test.go — unit tests for ListUserSkills (south-bound, SPIFFE-gated).
//
// Contract:
//   - No SPIFFE peer → PermissionDenied
//   - Wrong SPIFFE peer → PermissionDenied
//   - Missing tenant_id → InvalidArgument
//   - Missing user_id → InvalidArgument
//   - FGA disabled → all registry tools returned
//   - FGA enabled + granted → tool included
//   - FGA enabled + denied → tool excluded (fail-closed per-tool, no error)
//   - FGA enabled + CheckFGA error → tool excluded (fail-closed per-tool, no error)

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func newSkillsSouthSvc(t *testing.T, sp skillGatePolicy) *SandboxService {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	reg := newTestToolRegistry()
	deps := Deps{
		Logger:          zap.NewNop(),
		Audit:           em,
		GatewaySpiffeID: testGateway,
		TenantID:        testTenant,
		ToolRegistry:    reg,
	}
	svc := NewSandboxService(deps)
	svc.skillPolicy = sp
	return svc
}

// ── SPIFFE gate tests ─────────────────────────────────────────────────────────

func TestListUserSkills_NoPeer_PermissionDenied(t *testing.T) {
	svc := newSkillsSouthSvc(t, &fakeSkillPolicy{enabled: false})
	_, err := svc.ListUserSkills(context.Background(), &brokerv1.ListUserSkillsRequest{
		TenantId: testTenant,
		UserId:   "alice",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("no peer: want PermissionDenied, got %v", err)
	}
}

func TestListUserSkills_WrongPeer_PermissionDenied(t *testing.T) {
	svc := newSkillsSouthSvc(t, &fakeSkillPolicy{enabled: false})
	ctx := gatewayCtx("spiffe://aikonos.com/wrong-svc")
	_, err := svc.ListUserSkills(ctx, &brokerv1.ListUserSkillsRequest{
		TenantId: testTenant,
		UserId:   "alice",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("wrong peer: want PermissionDenied, got %v", err)
	}
}

// ── validation tests ──────────────────────────────────────────────────────────

func TestListUserSkills_EmptyTenant_InvalidArgument(t *testing.T) {
	svc := newSkillsSouthSvc(t, &fakeSkillPolicy{enabled: false})
	ctx := gatewayCtx(testGateway)
	_, err := svc.ListUserSkills(ctx, &brokerv1.ListUserSkillsRequest{UserId: "alice"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty tenant: want InvalidArgument, got %v", err)
	}
}

func TestListUserSkills_EmptyUser_InvalidArgument(t *testing.T) {
	svc := newSkillsSouthSvc(t, &fakeSkillPolicy{enabled: false})
	ctx := gatewayCtx(testGateway)
	_, err := svc.ListUserSkills(ctx, &brokerv1.ListUserSkillsRequest{TenantId: testTenant})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty user: want InvalidArgument, got %v", err)
	}
}

// ── FGA-disabled: full registry returned ─────────────────────────────────────

func TestListUserSkills_FGADisabled_ReturnsAllTools(t *testing.T) {
	sp := &fakeSkillPolicy{enabled: false}
	svc := newSkillsSouthSvc(t, sp)
	ctx := gatewayCtx(testGateway)

	resp, err := svc.ListUserSkills(ctx, &brokerv1.ListUserSkillsRequest{
		TenantId: testTenant,
		UserId:   "alice",
	})
	if err != nil {
		t.Fatalf("ListUserSkills: %v", err)
	}
	if len(resp.Skills) == 0 {
		t.Fatal("expected non-empty skills list when FGA disabled")
	}
	// CheckFGA must never be called when FGA is disabled.
	if sp.checkFGACalls != 0 {
		t.Errorf("FGA disabled: CheckFGA called %d times, want 0", sp.checkFGACalls)
	}
	// Registry tools must have non-empty tool_id and scope; capability skills
	// (e.g. "workflows", "scheduler") carry empty scope by design and are
	// included unconditionally when FGA is disabled — so only enforce non-empty
	// scope for items that are not capability skills.
	capSet := make(map[string]bool, len(capabilitySkills))
	for _, id := range capabilitySkills {
		capSet[id] = true
	}
	for _, s := range resp.Skills {
		if s.ToolId == "" {
			t.Errorf("empty tool_id in SkillItem: %+v", s)
		}
		if !capSet[s.ToolId] && s.Scope == "" {
			t.Errorf("registry tool missing scope in SkillItem: %+v", s)
		}
	}
}

// ── FGA-enabled: granted tool included ───────────────────────────────────────

func TestListUserSkills_FGAEnabled_GrantedToolIncluded(t *testing.T) {
	sp := &fakeSkillPolicy{enabled: true, granted: true}
	svc := newSkillsSouthSvc(t, sp)
	ctx := gatewayCtx(testGateway)

	resp, err := svc.ListUserSkills(ctx, &brokerv1.ListUserSkillsRequest{
		TenantId: testTenant,
		UserId:   "alice",
	})
	if err != nil {
		t.Fatalf("ListUserSkills: %v", err)
	}
	if len(resp.Skills) == 0 {
		t.Fatal("expected skills when all grants succeed")
	}
	// CheckFGA must have been called (at least once per tool in registry).
	if sp.checkFGACalls == 0 {
		t.Error("FGA enabled: CheckFGA was never called")
	}
	// The user/relation args must follow the skill gate convention.
	if sp.lastUser != "user:alice" {
		t.Errorf("lastUser: want %q, got %q", "user:alice", sp.lastUser)
	}
	if sp.lastRelation != "can_invoke" {
		t.Errorf("lastRelation: want %q, got %q", "can_invoke", sp.lastRelation)
	}
}

// ── FGA-enabled: denied tool excluded, no error surfaced ─────────────────────

func TestListUserSkills_FGAEnabled_DeniedToolExcluded(t *testing.T) {
	sp := &fakeSkillPolicy{enabled: true, granted: false}
	svc := newSkillsSouthSvc(t, sp)
	ctx := gatewayCtx(testGateway)

	resp, err := svc.ListUserSkills(ctx, &brokerv1.ListUserSkillsRequest{
		TenantId: testTenant,
		UserId:   "alice",
	})
	if err != nil {
		t.Fatalf("ListUserSkills: %v", err)
	}
	if len(resp.Skills) != 0 {
		t.Errorf("all denied: want empty skills, got %d", len(resp.Skills))
	}
}

// ── FGA-enabled: CheckFGA error → tool excluded, no error surfaced ────────────

func TestListUserSkills_FGAEnabled_CheckFGAError_ToolExcluded(t *testing.T) {
	sp := &fakeSkillPolicy{enabled: true, err: context.DeadlineExceeded}
	svc := newSkillsSouthSvc(t, sp)
	ctx := gatewayCtx(testGateway)

	resp, err := svc.ListUserSkills(ctx, &brokerv1.ListUserSkillsRequest{
		TenantId: testTenant,
		UserId:   "alice",
	})
	if err != nil {
		t.Fatalf("CheckFGA error must not surface: %v", err)
	}
	if len(resp.Skills) != 0 {
		t.Errorf("CheckFGA error should exclude tool: got %d skills", len(resp.Skills))
	}
}

// ── capability skills (scheduler, workflows) surface correctly ────────────────
//
// Personal-session skill resolution must surface held capability skills;
// without this, workflow Pi tools are dead in chat for every user.

func TestListUserSkills_CapabilitySkill_FGAGranted_Included(t *testing.T) {
	// Grant everything so registry tools pass too; we only care that "workflows"
	// appears and carries an empty scope (no Biscuit scope for capability skills).
	sp := &fakeSkillPolicy{enabled: true, granted: true}
	svc := newSkillsSouthSvc(t, sp)
	ctx := gatewayCtx(testGateway)

	resp, err := svc.ListUserSkills(ctx, &brokerv1.ListUserSkillsRequest{
		TenantId: testTenant,
		UserId:   "alice",
	})
	if err != nil {
		t.Fatalf("ListUserSkills: %v", err)
	}

	toolIDs := make(map[string]string, len(resp.Skills)) // tool_id → scope
	for _, s := range resp.Skills {
		toolIDs[s.ToolId] = s.Scope
	}

	// Capability skill must appear.
	if _, ok := toolIDs["workflows"]; !ok {
		t.Error("FGA granted skill:workflows but 'workflows' absent from ListUserSkills response")
	}
	// Capability skills carry empty scope — no Biscuit capability involved.
	if scope := toolIDs["workflows"]; scope != "" {
		t.Errorf("'workflows' capability skill scope: want \"\", got %q", scope)
	}
	// Registry tools must still be present (no regression).
	if _, ok := toolIDs["web.fetch"]; !ok {
		t.Error("registry tool 'web.fetch' missing from response — regression in registry loop")
	}
}

// skill:subagents gates the spawn_subagents Pi tool. Without registration in
// capabilitySkills the grant would never reach a personal chat session, so the
// tool would be dead for every user (same failure mode workflows had).
func TestListUserSkills_SubagentsCapability_FGAGranted_Included(t *testing.T) {
	sp := &fakeSkillPolicy{enabled: true, granted: true}
	svc := newSkillsSouthSvc(t, sp)
	ctx := gatewayCtx(testGateway)

	resp, err := svc.ListUserSkills(ctx, &brokerv1.ListUserSkillsRequest{
		TenantId: testTenant,
		UserId:   "alice",
	})
	if err != nil {
		t.Fatalf("ListUserSkills: %v", err)
	}

	for _, s := range resp.Skills {
		if s.ToolId != "subagents" {
			continue
		}
		if s.Scope != "" {
			t.Errorf("'subagents' capability skill scope: want \"\", got %q", s.Scope)
		}
		return
	}
	t.Error("FGA granted skill:subagents but 'subagents' absent from ListUserSkills response")
}

func TestListUserSkills_SubagentsCapability_FGADenied_Excluded(t *testing.T) {
	sp := &fakeSkillPolicy{enabled: true, granted: false}
	svc := newSkillsSouthSvc(t, sp)
	ctx := gatewayCtx(testGateway)

	resp, err := svc.ListUserSkills(ctx, &brokerv1.ListUserSkillsRequest{
		TenantId: testTenant,
		UserId:   "alice",
	})
	if err != nil {
		t.Fatalf("ListUserSkills: %v", err)
	}

	for _, s := range resp.Skills {
		if s.ToolId == "subagents" {
			t.Error("FGA denied skill:subagents but 'subagents' still present in response")
		}
	}
}

func TestListUserSkills_CapabilitySkill_FGADenied_Excluded(t *testing.T) {
	// Deny everything; "workflows" must not appear.
	sp := &fakeSkillPolicy{enabled: true, granted: false}
	svc := newSkillsSouthSvc(t, sp)
	ctx := gatewayCtx(testGateway)

	resp, err := svc.ListUserSkills(ctx, &brokerv1.ListUserSkillsRequest{
		TenantId: testTenant,
		UserId:   "alice",
	})
	if err != nil {
		t.Fatalf("ListUserSkills: %v", err)
	}

	for _, s := range resp.Skills {
		if s.ToolId == "workflows" {
			t.Error("FGA denied skill:workflows but 'workflows' still present in response")
		}
	}
}
