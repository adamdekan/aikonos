package broker

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// scopedEnvelope is a delivered delegation as read back from the row (JSON
// numbers and arrays decode to float64 / []any).
func scopedEnvelope(envelopeID, toUser string) *db.Envelope {
	env := deliveredEnvelope(testTenantUUID, envelopeID, toUser)
	env.DelegationSpec = map[string]any{"attenuated_scopes": []any{"siem:read"}, "max_cost_units": float64(50)}
	return env
}

func checkDelegatedTask(t *testing.T, task *db.Task, envelopeID string) {
	t.Helper()
	o := task.OnBehalfOf()
	if o.DelegatedBy != "alice@example.com" || o.EnvelopeID != envelopeID || o.Kind != "delegated" {
		t.Errorf("obo = %+v, want delegated_by alice, envelope %s, kind delegated", o, envelopeID)
	}
	if !slices.Equal(task.DelegatedScopes, []string{"siem:read"}) {
		t.Errorf("delegated_scopes = %v, want [siem:read]", task.DelegatedScopes)
	}
	if task.CostBudget != 50 {
		t.Errorf("cost_budget = %d, want the envelope's 50", task.CostBudget)
	}
}

// AG-11: the task an accepted envelope spawns carries the envelope id, its
// attenuated scopes, the cost limit and the delegated obo kind.
func TestRespondToEnvelope_Accept_CarriesLimits(t *testing.T) {
	const envelopeID = "44444444-4444-4444-4444-444444444444"
	store := &fakeAcceptStore{envelope: scopedEnvelope(envelopeID, "bob@example.com")}
	svc := NewBrokerService(testDismissDeps(t, store))
	if _, err := svc.RespondToEnvelope(ctxWithIdentity(testTenantUUID, "bob@example.com"), &brokerv1.RespondToEnvelopeRequest{
		EnvelopeId: envelopeID, TenantId: testTenantUUID, Accepted: true,
	}); err != nil {
		t.Fatalf("RespondToEnvelope: %v", err)
	}
	checkDelegatedTask(t, store.created, envelopeID)
}

// AG-11: the inbox "Send to agent" path (the envelope is dismissed before
// the chat runs) carries the same limits.
func TestCreateTask_EnvelopeID_CarriesLimits(t *testing.T) {
	env := scopedEnvelope(testEnvelopeID, "bob@example.com")
	env.State = db.EnvelopeDismissed
	store := &envelopeCreateStore{envelope: env}
	svc := newBrokerSvcForCreateTaskTest(t, store)
	if _, err := svc.CreateTask(ctxWithIdentity(testTenantUUID, "bob@example.com"), &brokerv1.CreateTaskRequest{
		TenantId: testTenantUUID, Prompt: "p", EnvelopeId: testEnvelopeID,
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	checkDelegatedTask(t, store.created[0], testEnvelopeID)
}

// The in-memory envelope on SendEnvelope's auto-accept path holds []string
// and int64, not the JSON-decoded shapes.
func TestApplyEnvelopeLimits_InMemoryShapes(t *testing.T) {
	env := deliveredEnvelope(testTenantUUID, testEnvelopeID, "bob@example.com")
	env.DelegationSpec = map[string]any{"attenuated_scopes": []string{"siem:read"}, "max_cost_units": int64(50)}
	task := &db.Task{CostBudget: 1000}
	applyEnvelopeLimits(task, env)
	checkDelegatedTask(t, task, testEnvelopeID)

	// A notification envelope (no delegation spec) conveys no authority and
	// sets no scope limit.
	task = &db.Task{CostBudget: 1000}
	applyEnvelopeLimits(task, deliveredEnvelope(testTenantUUID, testEnvelopeID, "bob@example.com"))
	if task.DelegatedScopes != nil || task.CostBudget != 1000 {
		t.Errorf("notification envelope limited the task: scopes %v budget %d", task.DelegatedScopes, task.CostBudget)
	}
	// An empty grant still limits (to nothing).
	env.DelegationSpec = map[string]any{"attenuated_scopes": []any{}}
	task = &db.Task{}
	applyEnvelopeLimits(task, env)
	if task.DelegatedScopes == nil || len(task.DelegatedScopes) != 0 {
		t.Errorf("empty grant: scopes = %#v, want non-nil empty", task.DelegatedScopes)
	}
}

// AG-11: SubmitPlan denies a delegated task's step whose required scope is
// outside the envelope's attenuated scopes, and records why; a step inside
// them proceeds to the normal pipeline.
func TestSubmitPlan_DelegationScope(t *testing.T) {
	envID := uuid.MustParse(testEnvelopeID)
	newSvc := func() (*SandboxService, *capturingAudit) {
		tasks := &fakeTaskStore{task: newFakeTask(db.TaskStateValidating)}
		from := "alice@example.com"
		tasks.task.DelegatedByUserID = &from
		tasks.task.EnvelopeID = &envID
		tasks.task.DelegatedScopes = []string{"siem:read"}
		audit := &capturingAudit{}
		return &SandboxService{deps: Deps{GatewaySpiffeID: testGWSpiffeID, Logger: zap.NewNop(), Audit: audit, Tasks: tasks, DevMode: true},
			policy: &fakePolicyEngine{planAllow: true, toolDec: &policy.Decision{Allow: true}}}, audit
	}

	svc, audit := newSvc()
	res, err := svc.SubmitPlan(asGateway(context.Background()), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(), Plan: makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "slack.post", nil)}),
	})
	if err != nil || res.Outcome != planv1.ValidationOutcome_DENIED {
		t.Fatalf("out-of-scope step: %v, %v; want DENIED", res, err)
	}
	records := audit.ofType(PolicyDecisionEvent)
	if len(records) != 1 || field(records[0], "kind") != decisionKindDelegationScope {
		t.Fatalf("want one delegation_scope record, got %+v", records)
	}
	if field(records[0], "obo", "delegated_by") != "alice@example.com" || field(records[0], "obo", "envelope_id") != testEnvelopeID {
		t.Errorf("record obo = %v, want sender and envelope", field(records[0], "obo"))
	}

	svc, _ = newSvc()
	res, err = svc.SubmitPlan(asGateway(context.Background()), &brokerv1.SubmitPlanRequest{
		TaskId: validTaskUUID(), Plan: makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "siem.query", nil)}),
	})
	if err != nil || res.Outcome == planv1.ValidationOutcome_DENIED {
		t.Fatalf("in-scope step: %v, %v; want it past the delegation gate", res, err)
	}
}

func TestDelegationScopeViolation(t *testing.T) {
	plan := makePlan(testTenantUUID, []*planv1.PlanStep{makeStep(1, "siem.query", nil), makeStep(2, "unregistered.tool", nil)})
	reg := (&SandboxService{}).toolReg()
	if v := delegationScopeViolation(reg, &db.Task{}, plan); v != "" {
		t.Errorf("no limit: %q", v)
	}
	if v := delegationScopeViolation(reg, &db.Task{DelegatedScopes: []string{"siem:read"}}, plan); v == "" {
		t.Error("an unregistered tool must fall outside any delegation")
	}
	if v := delegationScopeViolation(reg, &db.Task{DelegatedScopes: []string{}}, plan); v == "" {
		t.Error("an empty grant must deny every step")
	}
}
