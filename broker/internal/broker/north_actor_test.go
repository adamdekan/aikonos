package broker

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/gatewaygrant"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// newNorthActorSvc wires CreateTask with a fake OpenFGA answering checks and
// an agent store holding agents (all in the test tenant).
func newNorthActorSvc(t *testing.T, checks map[string]bool, agents ...uuid.UUID) (*BrokerService, *fakeCreateStore) {
	t.Helper()
	srv := (&fakeFGA{checks: checks}).server(t)
	t.Cleanup(srv.Close)
	eng, err := policy.NewEngine(context.Background(), policy.Config{
		OPAEndpoint: "http://unused", OpenFGAEndpoint: srv.URL, OpenFGAStoreID: "store-1",
	})
	if err != nil {
		t.Fatalf("policy.NewEngine: %v", err)
	}
	store := &fakeCreateStore{}
	deps := newMinimalDeps(t)
	deps.Policy, deps.Tasks, deps.TenantID = eng, store, testTenantUUID
	tid := uuid.MustParse(testTenantUUID)
	agentStore := &fakeAgentStore{}
	for _, a := range agents {
		agentStore.agents = append(agentStore.agents, &db.Agent{ID: a, TenantID: tid, Name: "a"})
	}
	deps.Agents = agentStore
	return NewBrokerService(deps), store
}

// AG-17: a north task binds the requested agent when the caller may operate it
// and records kind=interactive.
func TestCreateTask_AgentID_Permitted(t *testing.T) {
	agent := uuid.New()
	svc, store := newNorthActorSvc(t, map[string]bool{
		"user:alice@example.com|can_use|agent:" + agent.String(): true,
	}, agent)
	if _, err := svc.CreateTask(ctxWithIdentity(testTenantUUID, "alice@example.com"), &brokerv1.CreateTaskRequest{
		TenantId: testTenantUUID, Prompt: "p", AgentId: agent.String(),
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	got := store.created[0]
	if got.AgentID == nil || *got.AgentID != agent {
		t.Errorf("agent = %v, want %s", got.AgentID, agent)
	}
	if o := got.OnBehalfOf(); o.Kind != gatewaygrant.KindInteractive || o.Actor != agent.String() {
		t.Errorf("obo = %+v, want interactive actor %s", o, agent)
	}
}

// Without an agent the north task is unbound but still interactive.
func TestCreateTask_NoAgentID_Interactive(t *testing.T) {
	store := &fakeCreateStore{}
	svc := newBrokerSvcForCreateTaskTest(t, store)
	if _, err := svc.CreateTask(context.Background(), &brokerv1.CreateTaskRequest{
		TenantId: testTenantUUID, UserId: "alice@example.com", Prompt: "p",
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if got := store.created[0]; got.AgentID != nil || got.OnBehalfOf().Kind != "interactive" {
		t.Errorf("task = agent %v obo %+v, want unbound interactive", got.AgentID, got.OnBehalfOf())
	}
}

// A caller who may not operate the agent (no can_use, not admin) is refused
// with a DENY audit naming actor_not_permitted.
func TestCreateTask_AgentID_Denied(t *testing.T) {
	agentID := uuid.New()
	agent := agentID.String()
	svc, store := newNorthActorSvc(t, map[string]bool{}, agentID)
	em, sub := buildAuditBus(t, testTenantUUID)
	svc.deps.Audit = em

	_, err := svc.CreateTask(ctxWithIdentity(testTenantUUID, "alice@example.com"), &brokerv1.CreateTaskRequest{
		TenantId: testTenantUUID, Prompt: "p", AgentId: agent,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
	if len(store.created) != 0 {
		t.Error("no task must be created for a refused actor")
	}
	ev := receiveAuditEvent(t, sub, "aikonos.broker.task.denied", 2*time.Second)
	if r := ev.Context.Fields["reason"].GetStringValue(); r != "actor_not_permitted" {
		t.Errorf("reason = %q, want actor_not_permitted", r)
	}
	if ev.ActorAgentId != agent || ev.ActorUserId != "alice@example.com" {
		t.Errorf("event actor = %q/%q, want alice/%s", ev.ActorUserId, ev.ActorAgentId, agent)
	}
}

// An agent that does not exist in the tenant is refused before the FGA check
// (even an allowing tuple does not help) with a DENY audit naming
// actor_not_found.
func TestCreateTask_AgentID_NotFound(t *testing.T) {
	agent := uuid.NewString()
	svc, store := newNorthActorSvc(t, map[string]bool{
		"user:alice@example.com|can_use|agent:" + agent: true,
	}) // no agents seeded
	em, sub := buildAuditBus(t, testTenantUUID)
	svc.deps.Audit = em

	_, err := svc.CreateTask(ctxWithIdentity(testTenantUUID, "alice@example.com"), &brokerv1.CreateTaskRequest{
		TenantId: testTenantUUID, Prompt: "p", AgentId: agent,
	})
	if status.Code(err) != codes.PermissionDenied || len(store.created) != 0 {
		t.Fatalf("want PermissionDenied and no task, got %v (%d created)", err, len(store.created))
	}
	ev := receiveAuditEvent(t, sub, "aikonos.broker.task.denied", 2*time.Second)
	if r := ev.Context.Fields["reason"].GetStringValue(); r != "actor_not_found" {
		t.Errorf("reason = %q, want actor_not_found", r)
	}
}

// A non-UUID agent_id (e.g. the "<user>-agent" pseudo-id) is InvalidArgument.
func TestCreateTask_AgentID_Invalid(t *testing.T) {
	store := &fakeCreateStore{}
	svc := newBrokerSvcForCreateTaskTest(t, store)
	_, err := svc.CreateTask(context.Background(), &brokerv1.CreateTaskRequest{
		TenantId: testTenantUUID, UserId: "alice@example.com", Prompt: "p", AgentId: "alice-agent",
	})
	if status.Code(err) != codes.InvalidArgument || len(store.created) != 0 {
		t.Fatalf("want InvalidArgument and no task, got %v (%d created)", err, len(store.created))
	}
}

// A workflow grant stamps kind=workflow on the south twin task.
func TestCreateGatewayTask_WorkflowGrantStampsKind(t *testing.T) {
	store := &fakeCreateStore{}
	svc := newSandboxSvcForGatewayTest(t, store, nil)
	agent := uuid.New()
	grant := mintV2Grant(t, gatewaygrant.Grant{TenantID: testTenantUUID, Subject: "alice@example.com",
		Actor: agent.String(), Kind: gatewaygrant.KindWorkflow, GrantID: uuid.NewString()})
	if _, err := svc.CreateGatewayTask(gatewaySpiffeCtx(), &brokerv1.CreateGatewayTaskRequest{
		TenantId: testTenantUUID, Prompt: "p", OwnerGrant: grant,
	}); err != nil {
		t.Fatalf("CreateGatewayTask: %v", err)
	}
	if o := store.created[0].OnBehalfOf(); o.Kind != "workflow" || o.Actor != agent.String() {
		t.Errorf("obo = %+v, want workflow actor %s", o, agent)
	}
}
