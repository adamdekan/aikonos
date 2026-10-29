package broker

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/apikey"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/revocation"
	"github.com/adamdekan/aikonos/broker/internal/toolproxy"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// fakeRevStore is an in-memory revocation.Store.
type fakeRevStore struct {
	blocks  map[string]db.UserBlock
	userNB  map[string]time.Time
	agentNB map[string]time.Time
	agentSt map[string]string
}

func newFakeRevStore() *fakeRevStore {
	return &fakeRevStore{blocks: map[string]db.UserBlock{}, userNB: map[string]time.Time{}, agentNB: map[string]time.Time{}, agentSt: map[string]string{}}
}

func (f *fakeRevStore) UserState(_ context.Context, _, subject string) (bool, time.Time, error) {
	_, blocked := f.blocks[subject]
	return blocked, f.userNB[subject], nil
}
func (f *fakeRevStore) AgentState(_ context.Context, _, agentID string) (string, time.Time, error) {
	st := f.agentSt[agentID]
	if st == "" {
		st = db.AgentActive // agents in these fakes predate AG-03: active
	}
	return st, f.agentNB[agentID], nil
}
func (f *fakeRevStore) SetAgentStatus(_ context.Context, _, agentID, from, to, _ string) error {
	if cur, _, _ := f.AgentState(context.Background(), "", agentID); cur != from {
		return db.ErrAgentNotFound
	}
	f.agentSt[agentID] = to
	return nil
}
func (f *fakeRevStore) BlockUser(_ context.Context, _, subject, reason, by string) (bool, error) {
	if _, ok := f.blocks[subject]; ok {
		return false, nil
	}
	f.blocks[subject] = db.UserBlock{Reason: reason, By: by, At: time.Now()}
	return true, nil
}
func (f *fakeRevStore) UnblockUser(_ context.Context, _, subject, _ string) (bool, error) {
	_, ok := f.blocks[subject]
	delete(f.blocks, subject)
	return ok, nil
}
func (f *fakeRevStore) UserBlocks(context.Context, string) (map[string]db.UserBlock, error) {
	return f.blocks, nil
}
func (f *fakeRevStore) RevokeUserSessions(_ context.Context, _, subject string, at time.Time) error {
	f.userNB[subject] = at
	return nil
}
func (f *fakeRevStore) RevokeAgentSessions(_ context.Context, _, agentID string, at time.Time) error {
	if agentID != apiKeyTestAgentUUID {
		return db.ErrAgentNotFound
	}
	f.agentNB[agentID] = at
	return nil
}

func findEvent(events []*auditv1.AuditEvent, eventType string) *auditv1.AuditEvent {
	for _, e := range events {
		if e.EventType == eventType {
			return e
		}
	}
	return nil
}

// AG-06: block and unblock are admin-only, need a reason, refuse a self-block,
// and emit audit events with the acting admin and the reason.
func TestSetUserBlocked_AuditsActorAndReason(t *testing.T) {
	const admin = "admin@example.com"
	deps := newApiKeyDeps(t, admin, &fakeApiKeyStore{}, agentStoreWithAgent(t))
	aud := &fakeInvokeAudit{}
	deps.Audit = aud
	store := newFakeRevStore()
	deps.Revocation = revocation.New(store)
	svc := NewBrokerService(deps)
	ctx := ctxWithIdentity(apiKeyTestTenantUUID, admin)
	req := func(blocked bool, subject, reason string) *brokerv1.SetUserBlockedRequest {
		return &brokerv1.SetUserBlockedRequest{TenantId: apiKeyTestTenantUUID, Subject: subject, Blocked: blocked, Reason: reason}
	}

	if _, err := svc.SetUserBlocked(ctx, req(true, "bob", " ")); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("no reason: want InvalidArgument, got %v", err)
	}
	if _, err := svc.SetUserBlocked(ctx, req(true, admin, "oops")); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("self-block: want InvalidArgument, got %v", err)
	}
	if _, err := svc.SetUserBlocked(ctxWithIdentity(apiKeyTestTenantUUID, "eve@example.com"), req(true, "bob", "x")); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin: want PermissionDenied, got %v", err)
	}
	if len(store.blocks) != 0 {
		t.Fatalf("refused calls must not block: %v", store.blocks)
	}

	resp, err := svc.SetUserBlocked(ctx, req(true, "bob", "left the company"))
	if err != nil || !resp.Changed {
		t.Fatalf("block: %v %v", resp, err)
	}
	if b := store.blocks["bob"]; b.Reason != "left the company" || b.By != admin {
		t.Fatalf("stored block = %+v", b)
	}
	if _, err := svc.SetUserBlocked(ctx, req(false, "bob", "back from leave")); err != nil {
		t.Fatalf("unblock: %v", err)
	}
	for ev, reason := range map[string]string{
		"aikonos.broker.user.blocked":   "left the company",
		"aikonos.broker.user.unblocked": "back from leave",
	} {
		e := findEvent(aud.events, ev)
		if e == nil {
			t.Fatalf("no %s event", ev)
		}
		if e.ActorUserId != admin || e.ResourceRef != "user:bob" || e.Context.Fields["reason"].GetStringValue() != reason {
			t.Errorf("%s: actor=%q resource=%q ctx=%v", ev, e.ActorUserId, e.ResourceRef, e.Context)
		}
	}
}

// Revoke-all on an agent and a user moves the epoch and is audited.
func TestRevokeSessions_MovesEpochAndAudits(t *testing.T) {
	const admin = "admin@example.com"
	deps := newApiKeyDeps(t, admin, &fakeApiKeyStore{}, agentStoreWithAgent(t))
	aud := &fakeInvokeAudit{}
	deps.Audit = aud
	store := newFakeRevStore()
	deps.Revocation = revocation.New(store)
	svc := NewBrokerService(deps)
	ctx := ctxWithIdentity(apiKeyTestTenantUUID, admin)

	before := time.Now()
	if _, err := svc.RevokeAgentSessions(ctx, &brokerv1.RevokeAgentSessionsRequest{TenantId: apiKeyTestTenantUUID, AgentId: apiKeyTestAgentUUID, Reason: "key leaked"}); err != nil {
		t.Fatalf("revoke agent: %v", err)
	}
	if nb := store.agentNB[apiKeyTestAgentUUID]; nb.Before(before) {
		t.Fatalf("agent epoch %v not moved", nb)
	}
	if _, err := svc.RevokeAgentSessions(ctx, &brokerv1.RevokeAgentSessionsRequest{TenantId: apiKeyTestTenantUUID, AgentId: uuid.NewString(), Reason: "x"}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown agent: want NotFound, got %v", err)
	}
	if _, err := svc.RevokeUserSessions(ctx, &brokerv1.RevokeUserSessionsRequest{TenantId: apiKeyTestTenantUUID, Subject: "bob", Reason: "laptop stolen"}); err != nil {
		t.Fatalf("revoke user: %v", err)
	}
	for ev, reason := range map[string]string{
		"aikonos.broker.agent.sessions_revoked": "key leaked",
		"aikonos.broker.user.sessions_revoked":  "laptop stolen",
	} {
		e := findEvent(aud.events, ev)
		if e == nil || e.ActorUserId != admin || e.Context.Fields["reason"].GetStringValue() != reason || e.Context.Fields["not_before"].GetStringValue() == "" {
			t.Errorf("%s: %v", ev, e)
		}
	}
}

// AG-06 acceptance: a blocked user's HMAC owner grant is rejected at the
// gateway (CreateGatewayTask), and a revoke-all voids grants issued before it.
func TestCreateGatewayTask_RevokedOrBlockedGrantRefused(t *testing.T) {
	store := &fakeCreateStore{}
	svc := newSandboxSvcForGatewayTest(t, store, nil)
	aud := &fakeInvokeAudit{}
	svc.deps.Audit = aud
	rev := newFakeRevStore()
	svc.deps.Revocation = revocation.New(rev)
	create := func(grant string) error {
		_, err := svc.CreateGatewayTask(gatewaySpiffeCtx(), &brokerv1.CreateGatewayTaskRequest{TenantId: testTenantUUID, Prompt: "p", OwnerGrant: grant})
		return err
	}

	bobGrant := mintTestGrant(t, testTenantUUID, "bob")
	if err := create(bobGrant); err != nil {
		t.Fatalf("unblocked: %v", err)
	}
	if _, err := svc.deps.Revocation.BlockUser(context.Background(), testTenantUUID, "bob", "r", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := create(bobGrant); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("blocked subject: want PermissionDenied, got %v", err)
	}

	aliceGrant := mintTestGrant(t, testTenantUUID, "alice")
	if _, err := svc.deps.Revocation.RevokeUser(context.Background(), testTenantUUID, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := create(aliceGrant); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("grant issued before revoke-all: want PermissionDenied, got %v", err)
	}
	// Grants are second-precision; one minted in a later second is valid.
	rev.userNB["alice"] = time.Now().Add(-2 * time.Second)
	svc.deps.Revocation = revocation.New(rev)
	if err := create(mintTestGrant(t, testTenantUUID, "alice")); err != nil {
		t.Fatalf("grant after the epoch: %v", err)
	}

	got := map[string]bool{}
	for _, e := range aud.events {
		if e.EventType == "aikonos.broker.task.denied" {
			got[e.Context.Fields["reason"].GetStringValue()] = true
		}
	}
	if !got["subject_blocked"] || !got["grant_revoked"] {
		t.Errorf("denial reasons = %v, want subject_blocked and grant_revoked", got)
	}
	if len(store.created) != 2 {
		t.Errorf("created %d tasks, want 2", len(store.created))
	}
}

// TestCreateGatewayTask_RevokeSurvivesTTLChange pins F244: the grant carries
// its issue time, so lowering the grant TTL after minting does not move the
// grant's apparent issue past a later revoke-all.
func TestCreateGatewayTask_RevokeSurvivesTTLChange(t *testing.T) {
	svc := newSandboxSvcForGatewayTest(t, &fakeCreateStore{}, nil)
	rev := newFakeRevStore()
	svc.deps.Revocation = revocation.New(rev)
	svc.deps.GatewayGrantTTL = time.Hour
	grant := mintTestGrant(t, testTenantUUID, "alice")

	rev.userNB["alice"] = time.Now().Add(2 * time.Second) // revoke-all after the mint
	svc.deps.GatewayGrantTTL = time.Minute                // restart with a tighter TTL
	_, err := svc.CreateGatewayTask(gatewaySpiffeCtx(), &brokerv1.CreateGatewayTaskRequest{TenantId: testTenantUUID, Prompt: "p", OwnerGrant: grant})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("grant minted before the revoke-all: want PermissionDenied, got %v", err)
	}
}

// AG-06: InvokeTool stops a blocked subject at the next tool call and refuses a
// capability token issued before the actor's revoke-all, without waiting for
// the token to expire.
func TestInvokeTool_BlockAndRevocationEpoch(t *testing.T) {
	agentID := uuid.New()
	for _, tc := range []struct {
		name   string
		set    func(*fakeRevStore)
		reason string
	}{
		{"none", func(*fakeRevStore) {}, ""},
		{"subject blocked", func(f *fakeRevStore) { f.blocks["Y"] = db.UserBlock{} }, "subject_blocked"},
		{"actor revoked", func(f *fakeRevStore) { f.agentNB[agentID.String()] = time.Now().Add(time.Second) }, "capability_revoked"},
		{"subject revoked", func(f *fakeRevStore) { f.userNB["Y"] = time.Now().Add(time.Second) }, "capability_revoked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got toolproxy.Request
			tasks := &fakeInvokeToolTaskStore{task: &db.Task{State: db.TaskStateExecuting, CostBudget: 100, OwnerUserID: "Y", AgentID: &agentID}}
			svc, m, aud := newMemorySvc(t, "memory.write", tasks, &got)
			rev := newFakeRevStore()
			tc.set(rev)
			svc.deps.Revocation = revocation.New(rev)
			raw, err := m.Mint("sb", "task-1", db.OnBehalfOf{Subject: "Y", Actor: agentID.String()}, []string{"memory:write"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
				TaskId: "task-1", ToolId: "memory.write", CapabilityToken: base64.StdEncoding.EncodeToString(raw),
				Args: memoryArgs(t, map[string]any{"scope": "user", "id": "c1"}),
			})
			if tc.reason == "" {
				if err != nil || got.ToolID == "" {
					t.Fatalf("want the tool to run, got err=%v", err)
				}
				return
			}
			if status.Code(err) != codes.PermissionDenied || got.ToolID != "" {
				t.Fatalf("want PermissionDenied and no run, got err=%v ran=%v", err, got.ToolID != "")
			}
			for _, e := range aud.events {
				if e.EventType == "aikonos.broker.tool.denied" && e.Context.Fields["reason"].GetStringValue() == tc.reason {
					return
				}
			}
			t.Fatalf("no tool.denied event with reason %s", tc.reason)
		})
	}
}

// AG-06: an agent API key minted before the agent's revoke-all no longer
// resolves; a key minted afterwards does.
func TestResolveAgentApiKey_RevokedAgentKeyInvalid(t *testing.T) {
	const admin = "admin@example.com"
	deps := newApiKeyDeps(t, admin, &fakeApiKeyStore{}, agentStoreWithAgent(t))
	rev := newFakeRevStore()
	deps.Revocation = revocation.New(rev)
	north, south := NewBrokerService(deps), NewSandboxService(deps)
	ctx := ctxWithIdentity(apiKeyTestTenantUUID, admin)
	mint := func() string {
		resp, err := north.MintAgentApiKey(ctx, &brokerv1.MintAgentApiKeyRequest{TenantId: apiKeyTestTenantUUID, AgentId: apiKeyTestAgentUUID, TtlDays: 30})
		if err != nil {
			t.Fatal(err)
		}
		return apikey.HashForResolve(resp.RawKey)
	}
	resolve := func(hash string) bool {
		resp, err := south.ResolveAgentApiKey(sandboxCtx(), &brokerv1.ResolveAgentApiKeyRequest{KeyHashHex: hash, TenantId: apiKeyTestTenantUUID})
		if err != nil {
			t.Fatal(err)
		}
		return resp.Valid
	}

	old := mint()
	if !resolve(old) {
		t.Fatal("key must resolve before the revoke-all")
	}
	if _, err := north.RevokeAgentSessions(ctx, &brokerv1.RevokeAgentSessionsRequest{TenantId: apiKeyTestTenantUUID, AgentId: apiKeyTestAgentUUID, Reason: "compromised"}); err != nil {
		t.Fatal(err)
	}
	if resolve(old) {
		t.Fatal("a key minted before the revoke-all must not resolve")
	}
	if fresh := mint(); !resolve(fresh) {
		t.Fatal("a key minted after the revoke-all must resolve")
	}
}
