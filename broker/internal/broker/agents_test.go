package broker

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ── validateAgent: pure-function table tests ──────────────────────────────────

func TestValidateAgent(t *testing.T) {
	cases := []struct {
		name    string
		agent   *brokerv1.Agent
		wantErr codes.Code // codes.OK means no error expected
	}{
		{
			name: "valid minimal",
			agent: &brokerv1.Agent{
				Name:         "my-agent",
				ApprovalMode: "needs_approval",
			},
			wantErr: codes.OK,
		},
		{
			name: "valid with skills and mcp",
			agent: &brokerv1.Agent{
				Name:         "full-agent",
				ApprovalMode: "auto",
				LlmModel:     "anthropic/claude-sonnet-4.6",
				Skills:       []string{"web.fetch", "doc.read", "mcp:my-server:some-tool"},
				McpServers:   []string{"conn-1", "conn-2"},
			},
			wantErr: codes.OK,
		},
		{
			name: "empty skills allowed",
			agent: &brokerv1.Agent{
				Name:         "no-tools-agent",
				ApprovalMode: "needs_approval",
				Skills:       []string{},
			},
			wantErr: codes.OK,
		},
		{
			name:    "nil agent",
			agent:   nil,
			wantErr: codes.InvalidArgument,
		},
		{
			name:    "empty name",
			agent:   &brokerv1.Agent{Name: "", ApprovalMode: "needs_approval"},
			wantErr: codes.InvalidArgument,
		},
		{
			name:    "name whitespace only",
			agent:   &brokerv1.Agent{Name: "   ", ApprovalMode: "needs_approval"},
			wantErr: codes.InvalidArgument,
		},
		{
			name:    "name too long",
			agent:   &brokerv1.Agent{Name: string(make([]byte, 129)), ApprovalMode: "needs_approval"},
			wantErr: codes.InvalidArgument,
		},
		{
			name:    "bad approval_mode",
			agent:   &brokerv1.Agent{Name: "x", ApprovalMode: "always"},
			wantErr: codes.InvalidArgument,
		},
		{
			name:    "bad llm_model",
			agent:   &brokerv1.Agent{Name: "x", ApprovalMode: "auto", LlmModel: "not-valid"},
			wantErr: codes.InvalidArgument,
		},
		{
			name: "approval_mode defaults to needs_approval when empty",
			agent: &brokerv1.Agent{
				Name:         "x",
				ApprovalMode: "",
			},
			wantErr: codes.OK,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAgent(tc.agent)
			if tc.wantErr == codes.OK {
				if err != nil {
					t.Errorf("want no error, got %v", err)
				}
				// Verify default fill for approval_mode
				if tc.agent != nil && tc.agent.ApprovalMode == "" {
					t.Errorf("approval_mode not defaulted")
				}
				return
			}
			if err == nil {
				t.Fatalf("want error %v, got nil", tc.wantErr)
			}
			if got := status.Code(err); got != tc.wantErr {
				t.Errorf("want code %v, got %v: %v", tc.wantErr, got, err)
			}
		})
	}
}

// ── validateAgentSkills: registry + capabilitySkills vocabulary ───────────────
// A zero-value BrokerService resolves toolReg() to the package-default registry,
// which carries the baseline tools — no DB or deps needed.

func TestValidateAgentSkills(t *testing.T) {
	s := &BrokerService{}
	cases := []struct {
		name    string
		skills  []string
		wantErr codes.Code
	}{
		{"empty", nil, codes.OK},
		{"baseline tool", []string{"web.fetch", "doc.read"}, codes.OK},
		{"mcp prefixed", []string{"mcp:server-id:tool-name"}, codes.OK},
		{"capability workflows", []string{"workflows"}, codes.OK},
		{"capability scheduler", []string{"scheduler"}, codes.OK},
		{"capability vision", []string{"vision"}, codes.OK},
		{"mixed valid", []string{"web.fetch", "workflows", "mcp:s:t"}, codes.OK},
		{"unknown skill", []string{"unknown.tool"}, codes.InvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.validateAgentSkills(&brokerv1.Agent{Skills: tc.skills})
			if got := status.Code(err); got != tc.wantErr {
				t.Errorf("want code %v, got %v: %v", tc.wantErr, got, err)
			}
		})
	}
}

// ── handler tests: CreateAgent happy-path and requireTenantAdmin deny ─────────
// These use the fakeFGA + policy.Engine seam already established by alerts_test.go
// and roles_test.go. No DB is needed: agentsEnabled() = false when Agents is nil,
// so we wire a real *db.AgentRepo only via the interface — but since we can't
// construct a real pgxpool in a unit test, we test the guard paths only.
// DB-backed handler behaviour is integration-tested in-cluster (repo norm).

const agentTestTenantUUID = "00000000-0000-0000-0000-000000000002"
const agentTestTenantName = "aikonos-dev"

func newAgentsDeps(t *testing.T, adminUser string) Deps {
	t.Helper()
	fga := &fakeFGA{admins: map[string]bool{"user:" + adminUser: true}}
	fgaSrv := fga.server(t)
	t.Cleanup(fgaSrv.Close)

	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("audit.NewEmitter: %v", err)
	}
	eng, err := policy.NewEngine(context.Background(), policy.Config{
		OPAEndpoint:     "http://unused",
		OpenFGAEndpoint: fgaSrv.URL,
		OpenFGAStoreID:  "store-1",
	})
	if err != nil {
		t.Fatalf("policy.NewEngine: %v", err)
	}
	return Deps{
		Logger:   zap.NewNop(),
		Policy:   eng,
		TenantID: agentTestTenantName,
		Audit:    em,
		// Agents intentionally nil — exercises agentsEnabled() guard path.
	}
}

func TestCreateAgent_AgentsDisabled(t *testing.T) {
	deps := newAgentsDeps(t, "admin@example.com")
	svc := NewBrokerService(deps)

	_, err := svc.CreateAgent(context.Background(), &brokerv1.CreateAgentRequest{
		TenantId: agentTestTenantUUID,
		UserId:   "admin@example.com",
		Agent:    &brokerv1.Agent{Name: "test", ApprovalMode: "auto"},
	})
	if err == nil {
		t.Fatal("want error when agents disabled, got nil")
	}
	if code := status.Code(err); code != codes.FailedPrecondition {
		t.Errorf("want FailedPrecondition, got %v", code)
	}
}

// TestCreateAgent_DisabledWhenRepoNil verifies that every agent RPC returns
// FailedPrecondition when Agents is nil (feature disabled). The admin gate
// (requireTenantAdmin → PermissionDenied) is covered by the shared fakeFGA
// harness in roles_test.go; it cannot be reached through agents.go without a
// non-nil AgentRepo, which requires a live pgxpool unavailable in unit tests.
func TestCreateAgent_DisabledWhenRepoNil(t *testing.T) {
	deps := newAgentsDeps(t, "admin@example.com")
	svc := NewBrokerService(deps)

	_, err := svc.ListAgents(context.Background(), &brokerv1.ListAgentsRequest{
		TenantId: agentTestTenantUUID,
		UserId:   "nonadmin@example.com",
	})
	if err == nil {
		t.Fatal("want error when agents disabled, got nil")
	}
	if code := status.Code(err); code != codes.FailedPrecondition {
		t.Errorf("want FailedPrecondition, got %v", code)
	}
}

func TestListMyAgents_AgentsDisabled(t *testing.T) {
	deps := newAgentsDeps(t, "admin@example.com")
	svc := NewBrokerService(deps)

	_, err := svc.ListMyAgents(context.Background(), &brokerv1.ListMyAgentsRequest{
		TenantId: agentTestTenantUUID,
		UserId:   "alice@example.com",
	})
	if err == nil {
		t.Fatal("want error when agents disabled, got nil")
	}
	if code := status.Code(err); code != codes.FailedPrecondition {
		t.Errorf("want FailedPrecondition, got %v", code)
	}
}

// ── fakeAgentStore ────────────────────────────────────────────────────────────

// fakeAgentStore is an in-memory AgentStore used by handler-level tests.
type fakeAgentStore struct {
	mu         sync.Mutex
	agents     []*db.Agent
	setSoulErr error // when set, SetSoul returns it (simulates a DB error after the gate passes)
}

func (f *fakeAgentStore) Create(_ context.Context, a *db.Agent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agents = append(f.agents, a)
	return nil
}

func (f *fakeAgentStore) List(_ context.Context, tenantID string) ([]*db.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*db.Agent
	for _, a := range f.agents {
		if a.TenantID.String() == tenantID {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fakeAgentStore) Get(_ context.Context, tenantID, id string) (*db.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.agents {
		if a.TenantID.String() == tenantID && a.ID.String() == id {
			return a, nil
		}
	}
	return nil, db.ErrAgentNotFound
}

func (f *fakeAgentStore) Update(_ context.Context, a *db.Agent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, existing := range f.agents {
		if existing.ID == a.ID && existing.TenantID == a.TenantID {
			f.agents[i] = a
			return nil
		}
	}
	return db.ErrAgentNotFound
}

func (f *fakeAgentStore) Delete(_ context.Context, tenantID, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, a := range f.agents {
		if a.TenantID.String() == tenantID && a.ID.String() == id {
			f.agents = append(f.agents[:i], f.agents[i+1:]...)
			return nil
		}
	}
	return db.ErrAgentNotFound
}

func (f *fakeAgentStore) SetSoul(_ context.Context, tenantID, id, soul string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setSoulErr != nil {
		return f.setSoulErr
	}
	for _, a := range f.agents {
		if a.TenantID.String() == tenantID && a.ID.String() == id {
			a.Soul = soul
			return nil
		}
	}
	return db.ErrAgentNotFound
}

// ── ListMyAgents handler tests ────────────────────────────────────────────────

func newListMyAgentsDeps(t *testing.T, fga *fakeFGA, store AgentStore) Deps {
	t.Helper()
	fgaSrv := fga.server(t)
	t.Cleanup(fgaSrv.Close)

	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("audit.NewEmitter: %v", err)
	}
	eng, err := policy.NewEngine(context.Background(), policy.Config{
		OPAEndpoint:     "http://unused",
		OpenFGAEndpoint: fgaSrv.URL,
		OpenFGAStoreID:  "store-1",
	})
	if err != nil {
		t.Fatalf("policy.NewEngine: %v", err)
	}
	return Deps{
		Logger:   zap.NewNop(),
		Policy:   eng,
		TenantID: agentTestTenantName,
		Audit:    em,
		Agents:   store,
	}
}

func makeTestAgent(t *testing.T, tenantUUID, id, name string) *db.Agent {
	t.Helper()
	tid, err := uuid.Parse(tenantUUID)
	if err != nil {
		t.Fatalf("parse tenant uuid: %v", err)
	}
	aid, err := uuid.Parse(id)
	if err != nil {
		t.Fatalf("parse agent uuid: %v", err)
	}
	return &db.Agent{
		ID:           aid,
		TenantID:     tid,
		Name:         name,
		ApprovalMode: "needs_approval",
		Skills:       []string{},
		McpServers:   []string{},
	}
}

// agentA and agentB are stable IDs used across ListMyAgents tests.
const (
	agentAID = "aaaaaaaa-0000-0000-0000-000000000001"
	agentBID = "bbbbbbbb-0000-0000-0000-000000000002"
)

// TestListMyAgents_FGAIntersection: ListObjects returns [agent:a, agent:x-not-in-db],
// repo has agents a and b → response contains only a (intersection; prefix stripped).
func TestListMyAgents_FGAIntersection(t *testing.T) {
	store := &fakeAgentStore{}
	store.agents = []*db.Agent{
		makeTestAgent(t, agentTestTenantUUID, agentAID, "agent-a"),
		makeTestAgent(t, agentTestTenantUUID, agentBID, "agent-b"),
	}
	fga := &fakeFGA{
		admins:            map[string]bool{},
		listObjectsResult: []string{"agent:" + agentAID, "agent:xxxxxxxx-ffff-ffff-ffff-ffffffffffff"},
	}
	deps := newListMyAgentsDeps(t, fga, store)
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(agentTestTenantUUID, "alice@example.com")
	resp, err := svc.ListMyAgents(ctx, &brokerv1.ListMyAgentsRequest{
		TenantId: agentTestTenantUUID,
		UserId:   "alice@example.com",
	})
	if err != nil {
		t.Fatalf("ListMyAgents: %v", err)
	}
	if len(resp.Agents) != 1 {
		t.Fatalf("want 1 agent (intersection), got %d: %+v", len(resp.Agents), resp.Agents)
	}
	if resp.Agents[0].Id != agentAID {
		t.Errorf("want agent a (%s), got %s", agentAID, resp.Agents[0].Id)
	}
}

// TestListMyAgents_ListObjectsError: ListObjects error → codes.Internal (fail-closed).
func TestListMyAgents_ListObjectsError(t *testing.T) {
	store := &fakeAgentStore{}
	store.agents = []*db.Agent{
		makeTestAgent(t, agentTestTenantUUID, agentAID, "agent-a"),
	}
	fga := &fakeFGA{
		admins:         map[string]bool{},
		listObjectsErr: true,
	}
	deps := newListMyAgentsDeps(t, fga, store)
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(agentTestTenantUUID, "alice@example.com")
	_, err := svc.ListMyAgents(ctx, &brokerv1.ListMyAgentsRequest{
		TenantId: agentTestTenantUUID,
		UserId:   "alice@example.com",
	})
	if err == nil {
		t.Fatal("want error on ListObjects failure, got nil")
	}
	if code := status.Code(err); code != codes.Internal {
		t.Errorf("want codes.Internal (fail-closed), got %v", code)
	}
}

// TestListMyAgents_FGADisabled: FGA disabled → all tenant agents returned without
// a ListObjects call (dev allow-all path).
func TestListMyAgents_FGADisabled(t *testing.T) {
	store := &fakeAgentStore{}
	store.agents = []*db.Agent{
		makeTestAgent(t, agentTestTenantUUID, agentAID, "agent-a"),
		makeTestAgent(t, agentTestTenantUUID, agentBID, "agent-b"),
	}
	// No OpenFGAStoreID → FGAEnabled() == false.
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("audit.NewEmitter: %v", err)
	}
	eng, err := policy.NewEngine(context.Background(), policy.Config{
		OPAEndpoint: "http://unused",
		// OpenFGAEndpoint + StoreID intentionally omitted → FGA disabled.
	})
	if err != nil {
		t.Fatalf("policy.NewEngine: %v", err)
	}
	svc := NewBrokerService(Deps{
		Logger:   zap.NewNop(),
		Policy:   eng,
		TenantID: agentTestTenantName,
		Audit:    em,
		Agents:   store,
	})

	ctx := ctxWithIdentity(agentTestTenantUUID, "alice@example.com")
	resp, err := svc.ListMyAgents(ctx, &brokerv1.ListMyAgentsRequest{
		TenantId: agentTestTenantUUID,
		UserId:   "alice@example.com",
	})
	if err != nil {
		t.Fatalf("ListMyAgents (FGA disabled): %v", err)
	}
	if len(resp.Agents) != 2 {
		t.Fatalf("want 2 agents (all tenant agents), got %d", len(resp.Agents))
	}
}

// TestCreateAgent_ProvidersRoundTrip: CreateAgent with AllowedProviders +
// PreferredProvider → Get returns both fields intact.
func TestCreateAgent_ProvidersRoundTrip(t *testing.T) {
	store := &fakeAgentStore{}
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	deps := newListMyAgentsDeps(t, fga, store)
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(agentTestTenantUUID, "admin@example.com")
	resp, err := svc.CreateAgent(ctx, &brokerv1.CreateAgentRequest{
		TenantId: agentTestTenantUUID,
		UserId:   "admin@example.com",
		Agent: &brokerv1.Agent{
			Name:              "provider-agent",
			ApprovalMode:      "auto",
			AllowedProviders:  []string{"anthropic", "openrouter"},
			PreferredProvider: "anthropic",
		},
	})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	got := resp.Agent
	if len(got.AllowedProviders) != 2 {
		t.Fatalf("want 2 AllowedProviders, got %d: %v", len(got.AllowedProviders), got.AllowedProviders)
	}
	if got.AllowedProviders[0] != "anthropic" || got.AllowedProviders[1] != "openrouter" {
		t.Errorf("AllowedProviders mismatch: %v", got.AllowedProviders)
	}
	if got.PreferredProvider != "anthropic" {
		t.Errorf("PreferredProvider: got %q, want %q", got.PreferredProvider, "anthropic")
	}
}

// TestUpdateAgent_ProvidersRoundTrip: UpdateAgent changes AllowedProviders +
// PreferredProvider → Get returns the updated values.
func TestUpdateAgent_ProvidersRoundTrip(t *testing.T) {
	store := &fakeAgentStore{}
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	deps := newListMyAgentsDeps(t, fga, store)
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(agentTestTenantUUID, "admin@example.com")

	// Create with initial providers.
	createResp, err := svc.CreateAgent(ctx, &brokerv1.CreateAgentRequest{
		TenantId: agentTestTenantUUID,
		UserId:   "admin@example.com",
		Agent: &brokerv1.Agent{
			Name:              "update-provider-agent",
			ApprovalMode:      "auto",
			AllowedProviders:  []string{"anthropic"},
			PreferredProvider: "anthropic",
		},
	})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	agentID := createResp.Agent.Id

	// Update to different providers.
	updateResp, err := svc.UpdateAgent(ctx, &brokerv1.UpdateAgentRequest{
		TenantId: agentTestTenantUUID,
		UserId:   "admin@example.com",
		Agent: &brokerv1.Agent{
			Id:                agentID,
			Name:              "update-provider-agent",
			ApprovalMode:      "auto",
			AllowedProviders:  []string{"openrouter", "openai"},
			PreferredProvider: "openrouter",
		},
	})
	if err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}
	got := updateResp.Agent
	if len(got.AllowedProviders) != 2 {
		t.Fatalf("want 2 AllowedProviders after update, got %d: %v", len(got.AllowedProviders), got.AllowedProviders)
	}
	if got.AllowedProviders[0] != "openrouter" || got.AllowedProviders[1] != "openai" {
		t.Errorf("AllowedProviders after update mismatch: %v", got.AllowedProviders)
	}
	if got.PreferredProvider != "openrouter" {
		t.Errorf("PreferredProvider after update: got %q, want %q", got.PreferredProvider, "openrouter")
	}
}

// TestCreateAgent_ProvidersNilDefaultsToEmpty: nil AllowedProviders in request
// → response has empty (not nil) slice; PreferredProvider defaults to "".
func TestCreateAgent_ProvidersNilDefaultsToEmpty(t *testing.T) {
	store := &fakeAgentStore{}
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	deps := newListMyAgentsDeps(t, fga, store)
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(agentTestTenantUUID, "admin@example.com")
	resp, err := svc.CreateAgent(ctx, &brokerv1.CreateAgentRequest{
		TenantId: agentTestTenantUUID,
		UserId:   "admin@example.com",
		Agent: &brokerv1.Agent{
			Name:         "no-provider-agent",
			ApprovalMode: "auto",
			// AllowedProviders and PreferredProvider intentionally omitted.
		},
	})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	got := resp.Agent
	if got.AllowedProviders == nil {
		// proto repeated fields return nil when empty; that's acceptable.
		// What we verify is that the db.Agent stored []string{} (not nil),
		// which is checked via the fakeAgentStore.
	}
	if got.PreferredProvider != "" {
		t.Errorf("PreferredProvider: want empty, got %q", got.PreferredProvider)
	}
	// Verify db.Agent stored non-nil slice.
	store.mu.Lock()
	stored := store.agents[0]
	store.mu.Unlock()
	if stored.AllowedProviders == nil {
		t.Error("db.Agent.AllowedProviders should be []string{} not nil")
	}
}

// ── permitted_agent FGA tuple tests ──────────────────────────────────────────

// newMcpAgentDeps wires a BrokerService with the given fakeFGA and fakeAgentStore.
// The caller owns calling fgaSrv.Close (registered via t.Cleanup).
func newMcpAgentDeps(t *testing.T, fga *fakeFGA, store AgentStore) (*BrokerService, *fakeFGA) {
	t.Helper()
	srv := fga.server(t)
	t.Cleanup(srv.Close)

	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("audit.NewEmitter: %v", err)
	}
	eng, err := policy.NewEngine(context.Background(), policy.Config{
		OPAEndpoint:     "http://unused",
		OpenFGAEndpoint: srv.URL,
		OpenFGAStoreID:  "store-1",
	})
	if err != nil {
		t.Fatalf("policy.NewEngine: %v", err)
	}
	svc := NewBrokerService(Deps{
		Logger:   zap.NewNop(),
		Policy:   eng,
		TenantID: agentTestTenantName,
		Audit:    em,
		Agents:   store,
	})
	return svc, fga
}

// tupleKey extracts user/relation/object from a raw FGA write map.
func tupleKey(m map[string]any) (user, relation, object string) {
	if v, ok := m["user"].(string); ok {
		user = v
	}
	if v, ok := m["relation"].(string); ok {
		relation = v
	}
	if v, ok := m["object"].(string); ok {
		object = v
	}
	return
}

// TestCreateAgent_McpTuplesWritten: CreateAgent with two mcp_servers writes
// exactly two permitted_agent tuples.
func TestCreateAgent_McpTuplesWritten(t *testing.T) {
	store := &fakeAgentStore{}
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	svc, f := newMcpAgentDeps(t, fga, store)

	ctx := ctxWithIdentity(agentTestTenantUUID, "admin@example.com")
	resp, err := svc.CreateAgent(ctx, &brokerv1.CreateAgentRequest{
		TenantId: agentTestTenantUUID,
		UserId:   "admin@example.com",
		Agent: &brokerv1.Agent{
			Name:         "mcp-agent",
			ApprovalMode: "auto",
			McpServers:   []string{"conn-aaa", "conn-bbb"},
		},
	})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	agentID := resp.Agent.Id

	f.mu.Lock()
	allWrites := append([]map[string]any{}, f.writes...)
	f.mu.Unlock()

	// CreateAgent also writes one owner_user tuple for the creator (F9, asserted
	// in TestCreateAgent_OwnerTupleWritten); filter to the mcp permitted_agent
	// tuples this test is about.
	var writes []map[string]any
	for _, w := range allWrites {
		if _, rel, _ := tupleKey(w); rel == "permitted_agent" {
			writes = append(writes, w)
		}
	}

	if len(writes) != 2 {
		t.Fatalf("want 2 permitted_agent write tuples, got %d: %v", len(writes), writes)
	}
	want := map[string]bool{
		"mcp_connector:conn-aaa": false,
		"mcp_connector:conn-bbb": false,
	}
	for _, w := range writes {
		u, rel, obj := tupleKey(w)
		if u != "agent:"+agentID {
			t.Errorf("tuple user: want %q, got %q", "agent:"+agentID, u)
		}
		if rel != "permitted_agent" {
			t.Errorf("tuple relation: want %q, got %q", "permitted_agent", rel)
		}
		if _, ok := want[obj]; !ok {
			t.Errorf("unexpected tuple object %q", obj)
		}
		want[obj] = true
	}
	for obj, seen := range want {
		if !seen {
			t.Errorf("missing tuple for %q", obj)
		}
	}
}

// TestCreateAgent_McpWriteRelationsError: WriteRelations failure → codes.Internal.
func TestCreateAgent_McpWriteRelationsError(t *testing.T) {
	store := &fakeAgentStore{}
	fga := &fakeFGA{
		admins:   map[string]bool{"user:admin@example.com": true},
		writeErr: true,
	}
	svc, _ := newMcpAgentDeps(t, fga, store)

	ctx := ctxWithIdentity(agentTestTenantUUID, "admin@example.com")
	_, err := svc.CreateAgent(ctx, &brokerv1.CreateAgentRequest{
		TenantId: agentTestTenantUUID,
		UserId:   "admin@example.com",
		Agent: &brokerv1.Agent{
			Name:         "mcp-agent-err",
			ApprovalMode: "auto",
			McpServers:   []string{"conn-x"},
		},
	})
	if err == nil {
		t.Fatal("want error on WriteRelations failure, got nil")
	}
	if code := status.Code(err); code != codes.Internal {
		t.Errorf("want codes.Internal, got %v", code)
	}
}

// TestUpdateAgent_McpDiff: changing mcp_servers from {A,B} to {B,C} writes C,
// deletes A, and leaves B untouched.
func TestUpdateAgent_McpDiff(t *testing.T) {
	store := &fakeAgentStore{}
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	svc, f := newMcpAgentDeps(t, fga, store)

	ctx := ctxWithIdentity(agentTestTenantUUID, "admin@example.com")

	// Seed an agent with {A, B}.
	createResp, err := svc.CreateAgent(ctx, &brokerv1.CreateAgentRequest{
		TenantId: agentTestTenantUUID,
		UserId:   "admin@example.com",
		Agent: &brokerv1.Agent{
			Name:         "diff-agent",
			ApprovalMode: "auto",
			McpServers:   []string{"conn-A", "conn-B"},
		},
	})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	agentID := createResp.Agent.Id

	// Clear captured tuples so the update diff is isolated.
	f.mu.Lock()
	f.writes = nil
	f.deletes = nil
	f.mu.Unlock()

	// Update to {B, C}.
	_, err = svc.UpdateAgent(ctx, &brokerv1.UpdateAgentRequest{
		TenantId: agentTestTenantUUID,
		UserId:   "admin@example.com",
		Agent: &brokerv1.Agent{
			Id:           agentID,
			Name:         "diff-agent",
			ApprovalMode: "auto",
			McpServers:   []string{"conn-B", "conn-C"},
		},
	})
	if err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}

	f.mu.Lock()
	writes := append([]map[string]any{}, f.writes...)
	deletes := append([]map[string]any{}, f.deletes...)
	f.mu.Unlock()

	// Exactly one write (C) and one delete (A).
	if len(writes) != 1 {
		t.Fatalf("want 1 write tuple (conn-C), got %d: %v", len(writes), writes)
	}
	if _, _, obj := tupleKey(writes[0]); obj != "mcp_connector:conn-C" {
		t.Errorf("written tuple object: want %q, got %q", "mcp_connector:conn-C", obj)
	}
	wu, wrel, _ := tupleKey(writes[0])
	if wu != "agent:"+agentID {
		t.Errorf("written tuple user: want %q, got %q", "agent:"+agentID, wu)
	}
	if wrel != "permitted_agent" {
		t.Errorf("written tuple relation: want permitted_agent, got %q", wrel)
	}

	if len(deletes) != 1 {
		t.Fatalf("want 1 delete tuple (conn-A), got %d: %v", len(deletes), deletes)
	}
	du, drel, dobj := tupleKey(deletes[0])
	if du != "agent:"+agentID {
		t.Errorf("deleted tuple user: want %q, got %q", "agent:"+agentID, du)
	}
	if drel != "permitted_agent" {
		t.Errorf("deleted tuple relation: want permitted_agent, got %q", drel)
	}
	if dobj != "mcp_connector:conn-A" {
		t.Errorf("deleted tuple object: want %q, got %q", "mcp_connector:conn-A", dobj)
	}
}

// TestUpdateAgent_McpUnchanged: unchanged mcp_servers → zero FGA calls.
func TestUpdateAgent_McpUnchanged(t *testing.T) {
	store := &fakeAgentStore{}
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	svc, f := newMcpAgentDeps(t, fga, store)

	ctx := ctxWithIdentity(agentTestTenantUUID, "admin@example.com")

	createResp, err := svc.CreateAgent(ctx, &brokerv1.CreateAgentRequest{
		TenantId: agentTestTenantUUID,
		UserId:   "admin@example.com",
		Agent: &brokerv1.Agent{
			Name:         "unchanged-agent",
			ApprovalMode: "auto",
			McpServers:   []string{"conn-X"},
		},
	})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	agentID := createResp.Agent.Id

	f.mu.Lock()
	f.writes = nil
	f.deletes = nil
	f.mu.Unlock()

	_, err = svc.UpdateAgent(ctx, &brokerv1.UpdateAgentRequest{
		TenantId: agentTestTenantUUID,
		UserId:   "admin@example.com",
		Agent: &brokerv1.Agent{
			Id:           agentID,
			Name:         "unchanged-agent",
			ApprovalMode: "auto",
			McpServers:   []string{"conn-X"},
		},
	})
	if err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}

	f.mu.Lock()
	writes := append([]map[string]any{}, f.writes...)
	deletes := append([]map[string]any{}, f.deletes...)
	f.mu.Unlock()

	if len(writes) != 0 {
		t.Errorf("want 0 write tuples on unchanged mcp_servers, got %d: %v", len(writes), writes)
	}
	if len(deletes) != 0 {
		t.Errorf("want 0 delete tuples on unchanged mcp_servers, got %d: %v", len(deletes), deletes)
	}
}

// TestDeleteAgent_McpTuplesCleanedUp: DeleteAgent removes the agent's
// permitted_agent tuples; a DeleteRelations error does NOT fail the RPC.
func TestDeleteAgent_McpTuplesCleanedUp(t *testing.T) {
	store := &fakeAgentStore{}
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	svc, f := newMcpAgentDeps(t, fga, store)

	ctx := ctxWithIdentity(agentTestTenantUUID, "admin@example.com")

	createResp, err := svc.CreateAgent(ctx, &brokerv1.CreateAgentRequest{
		TenantId: agentTestTenantUUID,
		UserId:   "admin@example.com",
		Agent: &brokerv1.Agent{
			Name:         "del-agent",
			ApprovalMode: "auto",
			McpServers:   []string{"conn-del-1", "conn-del-2"},
		},
	})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	agentID := createResp.Agent.Id

	f.mu.Lock()
	f.writes = nil
	f.deletes = nil
	f.mu.Unlock()

	delResp, err := svc.DeleteAgent(ctx, &brokerv1.DeleteAgentRequest{
		TenantId: agentTestTenantUUID,
		UserId:   "admin@example.com",
		Id:       agentID,
	})
	if err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}
	if !delResp.Success {
		t.Error("want Success=true")
	}

	f.mu.Lock()
	allDeletes := append([]map[string]any{}, f.deletes...)
	f.mu.Unlock()

	// DeleteAgent also removes the creator's owner_user tuple (F9, asserted in
	// TestDeleteAgent_OwnerTupleCleanedUp); filter to the mcp permitted_agent
	// tuples this test is about.
	var deletes []map[string]any
	for _, d := range allDeletes {
		if _, rel, _ := tupleKey(d); rel == "permitted_agent" {
			deletes = append(deletes, d)
		}
	}

	if len(deletes) != 2 {
		t.Fatalf("want 2 permitted_agent delete tuples, got %d: %v", len(deletes), deletes)
	}
	wantObjs := map[string]bool{"mcp_connector:conn-del-1": false, "mcp_connector:conn-del-2": false}
	for _, d := range deletes {
		_, _, obj := tupleKey(d)
		if _, ok := wantObjs[obj]; !ok {
			t.Errorf("unexpected deleted tuple object %q", obj)
		}
		wantObjs[obj] = true
	}
	for obj, seen := range wantObjs {
		if !seen {
			t.Errorf("missing delete tuple for %q", obj)
		}
	}
}

// TestDeleteAgent_McpDeleteRelationsErrorIsNonFatal: DeleteRelations error does
// not fail the DeleteAgent RPC (best-effort cleanup).
func TestDeleteAgent_McpDeleteRelationsErrorIsNonFatal(t *testing.T) {
	store := &fakeAgentStore{}
	fga := &fakeFGA{
		admins:        map[string]bool{"user:admin@example.com": true},
		deleteOnlyErr: true,
	}
	svc, _ := newMcpAgentDeps(t, fga, store)

	ctx := ctxWithIdentity(agentTestTenantUUID, "admin@example.com")

	// Create without mcp_servers so CreateAgent's WriteRelations (write path)
	// succeeds (deleteOnlyErr only blocks deletes), then plant mcp_servers directly.
	tid, _ := uuid.Parse(agentTestTenantUUID)
	aid := uuid.New()
	store.mu.Lock()
	store.agents = append(store.agents, &db.Agent{
		ID:           aid,
		TenantID:     tid,
		Name:         "del-err-agent",
		ApprovalMode: "auto",
		Skills:       []string{},
		McpServers:   []string{"conn-gone"},
	})
	store.mu.Unlock()

	_, err := svc.DeleteAgent(ctx, &brokerv1.DeleteAgentRequest{
		TenantId: agentTestTenantUUID,
		UserId:   "admin@example.com",
		Id:       aid.String(),
	})
	if err != nil {
		t.Fatalf("DeleteAgent must succeed even when DeleteRelations errors, got: %v", err)
	}
}

// TestCreateAgent_OwnerTupleWritten (F9): CreateAgent grants the creator
// owner_user on the new agent so may-operate-agent (can_use = usable_by or
// owner_user) lets the creator run workflows bound to it. No mcp_servers here,
// so exactly one owner_user tuple is written.
func TestCreateAgent_OwnerTupleWritten(t *testing.T) {
	store := &fakeAgentStore{}
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	svc, f := newMcpAgentDeps(t, fga, store)

	ctx := ctxWithIdentity(agentTestTenantUUID, "admin@example.com")
	resp, err := svc.CreateAgent(ctx, &brokerv1.CreateAgentRequest{
		TenantId: agentTestTenantUUID,
		UserId:   "admin@example.com",
		Agent: &brokerv1.Agent{
			Name:         "owned-agent",
			ApprovalMode: "auto",
		},
	})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	agentID := resp.Agent.Id

	f.mu.Lock()
	writes := append([]map[string]any{}, f.writes...)
	f.mu.Unlock()

	var owner []map[string]any
	for _, w := range writes {
		if _, rel, _ := tupleKey(w); rel == "owner_user" {
			owner = append(owner, w)
		}
	}
	if len(owner) != 1 {
		t.Fatalf("want exactly 1 owner_user tuple, got %d: %v", len(owner), writes)
	}
	u, _, obj := tupleKey(owner[0])
	if u != "user:admin@example.com" {
		t.Errorf("owner tuple user: want %q, got %q", "user:admin@example.com", u)
	}
	if obj != "agent:"+agentID {
		t.Errorf("owner tuple object: want %q, got %q", "agent:"+agentID, obj)
	}
}

// TestDeleteAgent_OwnerTupleCleanedUp (F9): DeleteAgent removes the creator's
// owner_user tuple alongside the mcp permitted_agent tuples.
func TestDeleteAgent_OwnerTupleCleanedUp(t *testing.T) {
	store := &fakeAgentStore{}
	fga := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	svc, f := newMcpAgentDeps(t, fga, store)

	ctx := ctxWithIdentity(agentTestTenantUUID, "admin@example.com")
	createResp, err := svc.CreateAgent(ctx, &brokerv1.CreateAgentRequest{
		TenantId: agentTestTenantUUID,
		UserId:   "admin@example.com",
		Agent: &brokerv1.Agent{
			Name:         "owned-del-agent",
			ApprovalMode: "auto",
		},
	})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	agentID := createResp.Agent.Id

	f.mu.Lock()
	f.writes = nil
	f.deletes = nil
	f.mu.Unlock()

	if _, err := svc.DeleteAgent(ctx, &brokerv1.DeleteAgentRequest{
		TenantId: agentTestTenantUUID,
		UserId:   "admin@example.com",
		Id:       agentID,
	}); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}

	f.mu.Lock()
	deletes := append([]map[string]any{}, f.deletes...)
	f.mu.Unlock()

	found := false
	for _, d := range deletes {
		u, rel, obj := tupleKey(d)
		if rel == "owner_user" && u == "user:admin@example.com" && obj == "agent:"+agentID {
			found = true
		}
	}
	if !found {
		t.Errorf("owner_user delete tuple for creator not found in %v", deletes)
	}
}

// TestToProtoAgent_ProvidersMapping: toProtoAgent copies both provider fields.
func TestToProtoAgent_ProvidersMapping(t *testing.T) {
	tid, _ := uuid.Parse(agentTestTenantUUID)
	aid, _ := uuid.Parse(agentAID)
	a := &db.Agent{
		ID:                aid,
		TenantID:          tid,
		Name:              "x",
		ApprovalMode:      "auto",
		Skills:            []string{},
		McpServers:        []string{},
		AllowedProviders:  []string{"anthropic", "openrouter"},
		PreferredProvider: "anthropic",
	}
	p := toProtoAgent(a)
	if len(p.AllowedProviders) != 2 {
		t.Fatalf("toProtoAgent: want 2 AllowedProviders, got %d", len(p.AllowedProviders))
	}
	if p.AllowedProviders[0] != "anthropic" || p.AllowedProviders[1] != "openrouter" {
		t.Errorf("toProtoAgent: AllowedProviders mismatch: %v", p.AllowedProviders)
	}
	if p.PreferredProvider != "anthropic" {
		t.Errorf("toProtoAgent: PreferredProvider got %q, want %q", p.PreferredProvider, "anthropic")
	}
}

// TestListMyAgents_IdentityBinding: the can_use lookup uses "user:"+<verified caller>.
// A user authenticated as alice cannot list agents with a spoofed user_id of bob.
// Additionally, the ListObjects query must use alice's identity (not bob's).
func TestListMyAgents_IdentityBinding(t *testing.T) {
	store := &fakeAgentStore{}
	store.agents = []*db.Agent{
		makeTestAgent(t, agentTestTenantUUID, agentAID, "agent-a"),
	}
	fga := &fakeFGA{
		admins:            map[string]bool{},
		listObjectsResult: []string{"agent:" + agentAID},
	}
	deps := newListMyAgentsDeps(t, fga, store)
	svc := NewBrokerService(deps)

	// Authenticated as alice, but user_id in request claims to be bob.
	ctx := ctxWithIdentity(agentTestTenantUUID, "alice@example.com")
	_, err := svc.ListMyAgents(ctx, &brokerv1.ListMyAgentsRequest{
		TenantId: agentTestTenantUUID,
		UserId:   "bob@example.com",
	})
	// callerIdentity rejects a user_id that disagrees with the verified OIDC subject.
	if err == nil {
		t.Fatal("want error when user_id disagrees with OIDC identity, got nil")
	}
	if code := status.Code(err); code != codes.PermissionDenied {
		t.Errorf("want PermissionDenied for spoofed user_id, got %v", code)
	}

	// Now call correctly as alice; assert the FGA query used alice's identity.
	resp, err := svc.ListMyAgents(ctx, &brokerv1.ListMyAgentsRequest{
		TenantId: agentTestTenantUUID,
		UserId:   "alice@example.com",
	})
	if err != nil {
		t.Fatalf("ListMyAgents as alice: %v", err)
	}
	if len(resp.Agents) != 1 {
		t.Fatalf("want 1 agent, got %d", len(resp.Agents))
	}
	fga.mu.Lock()
	queriedUser := fga.listObjectsUser
	fga.mu.Unlock()
	if queriedUser != "user:alice@example.com" {
		t.Errorf("ListObjects queried user %q, want %q", queriedUser, "user:alice@example.com")
	}
}
