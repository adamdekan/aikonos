package broker

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/auth"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/gatewaygrant"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// AG-01 tests: every task binds a registered agent.

// defaultAgentFixture is a FGA-enabled deps with an agent store holding the
// tenant default agent and one named agent. checks are "user|relation|object".
func defaultAgentFixture(t *testing.T, checks map[string]bool) (Deps, *fakeAgentStore, *fakeFGA, uuid.UUID, uuid.UUID) {
	t.Helper()
	fga := &fakeFGA{checks: checks}
	srv := fga.server(t)
	t.Cleanup(srv.Close)
	eng, err := policy.NewEngine(context.Background(), policy.Config{
		OPAEndpoint: "http://unused", OpenFGAEndpoint: srv.URL, OpenFGAStoreID: "store-1",
	})
	if err != nil {
		t.Fatalf("policy.NewEngine: %v", err)
	}
	agents := &fakeAgentStore{}
	def, _, _ := agents.EnsureDefault(context.Background(), testTenantUUID)
	named := uuid.New()
	agents.agents = append(agents.agents, &db.Agent{ID: named, TenantID: uuid.MustParse(testTenantUUID), Name: "named"})
	deps := newMinimalDeps(t)
	deps.Policy, deps.Agents, deps.TenantID = eng, agents, testTenantUUID
	deps.GatewaySpiffeID = testGWSpiffeID
	deps.GatewayGrantKey, deps.GatewayGrantTTL = testGrantKey, testGrantTTL
	return deps, agents, fga, def.ID, named
}

func canUse(user string, agent uuid.UUID) string {
	return "user:" + user + "|can_use|agent:" + agent.String()
}

// A north task that names no agent binds the caller's default agent; with
// can_use on it withdrawn (an admin narrowed it) the task is refused.
func TestCreateTask_NoAgentID_BindsDefaultAgent(t *testing.T) {
	deps, _, fga, def, _ := defaultAgentFixture(t, nil)
	fga.checks = map[string]bool{canUse("alice", def): true}
	store := &fakeCreateStore{}
	deps.Tasks = store
	svc := NewBrokerService(deps)
	ctx := ctxWithIdentity(testTenantUUID, "alice")

	if _, err := svc.CreateTask(ctx, &brokerv1.CreateTaskRequest{TenantId: testTenantUUID, Prompt: "p"}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if got := store.created[0].AgentID; got == nil || *got != def {
		t.Fatalf("agent = %v, want the default agent %s", got, def)
	}

	fga.checks[canUse("alice", def)] = false
	_, err := svc.CreateTask(ctx, &brokerv1.CreateTaskRequest{TenantId: testTenantUUID, Prompt: "p"})
	if status.Code(err) != codes.PermissionDenied || len(store.created) != 1 {
		t.Fatalf("default agent not usable: want PermissionDenied and no task, got %v", err)
	}
}

// CreateGatewayTask refuses a non-UUID agent_id (no NULL coercion), refuses a
// named agent the grant owner cannot use, and binds the default agent when
// none is named — never a NULL agent_id.
func TestCreateGatewayTask_AgentBinding(t *testing.T) {
	deps, _, fga, def, named := defaultAgentFixture(t, nil)
	fga.checks = map[string]bool{canUse("alice@example.com", def): true}
	store := &fakeCreateStore{}
	deps.Tasks = store
	svc := NewSandboxService(deps)
	grant := mintTestGrant(t, testTenantUUID, "alice@example.com")
	create := func(agentID string) error {
		_, err := svc.CreateGatewayTask(gatewaySpiffeCtx(), &brokerv1.CreateGatewayTaskRequest{
			TenantId: testTenantUUID, Prompt: "p", OwnerGrant: grant, AgentId: agentID,
		})
		return err
	}

	if err := create("alice-agent"); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("pseudo-id: want InvalidArgument, got %v", err)
	}
	if err := create(named.String()); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("named agent without can_use: want PermissionDenied, got %v", err)
	}
	if err := create(""); err != nil {
		t.Fatalf("no agent: %v", err)
	}
	if len(store.created) != 1 || store.created[0].AgentID == nil || *store.created[0].AgentID != def {
		t.Fatalf("tasks = %v, want one bound to the default agent", store.created)
	}
}

// ResolveRunAgent (H-05): a named agent needs can_use; no agent resolves the
// user's default-agent setting while usable, else the tenant default agent.
func TestResolveRunAgent(t *testing.T) {
	deps, agents, fga, def, named := defaultAgentFixture(t, nil)
	fga.checks = map[string]bool{canUse("alice", def): true}
	svc := NewSandboxService(deps)
	resolve := func(agentID string) (*brokerv1.ResolveRunAgentResponse, error) {
		return svc.ResolveRunAgent(gatewaySpiffeCtx(), &brokerv1.ResolveRunAgentRequest{
			TenantId: testTenantUUID, UserId: "alice", AgentId: agentID,
		})
	}

	if _, err := resolve(named.String()); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("named agent without can_use: want PermissionDenied, got %v", err)
	}
	if r, err := resolve(""); err != nil || r.AgentId != def.String() || !r.IsDefault {
		t.Fatalf("no agent: got %v %v, want the default agent", r, err)
	}

	// A setting the user may use wins; once can_use is gone it falls back.
	agents.userDefaults = map[string]string{"alice": named.String()}
	fga.checks[canUse("alice", named)] = true
	if r, err := resolve(""); err != nil || r.AgentId != named.String() || r.IsDefault {
		t.Fatalf("usable setting: got %v %v, want %s", r, err, named)
	}
	if r, err := resolve(named.String()); err != nil || r.AgentId != named.String() {
		t.Fatalf("named agent with can_use: got %v %v", r, err)
	}
	fga.checks[canUse("alice", named)] = false
	if r, err := resolve(""); err != nil || r.AgentId != def.String() {
		t.Fatalf("unusable setting: got %v %v, want the default agent", r, err)
	}

	// Only the gateway may ask.
	if _, err := svc.ResolveRunAgent(context.Background(), &brokerv1.ResolveRunAgentRequest{TenantId: testTenantUUID, UserId: "alice"}); status.Code(err) == codes.OK {
		t.Fatal("non-gateway caller: want an error")
	}
}

// SetMyDefaultAgent requires can_use; the tenant default agent clears it.
func TestSetMyDefaultAgent(t *testing.T) {
	deps, agents, fga, def, named := defaultAgentFixture(t, nil)
	fga.checks = map[string]bool{canUse("alice", def): true}
	svc := NewBrokerService(deps)
	ctx := ctxWithIdentity(testTenantUUID, "alice")
	set := func(agentID string) (*brokerv1.MyDefaultAgent, error) {
		return svc.SetMyDefaultAgent(ctx, &brokerv1.SetMyDefaultAgentRequest{TenantId: testTenantUUID, AgentId: agentID})
	}

	if _, err := set(named.String()); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("without can_use: want PermissionDenied, got %v", err)
	}
	fga.checks[canUse("alice", named)] = true
	if r, err := set(named.String()); err != nil || r.AgentId != named.String() || agents.userDefaults["alice"] != named.String() {
		t.Fatalf("with can_use: got %v %v, stored %v", r, err, agents.userDefaults)
	}
	if r, err := svc.GetMyDefaultAgent(ctx, &brokerv1.GetMyDefaultAgentRequest{TenantId: testTenantUUID}); err != nil || r.AgentId != named.String() {
		t.Fatalf("GetMyDefaultAgent: %v %v", r, err)
	}
	if r, err := set(def.String()); err != nil || r.AgentId != "" || agents.userDefaults["alice"] != "" {
		t.Fatalf("tenant default: want the setting cleared, got %v %v", r, err)
	}
}

// EnsureDefaultAgent creates the agent and grants every user can_use
// (user:* usable_by) once; an admin's narrower grant is kept.
func TestEnsureDefaultAgent_GrantsEveryUser(t *testing.T) {
	deps, _, fga, _, _ := defaultAgentFixture(t, nil)
	agents := &fakeAgentStore{}
	deps.Agents = agents
	svc := NewBrokerService(deps)

	if err := svc.EnsureDefaultAgent(context.Background(), testTenantUUID); err != nil {
		t.Fatalf("EnsureDefaultAgent: %v", err)
	}
	if len(agents.agents) != 1 || !agents.agents[0].IsDefault {
		t.Fatalf("agents = %v, want one default agent", agents.agents)
	}
	obj := "agent:" + agents.agents[0].ID.String()
	if len(fga.writes) != 1 || !writesTuple(fga.writes[0], "user:*", "usable_by", obj) {
		t.Fatalf("writes = %v, want user:* usable_by %s", fga.writes, obj)
	}

	fga.reads = map[string][]fgaKey{obj: {{User: "group:staff#member", Relation: "usable_by", Object: obj}}}
	if err := svc.EnsureDefaultAgent(context.Background(), testTenantUUID); err != nil {
		t.Fatalf("EnsureDefaultAgent again: %v", err)
	}
	if len(agents.agents) != 1 || len(fga.writes) != 1 {
		t.Fatalf("second ensure: %d agents, %d writes, want 1/1", len(agents.agents), len(fga.writes))
	}
}

// writesTuple reports whether a fakeFGA write record is the tuple.
func writesTuple(w map[string]any, user, relation, object string) bool {
	return w["user"] == user && w["relation"] == relation && w["object"] == object
}

// The tenant default agent cannot be deleted and is not listed for picking.
func TestDefaultAgent_NotDeletableNotListed(t *testing.T) {
	deps, _, fga, def, named := defaultAgentFixture(t, nil)
	fga.admins = map[string]bool{"user:admin": true}
	fga.listObjectsResult = []string{"agent:" + def.String(), "agent:" + named.String()}
	svc := NewBrokerService(deps)

	_, err := svc.DeleteAgent(ctxWithIdentity(testTenantUUID, "admin"), &brokerv1.DeleteAgentRequest{TenantId: testTenantUUID, Id: def.String()})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("delete default agent: want FailedPrecondition, got %v", err)
	}
	resp, err := svc.ListMyAgents(ctxWithIdentity(testTenantUUID, "alice"), &brokerv1.ListMyAgentsRequest{TenantId: testTenantUUID})
	if err != nil || len(resp.Agents) != 1 || resp.Agents[0].Id != named.String() {
		t.Fatalf("ListMyAgents = %v %v, want only the named agent", resp, err)
	}
}

// A task bound to the tenant default agent takes the personal skill gate: the
// default agent's own Skills list does not bound it.
func TestSkillBoundary_DefaultAgent_PersonalGate(t *testing.T) {
	svc := newSvcWithAgent(t, []string{"doc.read"}, agentBoundTask(), allowPol())
	svc.deps.Agents.(*fakeAgentStoreForSkill).agent.IsDefault = true

	result, err := svc.SubmitPlan(asGateway(context.Background()), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(),
		Plan:   makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "web.fetch", nil)}),
	})
	if err != nil {
		t.Fatalf("SubmitPlan: %v", err)
	}
	if result.Outcome != planv1.ValidationOutcome_APPROVED {
		t.Fatalf("default agent: want APPROVED (personal gate), got %s %v", result.Outcome, result.Violations)
	}
}

// A schedule with no actor fires as its owner's default agent: the grant and
// the DueRun name it.
func TestClaimDue_ActorlessRunUsesDefaultAgent(t *testing.T) {
	agents := &fakeAgentStore{}
	def, _, _ := agents.EnsureDefault(context.Background(), testTenantUUID)
	svc := NewSandboxService(Deps{
		Logger: zap.NewNop(), Audit: &fakeInvokeAudit{}, GatewaySpiffeID: testGWSpiffeID,
		GatewayGrantKey: testGrantKey, GatewayGrantTTL: testGrantTTL, Agents: agents,
		Scheduled: &fakeClaimDueStore{runs: []*db.ScheduledRun{{ScheduledRunID: uuid.New(), OwnerUserID: "alice"}}},
	})
	resp, err := svc.ClaimDueScheduledRuns(
		auth.WithIdentity(context.Background(), &auth.Identity{SpiffeID: testGWSpiffeID}),
		&brokerv1.ClaimDueScheduledRunsRequest{TenantId: testTenantUUID},
	)
	if err != nil || len(resp.Runs) != 1 {
		t.Fatalf("ClaimDueScheduledRuns: %v %v", resp, err)
	}
	g, err := gatewaygrant.Verify(testGrantKey, resp.Runs[0].OwnerGrant, false)
	if err != nil || g.Actor != def.ID.String() || resp.Runs[0].ActorAgentId != def.ID.String() {
		t.Fatalf("grant actor %q / run actor %q (%v), want the default agent %s", g.Actor, resp.Runs[0].ActorAgentId, err, def.ID)
	}
}
