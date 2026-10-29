package broker

// south_grant_test.go — F171 / F5 / G46 / F129: south RPCs that act for a user
// take that user from the session's owner grant, never from the gateway.

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/capability"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/gatewaygrant"
	"github.com/adamdekan/aikonos/broker/internal/revocation"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

func newGrantSouthSvc(aud *capturingAudit) *SandboxService {
	return NewSandboxService(Deps{
		Logger: zap.NewNop(), Audit: aud, GatewaySpiffeID: testGWSpiffeID,
		GatewayGrantKey: testGrantKey, GatewayGrantTTL: testGrantTTL,
	})
}

// deniedReasons returns the reason of every aikonos.broker.task.denied event.
func deniedReasons(aud *capturingAudit) []string {
	var out []string
	for _, e := range aud.ofType("aikonos.broker.task.denied") {
		out = append(out, e.GetContext().GetFields()["reason"].GetStringValue())
	}
	return out
}

// A valid grant for alice cannot read bob's skills, memory, skill files or
// MCP tools: the request's user_id must be the grant's owner.
func TestSouthReads_GrantForOtherUserDenied(t *testing.T) {
	aud := &capturingAudit{}
	svc := newGrantSouthSvc(aud)
	ctx := gatewaySpiffeCtx()
	g := mintTestGrant(t, testTenantUUID, "alice")
	calls := map[string]func() error{
		"ListUserSkills": func() error {
			_, err := svc.ListUserSkills(ctx, &brokerv1.ListUserSkillsRequest{TenantId: testTenantUUID, UserId: "bob", OwnerGrant: g})
			return err
		},
		"ListUserAgentSkills": func() error {
			_, err := svc.ListUserAgentSkills(ctx, &brokerv1.ListUserAgentSkillsRequest{TenantId: testTenantUUID, UserId: "bob", OwnerGrant: g})
			return err
		},
		"GetAgentSkillFileSouth": func() error {
			_, err := svc.GetAgentSkillFileSouth(ctx, &brokerv1.GetAgentSkillFileSouthRequest{TenantId: testTenantUUID, UserId: "bob", Id: uuid.NewString(), Path: "a.txt", OwnerGrant: g})
			return err
		},
		"ListPersonalSkillsSouth": func() error {
			_, err := svc.ListPersonalSkillsSouth(ctx, &brokerv1.ListPersonalSkillsSouthRequest{TenantId: testTenantUUID, UserId: "bob", OwnerGrant: g})
			return err
		},
		"GetPersonalSkillSouth": func() error {
			_, err := svc.GetPersonalSkillSouth(ctx, &brokerv1.GetPersonalSkillSouthRequest{TenantId: testTenantUUID, UserId: "bob", Name: "demo", OwnerGrant: g})
			return err
		},
		"GetPersonalSkillFileSouth": func() error {
			_, err := svc.GetPersonalSkillFileSouth(ctx, &brokerv1.GetPersonalSkillFileSouthRequest{TenantId: testTenantUUID, UserId: "bob", Name: "demo", Path: "a.txt", OwnerGrant: g})
			return err
		},
		"ListMemoryConceptsSouth": func() error {
			_, err := svc.ListMemoryConceptsSouth(ctx, &brokerv1.ListMemoryConceptsRequest{TenantId: testTenantUUID, UserId: "bob", OwnerGrant: g})
			return err
		},
		"ListMcpServerToolsSouth": func() error {
			_, err := svc.ListMcpServerToolsSouth(ctx, &brokerv1.ListMcpServerToolsRequest{TenantId: testTenantUUID, UserId: "bob", ConnectorId: uuid.NewString(), AgentId: uuid.NewString(), OwnerGrant: g})
			return err
		},
	}
	for name, call := range calls {
		if err := call(); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s with alice's grant for bob: want PermissionDenied, got %v", name, err)
		}
	}
	reasons := deniedReasons(aud)
	if len(reasons) != len(calls) {
		t.Fatalf("want one audited denial per call, got %v", reasons)
	}
	for _, r := range reasons {
		if r != "grant_user_mismatch" {
			t.Fatalf("reason = %q, want grant_user_mismatch", r)
		}
	}
}

// No grant: refused (and audited) outside dev mode; dev mode — the recovery
// path — falls back to the request's ids but still refuses a blocked user.
func TestSouthReads_MissingGrant(t *testing.T) {
	aud := &capturingAudit{}
	svc := newGrantSouthSvc(aud)
	ctx := gatewaySpiffeCtx()
	req := &brokerv1.ListUserSkillsRequest{TenantId: testTenantUUID, UserId: "alice"}

	if _, err := svc.ListUserSkills(ctx, req); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("no grant outside dev mode: want PermissionDenied, got %v", err)
	}
	if r := deniedReasons(aud); len(r) != 1 || r[0] != "grant_missing" {
		t.Fatalf("missing grant must be audited grant_missing, got %v", r)
	}

	svc.deps.DevMode = true
	resp, err := svc.ListUserSkills(ctx, req)
	if err != nil || len(resp.Skills) == 0 {
		t.Fatalf("no grant in dev mode: want the user's skills, got %v err=%v", resp, err)
	}

	rev := newFakeRevStore()
	rev.blocks["alice"] = db.UserBlock{}
	svc.deps.Revocation = revocation.New(rev)
	_, err = svc.ListUserSkills(ctx, req)
	if status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != "user is blocked" {
		t.Fatalf("blocked user in dev mode: want PermissionDenied %q, got %v", "user is blocked", err)
	}
}

// F129: the memory recall list refuses a blocked grant owner with a detail the
// gateway reads as access revoked (it ends in "user is blocked").
func TestListMemoryConceptsSouth_BlockedOwnerRefused(t *testing.T) {
	aud := &capturingAudit{}
	svc := newGrantSouthSvc(aud)
	rev := newFakeRevStore()
	rev.blocks["alice"] = db.UserBlock{}
	svc.deps.Revocation = revocation.New(rev)

	_, err := svc.ListMemoryConceptsSouth(gatewaySpiffeCtx(), &brokerv1.ListMemoryConceptsRequest{
		TenantId: testTenantUUID, UserId: "alice", OwnerGrant: mintTestGrant(t, testTenantUUID, "alice"),
	})
	if status.Code(err) != codes.PermissionDenied || !strings.HasSuffix(status.Convert(err).Message(), "user is blocked") {
		t.Fatalf("blocked owner: want PermissionDenied ending in %q, got %v", "user is blocked", err)
	}
	if r := deniedReasons(aud); len(r) != 1 || r[0] != "subject_blocked" {
		t.Fatalf("want subject_blocked audited, got %v", r)
	}
}

// F129: a blocked user's run resolves no agent; the detail is exactly the
// north RevocationInterceptor's, which the gateway maps to access revoked.
func TestResolveRunAgent_BlockedUserRefused(t *testing.T) {
	deps, _, fga, def, _ := defaultAgentFixture(t, nil)
	fga.checks = map[string]bool{canUse("alice", def): true}
	aud := &capturingAudit{}
	deps.Audit = aud
	rev := newFakeRevStore()
	rev.blocks["alice"] = db.UserBlock{}
	deps.Revocation = revocation.New(rev)

	_, err := NewSandboxService(deps).ResolveRunAgent(gatewaySpiffeCtx(), &brokerv1.ResolveRunAgentRequest{TenantId: testTenantUUID, UserId: "alice"})
	if status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != "user is blocked" {
		t.Fatalf("blocked user: want PermissionDenied %q, got %v", "user is blocked", err)
	}
	if r := deniedReasons(aud); len(r) != 1 || r[0] != "subject_blocked" {
		t.Fatalf("want subject_blocked audited, got %v", r)
	}
}

func approveFixture(t *testing.T, gatewayManaged bool) (*SandboxService, *capturingAudit, string) {
	t.Helper()
	minter, err := capability.GenerateMinter()
	if err != nil {
		t.Fatal(err)
	}
	taskID := uuid.New()
	store := &fakeGatewayApproveStore{
		created: []*db.Task{{
			TaskID: taskID, TenantID: uuid.MustParse(testTenantUUID), OwnerUserID: "alice@example.com",
			GatewayManaged: gatewayManaged, State: db.TaskStateAwaitingApproval,
		}},
		approval: &db.ApprovalRequest{
			ApprovalID: uuid.New(), TaskID: taskID, TenantID: uuid.MustParse(testTenantUUID),
			RequesterID: "alice@example.com", ApproverSet: []string{"alice@example.com"}, RequiresN: 1,
			State: db.ApprovalPending,
		},
	}
	svc := newSandboxSvcForGatewayTest(t, store, minter)
	aud := &capturingAudit{}
	svc.deps.Audit = aud
	return svc, aud, taskID.String()
}

// F5/G46: the gateway cannot vote on a task created in the web UI (north,
// not gateway-managed), even with the owner's own grant.
func TestApproveGatewayTask_NorthTaskRefused(t *testing.T) {
	svc, aud, taskID := approveFixture(t, false)
	resp, err := svc.ApproveGatewayTask(gatewaySpiffeCtx(), &brokerv1.ApproveGatewayTaskRequest{
		TenantId: testTenantUUID, TaskId: taskID, Approved: true, ArgsDigest: webFetchDigest,
		OwnerGrant: mintTestGrant(t, testTenantUUID, "alice@example.com"),
	})
	if status.Code(err) != codes.FailedPrecondition || resp != nil {
		t.Fatalf("north task: want FailedPrecondition, got resp=%v err=%v", resp, err)
	}
	if r := deniedReasons(aud); len(r) != 1 || r[0] != "gateway_vote_refused" {
		t.Fatalf("want gateway_vote_refused audited, got %v", r)
	}
}

// F5/G46: a grant for another user cannot vote as the task owner, and no
// grant is refused outside dev mode (dev mode is the recovery path).
func TestApproveGatewayTask_GrantMustNameOwner(t *testing.T) {
	svc, aud, taskID := approveFixture(t, true)
	req := &brokerv1.ApproveGatewayTaskRequest{
		TenantId: testTenantUUID, TaskId: taskID, Approved: true, ArgsDigest: webFetchDigest,
		OwnerGrant: mintTestGrant(t, testTenantUUID, "bob@example.com"),
	}
	if _, err := svc.ApproveGatewayTask(gatewaySpiffeCtx(), req); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("bob's grant on alice's task: want PermissionDenied, got %v", err)
	}
	if r := deniedReasons(aud); len(r) != 1 || r[0] != "gateway_vote_refused" {
		t.Fatalf("want gateway_vote_refused audited, got %v", r)
	}

	req.OwnerGrant = ""
	if _, err := svc.ApproveGatewayTask(gatewaySpiffeCtx(), req); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("no grant: want PermissionDenied, got %v", err)
	}
	svc.deps.DevMode = true
	if resp, err := svc.ApproveGatewayTask(gatewaySpiffeCtx(), req); err != nil || resp.NewStatus != brokerv1.TaskStatus_APPROVED {
		t.Fatalf("no grant in dev mode: want APPROVED, got %v err=%v", resp, err)
	}
}

// MintSessionGrant binds the OIDC caller as an interactive grant's subject;
// the grant then serves that caller's own south reads and no one else's.
func TestMintSessionGrant_BindsCaller(t *testing.T) {
	north := NewBrokerService(Deps{Logger: zap.NewNop(), GatewayGrantKey: testGrantKey, GatewayGrantTTL: testGrantTTL})
	resp, err := north.MintSessionGrant(ctxWithIdentity(testTenantUUID, "alice"), &brokerv1.MintSessionGrantRequest{TenantId: testTenantUUID})
	if err != nil {
		t.Fatalf("MintSessionGrant: %v", err)
	}
	g, err := gatewaygrant.Verify(testGrantKey, resp.OwnerGrant, false)
	if err != nil || g.TenantID != testTenantUUID || g.Owner() != "alice" || g.Kind != gatewaygrant.KindInteractive || g.Actor != "" {
		t.Fatalf("grant = %+v err=%v, want an interactive grant for alice", g, err)
	}
	if _, err := north.MintSessionGrant(ctxWithIdentity(testTenantUUID, "alice"), &brokerv1.MintSessionGrantRequest{TenantId: testTenantUUID, UserId: "bob"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("asking for bob's grant as alice: want PermissionDenied, got %v", err)
	}

	south := newGrantSouthSvc(&capturingAudit{})
	if _, err := south.ListUserSkills(gatewaySpiffeCtx(), &brokerv1.ListUserSkillsRequest{TenantId: testTenantUUID, UserId: "alice", OwnerGrant: resp.OwnerGrant}); err != nil {
		t.Fatalf("alice's session grant on her own read: %v", err)
	}
}
