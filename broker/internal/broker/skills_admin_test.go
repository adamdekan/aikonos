package broker

import (
	"sort"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// TestListSkills_NonAdminDenied: non-admin caller must get PermissionDenied.
func TestListSkills_NonAdminDenied(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testAdminDeps(t, srv.URL))

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.ListSkills(ctx, &brokerv1.ListSkillsRequest{
		TenantId: testTenantUUID,
		UserId:   "alice@example.com",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin should be denied, got %v", err)
	}
}

// TestListSkills_ForgedUserIdRejected: body user_id that disagrees with the
// OIDC context must be PermissionDenied (callerIdentity rejects it).
func TestListSkills_ForgedUserIdRejected(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testAdminDeps(t, srv.URL))

	// Context says alice, body claims to be admin — callerIdentity must reject.
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	_, err := svc.ListSkills(ctx, &brokerv1.ListSkillsRequest{
		TenantId: testTenantUUID,
		UserId:   "admin@example.com",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("forged user_id should be PermissionDenied, got %v", err)
	}
}

// TestListSkills_AdminHappyPath: admin caller gets the full registry, sorted,
// and capability skills (scheduler) are present even though they are not tools.
func TestListSkills_AdminHappyPath(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()

	reg := newTestToolRegistry()
	deps := testAdminDeps(t, srv.URL)
	deps.ToolRegistry = reg
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.ListSkills(ctx, &brokerv1.ListSkillsRequest{
		TenantId: testTenantUUID,
		UserId:   "admin@example.com",
	})
	if err != nil {
		t.Fatalf("admin list skills: %v", err)
	}
	if len(resp.Skills) == 0 {
		t.Fatal("expected at least one skill, got none")
	}
	// Must include at least the known baseline tools.
	byID := make(map[string]string, len(resp.Skills))
	for _, s := range resp.Skills {
		byID[s.ToolId] = s.Scope
	}
	if byID["web.fetch"] != "web:read" {
		t.Errorf("web.fetch scope: got %q, want %q", byID["web.fetch"], "web:read")
	}
	if byID["doc.write"] != "doc:write" {
		t.Errorf("doc.write scope: got %q, want %q", byID["doc.write"], "doc:write")
	}
	// skill:scheduler is FGA-gated (not a tool) — must appear with empty scope
	// so the admin console treats grants referencing it as legitimate.
	if _, ok := byID["scheduler"]; !ok {
		t.Error("scheduler capability skill missing from ListSkills response")
	}
	if byID["scheduler"] != "" {
		t.Errorf("scheduler scope: got %q, want %q (empty — no Biscuit scope)", byID["scheduler"], "")
	}
	// skill:vision is registered the same way as workflows/scheduler — a
	// capability-only FGA gate, not an invokable tool.
	if _, ok := byID["vision"]; !ok {
		t.Error("vision capability skill missing from ListSkills response")
	}
	if byID["vision"] != "" {
		t.Errorf("vision scope: got %q, want %q (empty — no Biscuit scope)", byID["vision"], "")
	}
	// Result must be sorted deterministically by tool_id.
	if !sort.SliceIsSorted(resp.Skills, func(i, j int) bool {
		return resp.Skills[i].ToolId < resp.Skills[j].ToolId
	}) {
		t.Error("skills not sorted by tool_id")
	}
}

// TestListSkills_FGADisabled: when FGA is disabled (no store), requireTenantAdmin
// uses the allow-all stub → ListSkills still succeeds (read-only vocabulary, no
// mutation). This mirrors ListAssignments' behavior under the same condition.
func TestListSkills_FGADisabled(t *testing.T) {
	// Pass empty fgaURL → no OpenFGA store → allow-all stub.
	deps := testAdminDeps(t, "")
	deps.ToolRegistry = newTestToolRegistry()
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	resp, err := svc.ListSkills(ctx, &brokerv1.ListSkillsRequest{
		TenantId: testTenantUUID,
		UserId:   "alice@example.com",
	})
	if err != nil {
		t.Fatalf("FGA-disabled list skills should succeed, got %v", err)
	}
	if len(resp.Skills) == 0 {
		t.Error("expected skills even in FGA-disabled mode")
	}
}
