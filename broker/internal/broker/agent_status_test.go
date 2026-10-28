package broker

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/apikey"
	"github.com/adamdekan/aikonos/broker/internal/auth"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/gatewaygrant"
	"github.com/adamdekan/aikonos/broker/internal/revocation"
	"github.com/adamdekan/aikonos/broker/internal/toolproxy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// AG-03 tests: agent lifecycle and the kill switch.

// rowRevStore is a fakeRevStore whose agent status lives on the agent rows,
// as agents.status does.
type rowRevStore struct {
	*fakeRevStore
	agents *fakeAgentStore
}

func (s rowRevStore) AgentState(ctx context.Context, tenant, id string) (string, time.Time, error) {
	a, err := s.agents.Get(ctx, tenant, id)
	if err != nil {
		return "", time.Time{}, nil
	}
	return a.Status, s.agentNB[id], nil
}

func (s rowRevStore) SetAgentStatus(ctx context.Context, tenant, id, from, to, reason string) error {
	a, err := s.agents.Get(ctx, tenant, id)
	if err != nil || a.Status != from {
		return db.ErrAgentNotFound
	}
	a.Status, a.StatusReason = to, reason
	return nil
}

// Lifecycle: new agents start staged; activation needs an owner who is not
// blocked; active <-> suspended; retired is final and not for the tenant
// default agent; admin only; every move audited with its reason.
func TestSetAgentStatus_Lifecycle(t *testing.T) {
	const admin = "admin@example.com"
	store := &fakeAgentStore{}
	def, _, _ := store.EnsureDefault(context.Background(), agentTestTenantUUID)
	def.Status = db.AgentActive
	obj := "agent:" + agentAID
	f := &fakeFGA{admins: map[string]bool{"user:" + admin: true}, reads: map[string][]fgaKey{}}
	deps := newListMyAgentsDeps(t, f, store)
	aud := &fakeInvokeAudit{}
	deps.Audit = aud
	rev := newFakeRevStore()
	rev.blocks["bob"] = db.UserBlock{Reason: "left"}
	deps.Revocation = revocation.New(rowRevStore{rev, store})
	svc := NewBrokerService(deps)
	ctx := ctxWithIdentity(agentTestTenantUUID, admin)
	set := func(id, to, reason string) (*brokerv1.Agent, error) {
		resp, err := svc.SetAgentStatus(ctx, &brokerv1.SetAgentStatusRequest{TenantId: agentTestTenantUUID, AgentId: id, Status: to, Reason: reason})
		return resp.GetAgent(), err
	}

	created, err := svc.CreateAgent(ctx, &brokerv1.CreateAgentRequest{TenantId: agentTestTenantUUID, Agent: &brokerv1.Agent{Name: "new"}})
	if err != nil || created.Agent.Status != db.AgentStaged {
		t.Fatalf("CreateAgent: status %q (%v), want staged", created.GetAgent().GetStatus(), err)
	}
	a := makeTestAgent(t, agentTestTenantUUID, agentAID, "a")
	a.Status = db.AgentStaged
	store.agents = append(store.agents, a)

	if _, err := set(agentAID, db.AgentActive, ""); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("no owner: want FailedPrecondition, got %v", err)
	}
	f.reads[obj] = []fgaKey{{User: "user:bob", Relation: "owner_user", Object: obj}}
	if _, err := set(agentAID, db.AgentActive, ""); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("only a blocked owner: want FailedPrecondition, got %v", err)
	}
	f.reads[obj] = append(f.reads[obj], fgaKey{User: "user:carol", Relation: "owner_user", Object: obj})
	if got, err := set(agentAID, db.AgentActive, ""); err != nil || got.Status != db.AgentActive {
		t.Fatalf("activate: %v %v", got, err)
	}
	if _, err := set(agentAID, db.AgentSuspended, " "); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("suspend without reason: want InvalidArgument, got %v", err)
	}
	if _, err := svc.SetAgentStatus(ctxWithIdentity(agentTestTenantUUID, "eve"), &brokerv1.SetAgentStatusRequest{TenantId: agentTestTenantUUID, AgentId: agentAID, Status: db.AgentSuspended, Reason: "x"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin: want PermissionDenied, got %v", err)
	}
	if got, err := set(agentAID, db.AgentSuspended, "incident 42"); err != nil || got.Status != db.AgentSuspended || got.StatusReason != "incident 42" {
		t.Fatalf("suspend: %v %v", got, err)
	}
	if st, _ := deps.Revocation.Agent(context.Background(), agentTestTenantUUID, agentAID); st.AgentRefusal() != "agent_suspended" {
		t.Fatalf("checker after suspend: %q", st.AgentRefusal())
	}
	if _, err := set(agentAID, db.AgentSuspended, "again"); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("suspend twice: want FailedPrecondition, got %v", err)
	}
	if _, err := set(agentAID, db.AgentActive, ""); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	if _, err := set(agentAID, db.AgentRetired, "replaced"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if _, err := set(agentAID, db.AgentActive, ""); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("retired is final: want FailedPrecondition, got %v", err)
	}
	if _, err := set(agentAID, db.AgentStaged, ""); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("staged is no target: want InvalidArgument, got %v", err)
	}

	// The tenant default agent has no owner: it may be suspended and
	// reactivated, never retired.
	if _, err := set(def.ID.String(), db.AgentRetired, "x"); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("retire default: want FailedPrecondition, got %v", err)
	}
	if _, err := set(def.ID.String(), db.AgentSuspended, "kill switch"); err != nil {
		t.Fatalf("suspend default: %v", err)
	}
	if _, err := set(def.ID.String(), db.AgentActive, ""); err != nil {
		t.Fatalf("reactivate default: %v", err)
	}

	for ev, reason := range map[string]string{
		"aikonos.broker.agent.activated":   "",
		"aikonos.broker.agent.suspended":   "incident 42",
		"aikonos.broker.agent.reactivated": "",
		"aikonos.broker.agent.retired":     "replaced",
	} {
		e := findEvent(aud.events, ev)
		if e == nil || e.ActorUserId != admin || e.ResourceRef != obj || e.Context.Fields["reason"].GetStringValue() != reason {
			t.Errorf("%s: %v", ev, e)
		}
	}
}

// Acceptance: suspending an agent makes the next InvokeTool of its running
// task fail with PermissionDenied (reason agent_suspended), with the same
// still-valid capability token that worked a moment before.
func TestInvokeTool_SuspendedAgentStopsAtNextCall(t *testing.T) {
	agentID := uuid.New()
	var got toolproxy.Request
	tasks := &fakeInvokeToolTaskStore{task: &db.Task{State: db.TaskStateExecuting, CostBudget: 100, OwnerUserID: "Y", AgentID: &agentID}}
	svc, m, aud := newMemorySvc(t, "memory.write", tasks, &got)
	svc.deps.Revocation = revocation.New(newFakeRevStore())
	raw, err := m.Mint("sb", "task-1", db.OnBehalfOf{Subject: "Y", Actor: agentID.String()}, []string{"memory:write"})
	if err != nil {
		t.Fatal(err)
	}
	invoke := func() error {
		got = toolproxy.Request{}
		_, err := svc.InvokeTool(context.Background(), &brokerv1.InvokeToolRequest{
			TaskId: "task-1", ToolId: "memory.write", CapabilityToken: base64.StdEncoding.EncodeToString(raw),
			Args: memoryArgs(t, map[string]any{"scope": "user", "id": "c1"}),
		})
		return err
	}
	if err := invoke(); err != nil || got.ToolID == "" {
		t.Fatalf("active agent: want the tool to run, got %v", err)
	}
	if err := svc.deps.Revocation.SetAgentStatus(context.Background(), testTenantUUID, agentID.String(), db.AgentActive, db.AgentSuspended, "incident"); err != nil {
		t.Fatal(err)
	}
	if err := invoke(); status.Code(err) != codes.PermissionDenied || got.ToolID != "" {
		t.Fatalf("suspended: want PermissionDenied and no run, got %v ran=%v", err, got.ToolID != "")
	}
	e := findEvent(aud.events, "aikonos.broker.tool.denied")
	if e == nil || e.Context.Fields["reason"].GetStringValue() != "agent_suspended" {
		t.Fatalf("tool.denied = %v, want reason agent_suspended", e)
	}
}

// Acceptance: CreateGatewayTask (grant actor and default binding), north
// CreateTask, ResolveRunAgent, SubmitPlan and ResolveAgentApiKey refuse every
// status other than active.
func TestAgentStatus_RunPathsRefuseInactive(t *testing.T) {
	for _, st := range []string{db.AgentStaged, db.AgentSuspended, db.AgentRetired} {
		t.Run(st, func(t *testing.T) {
			deps, _, fga, def, named := defaultAgentFixture(t, nil)
			fga.checks = map[string]bool{canUse("alice", def): true, canUse("alice", named): true}
			store := &fakeCreateStore{}
			deps.Tasks = store
			rev := newFakeRevStore()
			rev.agentSt[def.String()] = st
			rev.agentSt[named.String()] = st
			deps.Revocation = revocation.New(rev)
			south, north := NewSandboxService(deps), NewBrokerService(deps)

			_, err := north.CreateTask(ctxWithIdentity(testTenantUUID, "alice"), &brokerv1.CreateTaskRequest{TenantId: testTenantUUID, Prompt: "p"})
			if status.Code(err) != codes.PermissionDenied {
				t.Errorf("north CreateTask: want PermissionDenied, got %v", err)
			}
			create := func(grant gatewaygrant.Grant) error {
				g, err := gatewaygrant.Mint(testGrantKey, grant, testGrantTTL)
				if err != nil {
					t.Fatal(err)
				}
				_, err = south.CreateGatewayTask(gatewaySpiffeCtx(), &brokerv1.CreateGatewayTaskRequest{TenantId: testTenantUUID, Prompt: "p", OwnerGrant: g})
				return err
			}
			if err := create(gatewaygrant.Grant{TenantID: testTenantUUID, Subject: "alice"}); status.Code(err) != codes.PermissionDenied {
				t.Errorf("CreateGatewayTask (default agent): want PermissionDenied, got %v", err)
			}
			if err := create(gatewaygrant.Grant{TenantID: testTenantUUID, Subject: "alice", Actor: named.String(), Kind: gatewaygrant.KindInteractive}); status.Code(err) != codes.PermissionDenied {
				t.Errorf("CreateGatewayTask (grant actor): want PermissionDenied, got %v", err)
			}
			if _, err := south.ResolveRunAgent(gatewaySpiffeCtx(), &brokerv1.ResolveRunAgentRequest{TenantId: testTenantUUID, UserId: "alice", AgentId: named.String()}); status.Code(err) != codes.PermissionDenied {
				t.Errorf("ResolveRunAgent: want PermissionDenied, got %v", err)
			}
			if len(store.created) != 0 {
				t.Errorf("created %d tasks, want none", len(store.created))
			}

			plan := newSvcWithAgent(t, []string{"doc.read"}, agentBoundTask(), allowPol())
			prev := newFakeRevStore()
			prev.agentSt[testAgentIDStr] = st
			plan.deps.Revocation = revocation.New(prev)
			_, err = plan.SubmitPlan(context.Background(), &brokerv1.SubmitPlanRequest{
				TaskId: validTaskUUID(),
				Plan:   makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "doc.read", nil)}),
			})
			if status.Code(err) != codes.PermissionDenied {
				t.Errorf("SubmitPlan: want PermissionDenied, got %v", err)
			}

			const admin = "admin@example.com"
			kdeps := newApiKeyDeps(t, admin, &fakeApiKeyStore{}, agentStoreWithAgent(t))
			krev := newFakeRevStore()
			kdeps.Revocation = revocation.New(krev)
			minted, err := NewBrokerService(kdeps).MintAgentApiKey(ctxWithIdentity(apiKeyTestTenantUUID, admin), &brokerv1.MintAgentApiKeyRequest{TenantId: apiKeyTestTenantUUID, AgentId: apiKeyTestAgentUUID, TtlDays: 30})
			if err != nil {
				t.Fatal(err)
			}
			krev.agentSt[apiKeyTestAgentUUID] = st
			_, err = NewSandboxService(kdeps).ResolveAgentApiKey(sandboxCtx(), &brokerv1.ResolveAgentApiKeyRequest{KeyHashHex: apikey.HashForResolve(minted.RawKey), TenantId: apiKeyTestTenantUUID})
			if status.Code(err) != codes.PermissionDenied {
				t.Errorf("ResolveAgentApiKey: want PermissionDenied, got %v", err)
			}
		})
	}
}

// A default-agent setting whose agent is suspended falls back to the tenant
// default agent rather than refusing the chat.
func TestResolveRunAgent_SuspendedSettingFallsBack(t *testing.T) {
	deps, agents, fga, def, named := defaultAgentFixture(t, nil)
	fga.checks = map[string]bool{canUse("alice", def): true, canUse("alice", named): true}
	agents.userDefaults = map[string]string{"alice": named.String()}
	rev := newFakeRevStore()
	rev.agentSt[named.String()] = db.AgentSuspended
	deps.Revocation = revocation.New(rev)
	r, err := NewSandboxService(deps).ResolveRunAgent(gatewaySpiffeCtx(), &brokerv1.ResolveRunAgentRequest{TenantId: testTenantUUID, UserId: "alice"})
	if err != nil || r.AgentId != def.String() {
		t.Fatalf("got %v %v, want the tenant default agent", r, err)
	}
}

// A scheduled fire of a suspended actor is skipped and audited; the row is
// kept (ClaimDue parks only retired actors) so reactivation resumes it.
func TestClaimDue_SuspendedActorSkipped(t *testing.T) {
	agents := &fakeAgentStore{}
	def, _, _ := agents.EnsureDefault(context.Background(), testTenantUUID)
	actor := uuid.New()
	rev := newFakeRevStore()
	rev.agentSt[def.ID.String()] = db.AgentSuspended
	rev.agentSt[actor.String()] = db.AgentSuspended
	aud := &fakeInvokeAudit{}
	svc := NewSandboxService(Deps{
		Logger: zap.NewNop(), Audit: aud, GatewaySpiffeID: testGWSpiffeID,
		GatewayGrantKey: testGrantKey, GatewayGrantTTL: testGrantTTL, Agents: agents,
		Revocation: revocation.New(rev),
		Scheduled: &fakeClaimDueStore{runs: []*db.ScheduledRun{
			{ScheduledRunID: uuid.New(), OwnerUserID: "alice"},
			{ScheduledRunID: uuid.New(), OwnerUserID: "alice", ActorAgentID: &actor},
		}},
	})
	resp, err := svc.ClaimDueScheduledRuns(
		auth.WithIdentity(context.Background(), &auth.Identity{SpiffeID: testGWSpiffeID}),
		&brokerv1.ClaimDueScheduledRunsRequest{TenantId: testTenantUUID},
	)
	if err != nil || len(resp.Runs) != 0 {
		t.Fatalf("ClaimDueScheduledRuns: %v %v, want no runs", resp, err)
	}
	n := 0
	for _, e := range aud.events {
		if e.EventType == "aikonos.scheduler.run.skipped.agent_suspended" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("skipped.agent_suspended events = %d, want 2", n)
	}
}

// The orphan sweep suspends an active agent whose owners have all been
// blocked for longer than identity.orphan_grace_days (default 7), and leaves
// a recent orphan, an owned agent, a staged agent and the tenant default alone.
func TestSweepOrphanedAgents(t *testing.T) {
	const (
		oldOrphan   = "aaaaaaaa-0000-0000-0000-000000000031"
		freshOrphan = "aaaaaaaa-0000-0000-0000-000000000032"
		owned       = "aaaaaaaa-0000-0000-0000-000000000033"
		staged      = "aaaaaaaa-0000-0000-0000-000000000034"
	)
	now := time.Now()
	store := &fakeAgentStore{}
	def, _, _ := store.EnsureDefault(context.Background(), agentTestTenantUUID)
	def.Status = db.AgentActive
	f := &fakeFGA{reads: map[string][]fgaKey{}}
	for id, st := range map[string]string{oldOrphan: db.AgentActive, freshOrphan: db.AgentActive, owned: db.AgentActive, staged: db.AgentStaged} {
		a := makeTestAgent(t, agentTestTenantUUID, id, id)
		a.Status = st
		store.agents = append(store.agents, a)
	}
	owner := func(id, user string) fgaKey { return fgaKey{User: user, Relation: "owner_user", Object: "agent:" + id} }
	f.reads["agent:"+oldOrphan] = []fgaKey{owner(oldOrphan, "user:bob")}
	f.reads["agent:"+freshOrphan] = []fgaKey{owner(freshOrphan, "user:bob"), owner(freshOrphan, "user:dan")}
	f.reads["agent:"+owned] = []fgaKey{owner(owned, "user:bob"), owner(owned, "user:carol")}
	f.reads["agent:"+staged] = []fgaKey{owner(staged, "user:bob")}
	deps := newListMyAgentsDeps(t, f, store)
	aud := &fakeInvokeAudit{}
	deps.Audit = aud
	rev := newFakeRevStore()
	rev.blocks["bob"] = db.UserBlock{At: now.Add(-8 * 24 * time.Hour)}
	rev.blocks["dan"] = db.UserBlock{At: now.Add(-2 * 24 * time.Hour)}
	deps.Revocation = revocation.New(rowRevStore{rev, store})
	svc := NewBrokerService(deps)

	n, err := svc.SweepOrphanedAgents(context.Background(), agentTestTenantUUID, now)
	if err != nil || n != 1 {
		t.Fatalf("sweep: n=%d err=%v, want 1", n, err)
	}
	for _, a := range store.agents {
		want := db.AgentActive
		switch a.ID.String() {
		case oldOrphan:
			want = db.AgentSuspended
		case staged:
			want = db.AgentStaged
		}
		if a.Status != want {
			t.Errorf("agent %s status %q, want %q", a.ID, a.Status, want)
		}
	}
	e := findEvent(aud.events, "aikonos.broker.agent.suspended")
	if e == nil || e.ActorUserId != orphanSweepActor || e.ResourceRef != "agent:"+oldOrphan {
		t.Fatalf("suspended event = %v", e)
	}
	// Idempotent: a second sweep finds nothing more to do.
	if n, err := svc.SweepOrphanedAgents(context.Background(), agentTestTenantUUID, now); err != nil || n != 0 {
		t.Fatalf("second sweep: n=%d err=%v", n, err)
	}
}
