package broker

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/db"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// AG-14: the inventory carries owners, live key counts, the nearest live
// expiry and the last use of any unrevoked key, drops the soul, is audited,
// is admin-only and fails closed when owners cannot be read.
func TestExportAgentInventory(t *testing.T) {
	const (
		keyed = "aaaaaaaa-0000-0000-0000-000000000021"
		bare  = "aaaaaaaa-0000-0000-0000-000000000022"
	)
	store := &fakeAgentStore{}
	a := makeTestAgent(t, agentTestTenantUUID, keyed, "keyed")
	a.Soul = "secret system prompt"
	a.ServiceAgent = true
	a.McpServers = []string{"jira"}
	a.McpTools = []string{"mcp:jira:search"}
	store.agents = []*db.Agent{a, makeTestAgent(t, agentTestTenantUUID, bare, "bare")}

	tid := uuid.MustParse(agentTestTenantUUID)
	aid := uuid.MustParse(keyed)
	now := time.Now()
	used := func(d time.Duration) *time.Time { t := now.Add(d); return &t }
	soon, later := now.Add(48*time.Hour), now.Add(30*24*time.Hour)
	apiKeys := &fakeApiKeyStore{keys: []*db.StoredApiKey{
		{ID: uuid.New(), TenantID: tid, AgentID: aid, ExpiresAt: later, LastUsedAt: used(-2 * time.Hour)},
		// Expired: not counted, no expiry, but its use still counts.
		{ID: uuid.New(), TenantID: tid, AgentID: aid, ExpiresAt: now.Add(-time.Hour), LastUsedAt: used(-time.Hour)},
		// Revoked: invisible.
		{ID: uuid.New(), TenantID: tid, AgentID: aid, ExpiresAt: later, LastUsedAt: used(-time.Minute), RevokedAt: used(-30 * time.Second)},
	}}
	pubKeys := &fakePublicKeyStore{keys: []*db.StoredPublicKey{
		{ID: uuid.New(), TenantID: tid, AgentID: aid, Kid: "k1", Status: "active", ExpiresAt: soon},
	}}

	f := &fakeFGA{
		admins: map[string]bool{"user:admin@example.com": true},
		reads: map[string][]fgaKey{
			"agent:" + keyed: {
				{User: "user:alice", Relation: "owner_user", Object: "agent:" + keyed},
				{User: "group:ops#member", Relation: "owner_group", Object: "agent:" + keyed},
				{User: "user:carol", Relation: "usable_by", Object: "agent:" + keyed},
			},
		},
	}
	deps := newListMyAgentsDeps(t, f, store)
	deps.ApiKeys = apiKeys
	deps.PublicKeys = pubKeys
	aud := &fakeInvokeAudit{}
	deps.Audit = aud
	svc := NewBrokerService(deps)
	ctx := ctxWithIdentity(agentTestTenantUUID, "admin@example.com")

	resp, err := svc.ExportAgentInventory(ctx, &brokerv1.ExportAgentInventoryRequest{})
	if err != nil {
		t.Fatalf("ExportAgentInventory: %v", err)
	}
	if len(resp.Agents) != 2 {
		t.Fatalf("want 2 entries, got %d", len(resp.Agents))
	}
	e := resp.Agents[0]
	if e.Agent.Id != keyed || e.Agent.Soul != "" || !e.Agent.ServiceAgent {
		t.Fatalf("agent = %+v (soul must be dropped)", e.Agent)
	}
	if got := e.Agent.Owners; len(got) != 2 || got[0] != "user:alice" || got[1] != "group:ops#member" {
		t.Fatalf("owners = %v", got)
	}
	if len(e.Agent.McpServers) != 1 || len(e.Agent.McpTools) != 1 {
		t.Fatalf("connectors/tools = %v / %v", e.Agent.McpServers, e.Agent.McpTools)
	}
	if e.ApiKeys != 1 || e.PublicKeys != 1 {
		t.Fatalf("live keys = %d api, %d public; want 1, 1", e.ApiKeys, e.PublicKeys)
	}
	if !e.NearestKeyExpiry.AsTime().Equal(soon) {
		t.Fatalf("nearest expiry = %v, want %v", e.NearestKeyExpiry.AsTime(), soon)
	}
	if !e.LastUsedAt.AsTime().Equal(*used(-time.Hour)) {
		t.Fatalf("last used = %v, want the expired key's use", e.LastUsedAt.AsTime())
	}
	b := resp.Agents[1]
	if b.ApiKeys != 0 || b.NearestKeyExpiry != nil || b.LastUsedAt != nil || len(b.Agent.Owners) != 0 {
		t.Fatalf("bare agent entry = %+v", b)
	}
	if ev := findEvent(aud.events, "aikonos.broker.agent.inventory.exported"); ev == nil || ev.ActorUserId != "admin@example.com" {
		t.Fatalf("export must be audited, got %v", ev)
	}

	// Not an admin.
	if _, err := svc.ExportAgentInventory(ctxWithIdentity(agentTestTenantUUID, "carol@example.com"), &brokerv1.ExportAgentInventoryRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-admin: want PermissionDenied, got %v", err)
	}

	// Owners unreadable: the export fails closed, ListAgents stays best-effort.
	f.readErr = true
	if _, err := svc.ExportAgentInventory(ctx, &brokerv1.ExportAgentInventoryRequest{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("unreadable owners: want Unavailable, got %v", err)
	}
	if _, err := svc.ListAgents(ctx, &brokerv1.ListAgentsRequest{}); err != nil {
		t.Fatalf("ListAgents must stay best-effort: %v", err)
	}
}
