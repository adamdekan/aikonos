// broker/internal/broker/personal_skills_south_test.go
//
// ListPersonalSkillsSouth / GetPersonalSkillSouth: the gateway-peer gate, the
// empty-not-error FGA chokepoint (granted/ungranted/error), the svc-/group-
// synthetic-user rejection, and the valid/invalid skill Get outcomes
//.
package broker

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/personalskill"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	"github.com/adamdekan/aikonos/broker/internal/workspacefs"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

const pskUser = "alice@example.com"

// personalSkillsDeps builds a south-only Deps over a tempdir workspace,
// mirroring memoryDeps's shape. An empty fgaURL leaves FGA disabled
// (CheckFGA stubs to allow); every deny test passes a live fake server.
func personalSkillsDeps(t *testing.T, fgaURL, root string) Deps {
	t.Helper()
	cfg := policy.Config{OPAEndpoint: "http://unused", OpenFGAEndpoint: fgaURL}
	if fgaURL != "" {
		cfg.OpenFGAStoreID = "store-1"
	}
	eng, err := policy.NewEngine(context.Background(), cfg)
	if err != nil {
		t.Fatalf("policy.NewEngine: %v", err)
	}
	return Deps{
		Logger:    zap.NewNop(),
		Policy:    eng,
		TenantID:  "aikonos-dev",
		Workspace: WorkspaceDeps{Local: workspacefs.New(root)},
	}
}

func pskGrantedFGA(t *testing.T) string {
	t.Helper()
	return memoryFGA(t, &fakeFGA{checks: map[string]bool{
		"user:" + pskUser + "|can_invoke|skill:personal-skills": true,
	}})
}

func writeSkillMd(t *testing.T, be workspacefs.Backend, tenant, user, name, content string) {
	t.Helper()
	if _, err := be.Write(context.Background(), tenant, user, "Skills/"+name+"/SKILL.md", []byte(content)); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
}

const demoSkillMd = "---\n" +
	"name: demo\n" +
	"description: A demo skill.\n" +
	"allowed-tools:\n" +
	"  - web.fetch\n" +
	"---\n\n" +
	"# Demo body\n"

// ── ListPersonalSkillsSouth ─────────────────────────────────────────────────

func TestListPersonalSkillsSouth_RequiresGatewayPeer(t *testing.T) {
	deps := personalSkillsDeps(t, "", t.TempDir())
	svc := NewSandboxService(deps)
	req := &brokerv1.ListPersonalSkillsSouthRequest{TenantId: testTenantUUID, UserId: pskUser}
	if _, err := svc.ListPersonalSkillsSouth(context.Background(), req); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied without a peer identity, got %v", err)
	}
}

func TestListPersonalSkillsSouth_NoSkillGrantReturnsEmpty(t *testing.T) {
	root := t.TempDir()
	deps := personalSkillsDeps(t, memoryFGA(t, &fakeFGA{}), root)
	svc := NewSandboxService(deps)
	writeSkillMd(t, deps.Workspace.Local, testTenantUUID, pskUser, "demo", demoSkillMd)

	req := &brokerv1.ListPersonalSkillsSouthRequest{TenantId: testTenantUUID, UserId: pskUser}
	resp, err := svc.ListPersonalSkillsSouth(gatewayCtxForWorkflow(), req)
	if err != nil {
		t.Fatalf("want empty response, got error %v", err)
	}
	if len(resp.Skills) != 0 {
		t.Fatalf("no skill:personal-skills grant must list nothing, got %+v", resp.Skills)
	}
}

func TestListPersonalSkillsSouth_FGAErrorReturnsEmpty(t *testing.T) {
	root := t.TempDir()
	deps := personalSkillsDeps(t, memoryFGA(t, &fakeFGA{checkErr: true}), root)
	svc := NewSandboxService(deps)
	writeSkillMd(t, deps.Workspace.Local, testTenantUUID, pskUser, "demo", demoSkillMd)

	req := &brokerv1.ListPersonalSkillsSouthRequest{TenantId: testTenantUUID, UserId: pskUser}
	resp, err := svc.ListPersonalSkillsSouth(gatewayCtxForWorkflow(), req)
	if err != nil {
		t.Fatalf("an FGA outage must fail closed to empty, not error: %v", err)
	}
	if len(resp.Skills) != 0 {
		t.Fatalf("want no skills, got %+v", resp.Skills)
	}
}

func TestListPersonalSkillsSouth_GrantedReturnsEntries(t *testing.T) {
	root := t.TempDir()
	deps := personalSkillsDeps(t, pskGrantedFGA(t), root)
	svc := NewSandboxService(deps)
	writeSkillMd(t, deps.Workspace.Local, testTenantUUID, pskUser, "demo", demoSkillMd)

	req := &brokerv1.ListPersonalSkillsSouthRequest{TenantId: testTenantUUID, UserId: pskUser}
	resp, err := svc.ListPersonalSkillsSouth(gatewayCtxForWorkflow(), req)
	if err != nil {
		t.Fatalf("ListPersonalSkillsSouth: %v", err)
	}
	if len(resp.Skills) != 1 {
		t.Fatalf("want 1 skill, got %+v", resp.Skills)
	}
	got := resp.Skills[0]
	if got.Name != "demo" || got.Description != "A demo skill." || !got.Valid {
		t.Fatalf("entry = %+v", got)
	}
	if len(got.AllowedTools) != 1 || got.AllowedTools[0] != "web.fetch" {
		t.Fatalf("allowed_tools = %v", got.AllowedTools)
	}
}

func TestListPersonalSkillsSouth_SvcPrefixedUserReturnsEmpty(t *testing.T) {
	root := t.TempDir()
	deps := personalSkillsDeps(t, pskGrantedFGA(t), root)
	svc := NewSandboxService(deps)

	req := &brokerv1.ListPersonalSkillsSouthRequest{TenantId: testTenantUUID, UserId: "svc-" + pskUser}
	resp, err := svc.ListPersonalSkillsSouth(gatewayCtxForWorkflow(), req)
	if err != nil {
		t.Fatalf("svc- user must yield empty, not error: %v", err)
	}
	if len(resp.Skills) != 0 {
		t.Fatalf("want no skills for a svc- user, got %+v", resp.Skills)
	}
}

func TestListPersonalSkillsSouth_GroupPrefixedUserReturnsEmpty(t *testing.T) {
	root := t.TempDir()
	deps := personalSkillsDeps(t, pskGrantedFGA(t), root)
	svc := NewSandboxService(deps)

	req := &brokerv1.ListPersonalSkillsSouthRequest{TenantId: testTenantUUID, UserId: "group-" + pskUser}
	resp, err := svc.ListPersonalSkillsSouth(gatewayCtxForWorkflow(), req)
	if err != nil {
		t.Fatalf("group- user must yield empty, not error: %v", err)
	}
	if len(resp.Skills) != 0 {
		t.Fatalf("want no skills for a group- user, got %+v", resp.Skills)
	}
}

// ── GetPersonalSkillSouth ────────────────────────────────────────────────────

func TestGetPersonalSkillSouth_RequiresGatewayPeer(t *testing.T) {
	deps := personalSkillsDeps(t, "", t.TempDir())
	svc := NewSandboxService(deps)
	req := &brokerv1.GetPersonalSkillSouthRequest{TenantId: testTenantUUID, UserId: pskUser, Name: "demo"}
	if _, err := svc.GetPersonalSkillSouth(context.Background(), req); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied without a peer identity, got %v", err)
	}
}

// TestGetPersonalSkillSouth_EmptyRequiredFieldsReturnsInvalidArgument pins the
// malformed-request posture: empty tenant_id/user_id/name is a caller bug
// (InvalidArgument), distinct from an ungranted/FGA-error/missing-skill
// NotFound — matching ListPersonalSkillsSouth's existing sibling behavior.
func TestGetPersonalSkillSouth_EmptyRequiredFieldsReturnsInvalidArgument(t *testing.T) {
	deps := personalSkillsDeps(t, "", t.TempDir())
	svc := NewSandboxService(deps)

	cases := []*brokerv1.GetPersonalSkillSouthRequest{
		{TenantId: "", UserId: pskUser, Name: "demo"},
		{TenantId: testTenantUUID, UserId: "", Name: "demo"},
		{TenantId: testTenantUUID, UserId: pskUser, Name: ""},
	}
	for _, req := range cases {
		if _, err := svc.GetPersonalSkillSouth(gatewayCtxForWorkflow(), req); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("req=%+v: want InvalidArgument for an empty required field, got %v", req, err)
		}
	}
}

func TestGetPersonalSkillSouth_NoSkillGrantReturnsNotFound(t *testing.T) {
	root := t.TempDir()
	deps := personalSkillsDeps(t, memoryFGA(t, &fakeFGA{}), root)
	svc := NewSandboxService(deps)
	writeSkillMd(t, deps.Workspace.Local, testTenantUUID, pskUser, "demo", demoSkillMd)

	req := &brokerv1.GetPersonalSkillSouthRequest{TenantId: testTenantUUID, UserId: pskUser, Name: "demo"}
	if _, err := svc.GetPersonalSkillSouth(gatewayCtxForWorkflow(), req); status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound without a grant, got %v", err)
	}
}

func TestGetPersonalSkillSouth_FGAErrorReturnsNotFound(t *testing.T) {
	root := t.TempDir()
	deps := personalSkillsDeps(t, memoryFGA(t, &fakeFGA{checkErr: true}), root)
	svc := NewSandboxService(deps)
	writeSkillMd(t, deps.Workspace.Local, testTenantUUID, pskUser, "demo", demoSkillMd)

	req := &brokerv1.GetPersonalSkillSouthRequest{TenantId: testTenantUUID, UserId: pskUser, Name: "demo"}
	if _, err := svc.GetPersonalSkillSouth(gatewayCtxForWorkflow(), req); status.Code(err) != codes.NotFound {
		t.Fatalf("an FGA outage must fail closed to NotFound, not a transport error: %v", err)
	}
}

func TestGetPersonalSkillSouth_ValidSkillReturnsBodyAndAllowedTools(t *testing.T) {
	root := t.TempDir()
	deps := personalSkillsDeps(t, pskGrantedFGA(t), root)
	svc := NewSandboxService(deps)
	writeSkillMd(t, deps.Workspace.Local, testTenantUUID, pskUser, "demo", demoSkillMd)

	req := &brokerv1.GetPersonalSkillSouthRequest{TenantId: testTenantUUID, UserId: pskUser, Name: "demo"}
	resp, err := svc.GetPersonalSkillSouth(gatewayCtxForWorkflow(), req)
	if err != nil {
		t.Fatalf("GetPersonalSkillSouth: %v", err)
	}
	if resp.Body != "# Demo body\n" {
		t.Fatalf("body = %q, want the markdown after the frontmatter fence", resp.Body)
	}
	if len(resp.AllowedTools) != 1 || resp.AllowedTools[0] != "web.fetch" {
		t.Fatalf("allowed_tools = %v", resp.AllowedTools)
	}
}

func TestGetPersonalSkillSouth_MissingSkillReturnsNotFound(t *testing.T) {
	root := t.TempDir()
	deps := personalSkillsDeps(t, pskGrantedFGA(t), root)
	svc := NewSandboxService(deps)

	req := &brokerv1.GetPersonalSkillSouthRequest{TenantId: testTenantUUID, UserId: pskUser, Name: "nope"}
	if _, err := svc.GetPersonalSkillSouth(gatewayCtxForWorkflow(), req); status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound for a missing skill, got %v", err)
	}
}

// TestGetPersonalSkillSouth_InvalidSkillReturnsNotFound: a SKILL.md missing
// its required description is excluded from the catalog (buildEntry) — Get
// must refuse it too, not just List.
func TestGetPersonalSkillSouth_InvalidSkillReturnsNotFound(t *testing.T) {
	root := t.TempDir()
	deps := personalSkillsDeps(t, pskGrantedFGA(t), root)
	svc := NewSandboxService(deps)
	writeSkillMd(t, deps.Workspace.Local, testTenantUUID, pskUser, "broken", "---\nname: broken\n---\n\nbody\n")

	req := &brokerv1.GetPersonalSkillSouthRequest{TenantId: testTenantUUID, UserId: pskUser, Name: "broken"}
	if _, err := svc.GetPersonalSkillSouth(gatewayCtxForWorkflow(), req); status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound for a skill missing its description, got %v", err)
	}
}

func TestGetPersonalSkillSouth_SvcPrefixedUserReturnsNotFound(t *testing.T) {
	root := t.TempDir()
	deps := personalSkillsDeps(t, pskGrantedFGA(t), root)
	svc := NewSandboxService(deps)

	req := &brokerv1.GetPersonalSkillSouthRequest{TenantId: testTenantUUID, UserId: "svc-" + pskUser, Name: "demo"}
	if _, err := svc.GetPersonalSkillSouth(gatewayCtxForWorkflow(), req); status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound for a svc- user, got %v", err)
	}
}

func TestGetPersonalSkillSouth_GroupPrefixedUserReturnsNotFound(t *testing.T) {
	root := t.TempDir()
	deps := personalSkillsDeps(t, pskGrantedFGA(t), root)
	svc := NewSandboxService(deps)

	req := &brokerv1.GetPersonalSkillSouthRequest{TenantId: testTenantUUID, UserId: "group-" + pskUser, Name: "demo"}
	if _, err := svc.GetPersonalSkillSouth(gatewayCtxForWorkflow(), req); status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound for a group- user, got %v", err)
	}
}

// TestGetPersonalSkillSouth_FilePathsPopulated pins :
// the runtime manifest lists every file under Skills/<name>/ except SKILL.md,
// sorted ascending.
func TestGetPersonalSkillSouth_FilePathsPopulated(t *testing.T) {
	root := t.TempDir()
	deps := personalSkillsDeps(t, pskGrantedFGA(t), root)
	svc := NewSandboxService(deps)
	writeSkillMd(t, deps.Workspace.Local, testTenantUUID, pskUser, "demo", demoSkillMd)
	if _, err := deps.Workspace.Local.Write(context.Background(), testTenantUUID, pskUser, "Skills/demo/scripts/run.py", []byte("print()")); err != nil {
		t.Fatalf("write extra file: %v", err)
	}
	if _, err := deps.Workspace.Local.Write(context.Background(), testTenantUUID, pskUser, "Skills/demo/references/notes.md", []byte("# notes")); err != nil {
		t.Fatalf("write extra file: %v", err)
	}

	req := &brokerv1.GetPersonalSkillSouthRequest{TenantId: testTenantUUID, UserId: pskUser, Name: "demo"}
	resp, err := svc.GetPersonalSkillSouth(gatewayCtxForWorkflow(), req)
	if err != nil {
		t.Fatalf("GetPersonalSkillSouth: %v", err)
	}
	want := []string{"references/notes.md", "scripts/run.py"}
	if len(resp.FilePaths) != len(want) {
		t.Fatalf("file_paths = %v, want %v", resp.FilePaths, want)
	}
	for i := range want {
		if resp.FilePaths[i] != want[i] {
			t.Fatalf("file_paths = %v, want %v (sorted ascending)", resp.FilePaths, want)
		}
	}
}

// ── GetPersonalSkillFileSouth ────────────────────────────────────────────────

func TestGetPersonalSkillFileSouth_RequiresGatewayPeer(t *testing.T) {
	deps := personalSkillsDeps(t, "", t.TempDir())
	svc := NewSandboxService(deps)
	req := &brokerv1.GetPersonalSkillFileSouthRequest{TenantId: testTenantUUID, UserId: pskUser, Name: "demo", Path: "references/notes.md"}
	if _, err := svc.GetPersonalSkillFileSouth(context.Background(), req); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied without a peer identity, got %v", err)
	}
}

func TestGetPersonalSkillFileSouth_EmptyRequiredFieldsReturnsInvalidArgument(t *testing.T) {
	deps := personalSkillsDeps(t, "", t.TempDir())
	svc := NewSandboxService(deps)

	cases := []*brokerv1.GetPersonalSkillFileSouthRequest{
		{TenantId: "", UserId: pskUser, Name: "demo", Path: "a.txt"},
		{TenantId: testTenantUUID, UserId: "", Name: "demo", Path: "a.txt"},
		{TenantId: testTenantUUID, UserId: pskUser, Name: "", Path: "a.txt"},
		{TenantId: testTenantUUID, UserId: pskUser, Name: "demo", Path: ""},
	}
	for _, req := range cases {
		if _, err := svc.GetPersonalSkillFileSouth(gatewayCtxForWorkflow(), req); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("req=%+v: want InvalidArgument for an empty required field, got %v", req, err)
		}
	}
}

func TestGetPersonalSkillFileSouth_NoSkillGrantReturnsNotFound(t *testing.T) {
	root := t.TempDir()
	deps := personalSkillsDeps(t, memoryFGA(t, &fakeFGA{}), root)
	svc := NewSandboxService(deps)
	writeSkillMd(t, deps.Workspace.Local, testTenantUUID, pskUser, "demo", demoSkillMd)
	if _, err := deps.Workspace.Local.Write(context.Background(), testTenantUUID, pskUser, "Skills/demo/references/notes.md", []byte("hi")); err != nil {
		t.Fatalf("write extra file: %v", err)
	}

	req := &brokerv1.GetPersonalSkillFileSouthRequest{TenantId: testTenantUUID, UserId: pskUser, Name: "demo", Path: "references/notes.md"}
	if _, err := svc.GetPersonalSkillFileSouth(gatewayCtxForWorkflow(), req); status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound without a grant, got %v", err)
	}
}

func TestGetPersonalSkillFileSouth_ValidFileReturnsContent(t *testing.T) {
	root := t.TempDir()
	deps := personalSkillsDeps(t, pskGrantedFGA(t), root)
	svc := NewSandboxService(deps)
	writeSkillMd(t, deps.Workspace.Local, testTenantUUID, pskUser, "demo", demoSkillMd)
	if _, err := deps.Workspace.Local.Write(context.Background(), testTenantUUID, pskUser, "Skills/demo/references/notes.md", []byte("hello")); err != nil {
		t.Fatalf("write extra file: %v", err)
	}

	req := &brokerv1.GetPersonalSkillFileSouthRequest{TenantId: testTenantUUID, UserId: pskUser, Name: "demo", Path: "references/notes.md"}
	resp, err := svc.GetPersonalSkillFileSouth(gatewayCtxForWorkflow(), req)
	if err != nil {
		t.Fatalf("GetPersonalSkillFileSouth: %v", err)
	}
	if string(resp.Content) != "hello" {
		t.Fatalf("content = %q, want %q", resp.Content, "hello")
	}
}

func TestGetPersonalSkillFileSouth_MissingFileReturnsNotFound(t *testing.T) {
	root := t.TempDir()
	deps := personalSkillsDeps(t, pskGrantedFGA(t), root)
	svc := NewSandboxService(deps)
	writeSkillMd(t, deps.Workspace.Local, testTenantUUID, pskUser, "demo", demoSkillMd)

	req := &brokerv1.GetPersonalSkillFileSouthRequest{TenantId: testTenantUUID, UserId: pskUser, Name: "demo", Path: "references/missing.md"}
	if _, err := svc.GetPersonalSkillFileSouth(gatewayCtxForWorkflow(), req); status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound for a missing file, got %v", err)
	}
}

func TestGetPersonalSkillFileSouth_MissingSkillReturnsNotFound(t *testing.T) {
	root := t.TempDir()
	deps := personalSkillsDeps(t, pskGrantedFGA(t), root)
	svc := NewSandboxService(deps)

	req := &brokerv1.GetPersonalSkillFileSouthRequest{TenantId: testTenantUUID, UserId: pskUser, Name: "nope", Path: "a.txt"}
	if _, err := svc.GetPersonalSkillFileSouth(gatewayCtxForWorkflow(), req); status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound for a missing skill, got %v", err)
	}
}

func TestGetPersonalSkillFileSouth_TraversalPathReturnsNotFound(t *testing.T) {
	root := t.TempDir()
	deps := personalSkillsDeps(t, pskGrantedFGA(t), root)
	svc := NewSandboxService(deps)
	writeSkillMd(t, deps.Workspace.Local, testTenantUUID, pskUser, "demo", demoSkillMd)

	req := &brokerv1.GetPersonalSkillFileSouthRequest{TenantId: testTenantUUID, UserId: pskUser, Name: "demo", Path: "../../etc/passwd"}
	if _, err := svc.GetPersonalSkillFileSouth(gatewayCtxForWorkflow(), req); status.Code(err) != codes.NotFound {
		t.Fatalf("traversal path: want NotFound, got %v", err)
	}
}

func TestGetPersonalSkillFileSouth_OversizeFileReturnsInvalidArgument(t *testing.T) {
	root := t.TempDir()
	deps := personalSkillsDeps(t, pskGrantedFGA(t), root)
	svc := NewSandboxService(deps)
	writeSkillMd(t, deps.Workspace.Local, testTenantUUID, pskUser, "demo", demoSkillMd)
	big := make([]byte, personalskill.MaxSkillMdBytes+1)
	if _, err := deps.Workspace.Local.Write(context.Background(), testTenantUUID, pskUser, "Skills/demo/references/big.bin", big); err != nil {
		t.Fatalf("write big file: %v", err)
	}

	req := &brokerv1.GetPersonalSkillFileSouthRequest{TenantId: testTenantUUID, UserId: pskUser, Name: "demo", Path: "references/big.bin"}
	if _, err := svc.GetPersonalSkillFileSouth(gatewayCtxForWorkflow(), req); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("oversize file: want InvalidArgument, got %v", err)
	}
}

func TestGetPersonalSkillFileSouth_SvcPrefixedUserReturnsNotFound(t *testing.T) {
	root := t.TempDir()
	deps := personalSkillsDeps(t, pskGrantedFGA(t), root)
	svc := NewSandboxService(deps)
	writeSkillMd(t, deps.Workspace.Local, testTenantUUID, pskUser, "demo", demoSkillMd)

	req := &brokerv1.GetPersonalSkillFileSouthRequest{TenantId: testTenantUUID, UserId: "svc-" + pskUser, Name: "demo", Path: "references/notes.md"}
	if _, err := svc.GetPersonalSkillFileSouth(gatewayCtxForWorkflow(), req); status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound for a svc- user, got %v", err)
	}
}

func TestGetPersonalSkillFileSouth_GroupPrefixedUserReturnsNotFound(t *testing.T) {
	root := t.TempDir()
	deps := personalSkillsDeps(t, pskGrantedFGA(t), root)
	svc := NewSandboxService(deps)
	writeSkillMd(t, deps.Workspace.Local, testTenantUUID, pskUser, "demo", demoSkillMd)

	req := &brokerv1.GetPersonalSkillFileSouthRequest{TenantId: testTenantUUID, UserId: "group-" + pskUser, Name: "demo", Path: "references/notes.md"}
	if _, err := svc.GetPersonalSkillFileSouth(gatewayCtxForWorkflow(), req); status.Code(err) != codes.NotFound {
		t.Fatalf("want NotFound for a group- user, got %v", err)
	}
}
