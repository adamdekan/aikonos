package broker

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/apikey"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	"github.com/adamdekan/aikonos/broker/internal/revocation"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// AG-12: the actor chain on a task-scoped event names the user, the agent,
// the envelope sender, the external client and the service_agent flag.
func TestWithOBO_CarriesFullActorChain(t *testing.T) {
	agent, env, client := uuid.New(), uuid.New(), uuid.New()
	sender := "bob@example.com"
	ev := withOBO(&auditv1.AuditEvent{}, &db.Task{
		OwnerUserID: "alice@example.com", AgentID: &agent, DelegatedByUserID: &sender,
		EnvelopeID: &env, ExternalClientID: &client, ServiceAgent: true,
	})
	obo := ev.Context.Fields["obo"].GetStructValue().GetFields()
	want := map[string]string{
		"subject": "alice@example.com", "actor": agent.String(), "delegated_by": sender,
		"envelope_id": env.String(), "external_client_id": client.String(),
	}
	for k, v := range want {
		if got := obo[k].GetStringValue(); got != v {
			t.Errorf("obo.%s = %q, want %q", k, got, v)
		}
	}
	if !obo["service_agent"].GetBoolValue() || ev.ActorAgentId != agent.String() {
		t.Errorf("service_agent=%v actor_agent_id=%q, want true and %s", obo["service_agent"], ev.ActorAgentId, agent)
	}
}

// Events emitted through a package that cannot call withOBO (approvalsvc) and
// the network-ACL events from SubmitPlan carry the ctx task's chain.
func TestOBOEmitterAndNetworkAudit_StampCtxTask(t *testing.T) {
	agent := uuid.New()
	ctx := withOBOTask(context.Background(), &db.Task{OwnerUserID: "alice@example.com", AgentID: &agent})
	aud := &fakeInvokeAudit{}

	_ = oboEmitter{aud}.Emit(ctx, &auditv1.AuditEvent{EventType: "aikonos.broker.approval.insufficient_approvers"})
	svc := &SandboxService{deps: Deps{Logger: zap.NewNop(), Audit: aud}}
	svc.emitNetworkAudit(ctx, "tr", "t1", "alice@example.com", "example.com", "aikonos.network.fetch.denied", auditv1.PolicyDecision_DENY)

	if len(aud.events) != 2 {
		t.Fatalf("got %d events, want 2", len(aud.events))
	}
	for _, ev := range aud.events {
		obo := ev.Context.GetFields()["obo"].GetStructValue().GetFields()
		if ev.ActorAgentId != agent.String() || obo["subject"].GetStringValue() != "alice@example.com" {
			t.Errorf("%s: actor_agent_id=%q obo=%v, want the ctx task chain", ev.EventType, ev.ActorAgentId, obo)
		}
	}
}

// A new task's task.created event knows its agent is a service agent before
// the row is ever re-read.
func TestPersistManagedTask_CreatedEventCarriesServiceAgent(t *testing.T) {
	tenant, agent := uuid.New(), uuid.New()
	aud := &fakeInvokeAudit{}
	srv := (&fakeFGA{}).server(t)
	t.Cleanup(srv.Close)
	eng, err := policy.NewEngine(context.Background(), policy.Config{OPAEndpoint: "http://unused", OpenFGAEndpoint: srv.URL, OpenFGAStoreID: "store-1"})
	if err != nil {
		t.Fatal(err)
	}
	deps := Deps{Logger: zap.NewNop(), Audit: aud, Policy: eng, Agents: &fakeAgentStore{agents: []*db.Agent{
		{ID: agent, TenantID: tenant, ServiceAgent: true},
	}}}
	tasks := &fakeInvokeToolTaskStore{}
	task := &db.Task{TaskID: uuid.New(), TenantID: tenant, OwnerUserID: "svc-" + agent.String(), AgentID: &agent}
	if _, err := persistManagedTask(context.Background(), deps, tasks, task, "tr"); err != nil {
		t.Fatal(err)
	}
	for _, ev := range aud.events {
		if ev.EventType == "aikonos.broker.task.created" {
			if !ev.Context.Fields["obo"].GetStructValue().GetFields()["service_agent"].GetBoolValue() {
				t.Fatalf("task.created obo = %v, want service_agent=true", ev.Context.Fields["obo"])
			}
			return
		}
	}
	t.Fatalf("no task.created event in %v", aud.events)
}

// AG-06/AG-15 credential events: a key voided by the agent's revoke-all is
// audited as a refusal, and a revoke names the key.
func TestApiKeyCredentialEvents(t *testing.T) {
	const admin = "admin@example.com"
	deps := newApiKeyDeps(t, admin, &fakeApiKeyStore{}, agentStoreWithAgent(t))
	deps.Revocation = revocation.New(newFakeRevStore())
	aud := &fakeInvokeAudit{}
	deps.Audit = aud
	north, south := NewBrokerService(deps), NewSandboxService(deps)
	ctx := ctxWithIdentity(apiKeyTestTenantUUID, admin)

	minted, err := north.MintAgentApiKey(ctx, &brokerv1.MintAgentApiKeyRequest{TenantId: apiKeyTestTenantUUID, AgentId: apiKeyTestAgentUUID, TtlDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := north.RevokeAgentSessions(ctx, &brokerv1.RevokeAgentSessionsRequest{TenantId: apiKeyTestTenantUUID, AgentId: apiKeyTestAgentUUID, Reason: "compromised"}); err != nil {
		t.Fatal(err)
	}
	if _, err := south.ResolveAgentApiKey(sandboxCtx(), &brokerv1.ResolveAgentApiKeyRequest{KeyHashHex: apikey.HashForResolve(minted.RawKey), TenantId: apiKeyTestTenantUUID}); err != nil {
		t.Fatal(err)
	}
	if _, err := north.RevokeAgentApiKey(ctx, &brokerv1.RevokeAgentApiKeyRequest{TenantId: apiKeyTestTenantUUID, AgentId: apiKeyTestAgentUUID, KeyId: minted.Key.Id}); err != nil {
		t.Fatal(err)
	}

	var denied, revoked bool
	for _, ev := range aud.events {
		f := ev.Context.GetFields()
		switch ev.EventType {
		case "aikonos.broker.apikey.denied":
			denied = denied || f["reason"].GetStringValue() == "sessions_revoked"
		case "aikonos.broker.apikey.revoked":
			revoked = f["key_id"].GetStringValue() == minted.Key.Id
		}
	}
	if !denied || !revoked {
		t.Fatalf("sessions_revoked denial=%v, revoked with key_id=%v; events %v", denied, revoked, aud.events)
	}
}
