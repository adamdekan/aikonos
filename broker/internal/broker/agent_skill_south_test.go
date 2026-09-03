package broker

// agent_skill_south_test.go — unit tests for ListUserAgentSkills (south-bound, SPIFFE-gated).
//
// Contract (spec CP6):
//   - No SPIFFE peer → PermissionDenied
//   - Missing tenant_id → InvalidArgument
//   - Missing user_id → InvalidArgument
//   - FGA disabled → all tenant bundles returned (including disable_model_invocation=true)
//   - FGA enabled → only bundles where CheckFGA(user, can_use, agentskill:<id>) passes
//   - FGA enabled + CheckFGA error → bundle excluded, no error surfaced (fail-closed per-item)
//   - disable_model_invocation flag propagated correctly in response

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ── fake skill-gate policy for agentskill (reuses fakeSkillPolicy from skill_gate_test.go) ──
// fakeSkillPolicy already satisfies skillGatePolicy with configurable
// enabled/granted/err/lastUser/lastRelation/lastObject fields — we reuse it
// directly. The relation checked here is "can_use" (not "can_invoke"), so the
// test asserts lastRelation == "can_use".

// ── helper ────────────────────────────────────────────────────────────────────

// newAgentSkillSouthSvc builds a SandboxService with:
//   - nop audit emitter
//   - AgentSkillBundles wired to the provided fakeAgentSkillRepo
//   - skillPolicy set on the service (test-only override, consistent with
//     newSvcWithSkillPolicy)
func newAgentSkillSouthSvc(t *testing.T, repo agentSkillRepo, sp skillGatePolicy) *SandboxService {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	svc := NewSandboxService(Deps{
		Logger:            zap.NewNop(),
		Audit:             em,
		GatewaySpiffeID:   testGateway,
		TenantID:          testTenant,
		AgentSkillBundles: repo,
	})
	svc.skillPolicy = sp
	return svc
}

// seedBundle inserts a bundle row into the repo and returns its id string.
func seedBundle(t *testing.T, repo *fakeAgentSkillRepo, name string, dmi bool) string {
	t.Helper()
	id := uuid.New()
	tools, _ := json.Marshal([]string{"web.fetch"})
	repo.rows[id.String()] = &db.AgentSkill{
		ID:                     id,
		TenantID:               uuid.MustParse(testTenant),
		Name:                   name,
		Description:            "desc-" + name,
		Body:                   "body-" + name,
		AllowedTools:           tools,
		DisableModelInvocation: dmi,
	}
	repo.byName[name] = id.String()
	return id.String()
}

// seedBundleWithKeywords inserts a bundle row carrying pre-normalized keywords.
func seedBundleWithKeywords(t *testing.T, repo *fakeAgentSkillRepo, name string, keywords []string) string {
	t.Helper()
	id := uuid.New()
	kw, _ := json.Marshal(keywords)
	repo.rows[id.String()] = &db.AgentSkill{
		ID:       id,
		TenantID: uuid.MustParse(testTenant),
		Name:     name,
		Keywords: kw,
	}
	repo.byName[name] = id.String()
	return id.String()
}

// ── keywords propagated through the south list RPC ────────────────────────────

func TestListUserAgentSkills_Keywords_Propagated(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	seedBundleWithKeywords(t, repo, "kw-skill", []string{"invoice", "refund"})
	sp := &fakeSkillPolicy{enabled: false}
	svc := newAgentSkillSouthSvc(t, repo, sp)
	ctx := gatewayCtx(testGateway)

	resp, err := svc.ListUserAgentSkills(ctx, &brokerv1.ListUserAgentSkillsRequest{
		TenantId: testTenant,
		UserId:   "alice",
	})
	if err != nil {
		t.Fatalf("ListUserAgentSkills: %v", err)
	}
	if len(resp.Bundles) != 1 {
		t.Fatalf("want 1 bundle, got %d", len(resp.Bundles))
	}
	got := resp.Bundles[0].Keywords
	want := []string{"invoice", "refund"}
	if len(got) != len(want) {
		t.Fatalf("keywords: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("keywords: got %v, want %v", got, want)
		}
	}
}

// ── SPIFFE gate ───────────────────────────────────────────────────────────────

func TestListUserAgentSkills_NoPeer_PermissionDenied(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	svc := newAgentSkillSouthSvc(t, repo, &fakeSkillPolicy{enabled: false})
	_, err := svc.ListUserAgentSkills(context.Background(), &brokerv1.ListUserAgentSkillsRequest{
		TenantId: testTenant,
		UserId:   "alice",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("no peer: want PermissionDenied, got %v", err)
	}
}

// ── validation ────────────────────────────────────────────────────────────────

func TestListUserAgentSkills_EmptyTenant_InvalidArgument(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	svc := newAgentSkillSouthSvc(t, repo, &fakeSkillPolicy{enabled: false})
	ctx := gatewayCtx(testGateway)
	_, err := svc.ListUserAgentSkills(ctx, &brokerv1.ListUserAgentSkillsRequest{UserId: "alice"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty tenant: want InvalidArgument, got %v", err)
	}
}

func TestListUserAgentSkills_EmptyUser_InvalidArgument(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	svc := newAgentSkillSouthSvc(t, repo, &fakeSkillPolicy{enabled: false})
	ctx := gatewayCtx(testGateway)
	_, err := svc.ListUserAgentSkills(ctx, &brokerv1.ListUserAgentSkillsRequest{TenantId: testTenant})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty user: want InvalidArgument, got %v", err)
	}
}

// ── FGA disabled: all tenant bundles returned ─────────────────────────────────

func TestListUserAgentSkills_FGADisabled_ReturnsAllBundles(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	seedBundle(t, repo, "skill-a", false)
	seedBundle(t, repo, "skill-b", true) // disable_model_invocation=true still returned
	sp := &fakeSkillPolicy{enabled: false}
	svc := newAgentSkillSouthSvc(t, repo, sp)
	ctx := gatewayCtx(testGateway)

	resp, err := svc.ListUserAgentSkills(ctx, &brokerv1.ListUserAgentSkillsRequest{
		TenantId: testTenant,
		UserId:   "alice",
	})
	if err != nil {
		t.Fatalf("ListUserAgentSkills: %v", err)
	}
	if len(resp.Bundles) != 2 {
		t.Fatalf("FGA disabled: want 2 bundles, got %d", len(resp.Bundles))
	}
	// CheckFGA must never be called when FGA is disabled.
	if sp.checkFGACalls != 0 {
		t.Errorf("FGA disabled: CheckFGA called %d times, want 0", sp.checkFGACalls)
	}
}

// ── FGA enabled: only granted bundles returned ────────────────────────────────

func TestListUserAgentSkills_FGAEnabled_GrantedBundleIncluded(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	seedBundle(t, repo, "skill-a", false)
	sp := &fakeSkillPolicy{enabled: true, granted: true}
	svc := newAgentSkillSouthSvc(t, repo, sp)
	ctx := gatewayCtx(testGateway)

	resp, err := svc.ListUserAgentSkills(ctx, &brokerv1.ListUserAgentSkillsRequest{
		TenantId: testTenant,
		UserId:   "alice",
	})
	if err != nil {
		t.Fatalf("ListUserAgentSkills: %v", err)
	}
	if len(resp.Bundles) != 1 {
		t.Fatalf("FGA enabled+granted: want 1 bundle, got %d", len(resp.Bundles))
	}
	if sp.checkFGACalls == 0 {
		t.Error("FGA enabled: CheckFGA was never called")
	}
	if sp.lastUser != "user:alice" {
		t.Errorf("lastUser: want %q, got %q", "user:alice", sp.lastUser)
	}
	if sp.lastRelation != "can_use" {
		t.Errorf("lastRelation: want %q, got %q", "can_use", sp.lastRelation)
	}
	if !strings.HasPrefix(sp.lastObject, "agentskill:") {
		t.Errorf("lastObject: want agentskill:<id> prefix, got %q", sp.lastObject)
	}
}

func TestListUserAgentSkills_FGAEnabled_DeniedBundleExcluded(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	seedBundle(t, repo, "skill-a", false)
	sp := &fakeSkillPolicy{enabled: true, granted: false}
	svc := newAgentSkillSouthSvc(t, repo, sp)
	ctx := gatewayCtx(testGateway)

	resp, err := svc.ListUserAgentSkills(ctx, &brokerv1.ListUserAgentSkillsRequest{
		TenantId: testTenant,
		UserId:   "alice",
	})
	if err != nil {
		t.Fatalf("ListUserAgentSkills: %v", err)
	}
	if len(resp.Bundles) != 0 {
		t.Errorf("all denied: want 0 bundles, got %d", len(resp.Bundles))
	}
}

func TestListUserAgentSkills_FGAEnabled_CheckFGAError_BundleExcluded(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	seedBundle(t, repo, "skill-a", false)
	sp := &fakeSkillPolicy{enabled: true, err: context.DeadlineExceeded}
	svc := newAgentSkillSouthSvc(t, repo, sp)
	ctx := gatewayCtx(testGateway)

	resp, err := svc.ListUserAgentSkills(ctx, &brokerv1.ListUserAgentSkillsRequest{
		TenantId: testTenant,
		UserId:   "alice",
	})
	if err != nil {
		t.Fatalf("CheckFGA error must not surface: %v", err)
	}
	if len(resp.Bundles) != 0 {
		t.Errorf("CheckFGA error should exclude bundle: got %d", len(resp.Bundles))
	}
}

// ── file_paths propagated through the south list RPC ──────────────────────────

func TestListUserAgentSkills_FilePaths_Populated(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	id := seedBundle(t, repo, "tree-skill", false)
	repo.mu.Lock()
	repo.rows[id].Extras = []byte(`{"scripts/run.py":"cA==","references/notes.md":"bg=="}`)
	repo.mu.Unlock()
	sp := &fakeSkillPolicy{enabled: false}
	svc := newAgentSkillSouthSvc(t, repo, sp)
	ctx := gatewayCtx(testGateway)

	resp, err := svc.ListUserAgentSkills(ctx, &brokerv1.ListUserAgentSkillsRequest{
		TenantId: testTenant,
		UserId:   "alice",
	})
	if err != nil {
		t.Fatalf("ListUserAgentSkills: %v", err)
	}
	if len(resp.Bundles) != 1 {
		t.Fatalf("want 1 bundle, got %d", len(resp.Bundles))
	}
	want := []string{"references/notes.md", "scripts/run.py"}
	got := resp.Bundles[0].FilePaths
	if len(got) != len(want) {
		t.Fatalf("file_paths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("file_paths = %v, want %v (sorted ascending)", got, want)
		}
	}
}

// ── GetAgentSkillFileSouth ──────────────────────────────────────────────────

func seedBundleWithExtras(t *testing.T, repo *fakeAgentSkillRepo, name string, extras map[string]string) string {
	t.Helper()
	id := seedBundle(t, repo, name, false)
	raw, err := json.Marshal(extras)
	if err != nil {
		t.Fatal(err)
	}
	repo.mu.Lock()
	repo.rows[id].Extras = raw
	repo.mu.Unlock()
	return id
}

func TestGetAgentSkillFileSouth_RequiresGatewayPeer(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	svc := newAgentSkillSouthSvc(t, repo, &fakeSkillPolicy{enabled: false})
	_, err := svc.GetAgentSkillFileSouth(context.Background(), &brokerv1.GetAgentSkillFileSouthRequest{
		TenantId: testTenant, UserId: "alice", Id: uuid.New().String(), Path: "a.txt",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("no peer: want PermissionDenied, got %v", err)
	}
}

func TestGetAgentSkillFileSouth_EmptyFields_InvalidArgument(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	svc := newAgentSkillSouthSvc(t, repo, &fakeSkillPolicy{enabled: false})
	ctx := gatewayCtx(testGateway)
	id := uuid.New().String()

	cases := []*brokerv1.GetAgentSkillFileSouthRequest{
		{TenantId: "", UserId: "alice", Id: id, Path: "a.txt"},
		{TenantId: testTenant, UserId: "", Id: id, Path: "a.txt"},
		{TenantId: testTenant, UserId: "alice", Id: "", Path: "a.txt"},
		{TenantId: testTenant, UserId: "alice", Id: id, Path: ""},
	}
	for _, req := range cases {
		if _, err := svc.GetAgentSkillFileSouth(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("req=%+v: want InvalidArgument, got %v", req, err)
		}
	}
}

func TestGetAgentSkillFileSouth_UnknownId_NotFound(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	svc := newAgentSkillSouthSvc(t, repo, &fakeSkillPolicy{enabled: false})
	ctx := gatewayCtx(testGateway)

	_, err := svc.GetAgentSkillFileSouth(ctx, &brokerv1.GetAgentSkillFileSouthRequest{
		TenantId: testTenant, UserId: "alice", Id: uuid.New().String(), Path: "a.txt",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("unknown id: want NotFound, got %v", err)
	}
}

func TestGetAgentSkillFileSouth_UnknownPath_NotFound(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	id := seedBundleWithExtras(t, repo, "skill-a", map[string]string{"a.txt": "aGk="})
	svc := newAgentSkillSouthSvc(t, repo, &fakeSkillPolicy{enabled: false})
	ctx := gatewayCtx(testGateway)

	_, err := svc.GetAgentSkillFileSouth(ctx, &brokerv1.GetAgentSkillFileSouthRequest{
		TenantId: testTenant, UserId: "alice", Id: id, Path: "missing.txt",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("unknown path: want NotFound, got %v", err)
	}
}

func TestGetAgentSkillFileSouth_InvalidPath_NotFound(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	id := seedBundleWithExtras(t, repo, "skill-a", map[string]string{"a.txt": "aGk="})
	svc := newAgentSkillSouthSvc(t, repo, &fakeSkillPolicy{enabled: false})
	ctx := gatewayCtx(testGateway)

	_, err := svc.GetAgentSkillFileSouth(ctx, &brokerv1.GetAgentSkillFileSouthRequest{
		TenantId: testTenant, UserId: "alice", Id: id, Path: "../escape.txt",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("traversal path: want NotFound, got %v", err)
	}
}

func TestGetAgentSkillFileSouth_FGADisabled_ReturnsContent(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	id := seedBundleWithExtras(t, repo, "skill-a", map[string]string{"a.txt": "aGk="}) // base64("hi")
	svc := newAgentSkillSouthSvc(t, repo, &fakeSkillPolicy{enabled: false})
	ctx := gatewayCtx(testGateway)

	resp, err := svc.GetAgentSkillFileSouth(ctx, &brokerv1.GetAgentSkillFileSouthRequest{
		TenantId: testTenant, UserId: "alice", Id: id, Path: "a.txt",
	})
	if err != nil {
		t.Fatalf("GetAgentSkillFileSouth: %v", err)
	}
	if string(resp.Content) != "hi" {
		t.Fatalf("content = %q, want %q", resp.Content, "hi")
	}
}

func TestGetAgentSkillFileSouth_FGAEnabledGranted_ReturnsContent(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	id := seedBundleWithExtras(t, repo, "skill-a", map[string]string{"a.txt": "aGk="})
	sp := &fakeSkillPolicy{enabled: true, granted: true}
	svc := newAgentSkillSouthSvc(t, repo, sp)
	ctx := gatewayCtx(testGateway)

	resp, err := svc.GetAgentSkillFileSouth(ctx, &brokerv1.GetAgentSkillFileSouthRequest{
		TenantId: testTenant, UserId: "alice", Id: id, Path: "a.txt",
	})
	if err != nil {
		t.Fatalf("GetAgentSkillFileSouth: %v", err)
	}
	if string(resp.Content) != "hi" {
		t.Fatalf("content = %q, want %q", resp.Content, "hi")
	}
	if sp.lastRelation != "can_use" {
		t.Errorf("lastRelation: want can_use, got %q", sp.lastRelation)
	}
	if !strings.HasPrefix(sp.lastObject, "agentskill:") {
		t.Errorf("lastObject: want agentskill:<id> prefix, got %q", sp.lastObject)
	}
}

func TestGetAgentSkillFileSouth_FGAEnabledDenied_NotFound(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	id := seedBundleWithExtras(t, repo, "skill-a", map[string]string{"a.txt": "aGk="})
	svc := newAgentSkillSouthSvc(t, repo, &fakeSkillPolicy{enabled: true, granted: false})
	ctx := gatewayCtx(testGateway)

	_, err := svc.GetAgentSkillFileSouth(ctx, &brokerv1.GetAgentSkillFileSouthRequest{
		TenantId: testTenant, UserId: "alice", Id: id, Path: "a.txt",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("denied: want NotFound, got %v", err)
	}
}

func TestGetAgentSkillFileSouth_FGAEnabledError_NotFound(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	id := seedBundleWithExtras(t, repo, "skill-a", map[string]string{"a.txt": "aGk="})
	svc := newAgentSkillSouthSvc(t, repo, &fakeSkillPolicy{enabled: true, err: context.DeadlineExceeded})
	ctx := gatewayCtx(testGateway)

	_, err := svc.GetAgentSkillFileSouth(ctx, &brokerv1.GetAgentSkillFileSouthRequest{
		TenantId: testTenant, UserId: "alice", Id: id, Path: "a.txt",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("CheckFGA error: want NotFound (fail-closed), got %v", err)
	}
}

// ── disable_model_invocation flag propagated ──────────────────────────────────

func TestListUserAgentSkills_DisableModelInvocation_Propagated(t *testing.T) {
	repo := newFakeAgentSkillRepo()
	seedBundle(t, repo, "skill-visible", false)
	seedBundle(t, repo, "skill-no-model", true)
	sp := &fakeSkillPolicy{enabled: false} // FGA off → all returned
	svc := newAgentSkillSouthSvc(t, repo, sp)
	ctx := gatewayCtx(testGateway)

	resp, err := svc.ListUserAgentSkills(ctx, &brokerv1.ListUserAgentSkillsRequest{
		TenantId: testTenant,
		UserId:   "alice",
	})
	if err != nil {
		t.Fatalf("ListUserAgentSkills: %v", err)
	}
	if len(resp.Bundles) != 2 {
		t.Fatalf("want 2 bundles, got %d", len(resp.Bundles))
	}
	var gotDMI bool
	for _, b := range resp.Bundles {
		if b.Name == "skill-no-model" {
			gotDMI = b.DisableModelInvocation
		}
	}
	if !gotDMI {
		t.Error("disable_model_invocation not propagated: want true for skill-no-model")
	}
}
