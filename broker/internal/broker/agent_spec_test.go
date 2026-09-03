package broker

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// buildAgentSpecSvc constructs a SandboxService with a fakeAgentStore wired in,
// using testGatewaySpiffeID (defined in mcp_routing_test.go).
func buildAgentSpecSvc(t *testing.T, store AgentStore) *SandboxService {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("audit.NewEmitter: %v", err)
	}
	return NewSandboxService(Deps{
		Logger:          zap.NewNop(),
		Audit:           em,
		TenantID:        "aikonos-dev",
		GatewaySpiffeID: testGatewaySpiffeID,
		Agents:          store,
	})
}

// TestGetAgentSpec_Found: agent exists → found=true with fields mapped.
func TestGetAgentSpec_Found(t *testing.T) {
	tid := uuid.MustParse(testTenantUUID)
	aid := uuid.MustParse(agentAID)
	store := &fakeAgentStore{
		agents: []*db.Agent{{
			ID:           aid,
			TenantID:     tid,
			Name:         "my-agent",
			LLMModel:     "anthropic/claude-sonnet-4.6",
			ApprovalMode: "needs_approval",
			Skills:       []string{"web.fetch"},
		}},
	}
	svc := buildAgentSpecSvc(t, store)
	ctx := gatewayCtx(testGatewaySpiffeID)

	resp, err := svc.GetAgentSpec(ctx, &brokerv1.GetAgentSpecRequest{
		TenantId: testTenantUUID,
		AgentId:  agentAID,
	})
	if err != nil {
		t.Fatalf("GetAgentSpec: %v", err)
	}
	if !resp.Found {
		t.Fatal("want found=true, got false")
	}
	if resp.Name != "my-agent" {
		t.Errorf("name = %q, want %q", resp.Name, "my-agent")
	}
	if resp.LlmModel != "anthropic/claude-sonnet-4.6" {
		t.Errorf("llm_model = %q, want %q", resp.LlmModel, "anthropic/claude-sonnet-4.6")
	}
	if resp.ApprovalMode != "needs_approval" {
		t.Errorf("approval_mode = %q, want %q", resp.ApprovalMode, "needs_approval")
	}
	if len(resp.Skills) != 1 || resp.Skills[0] != "web.fetch" {
		t.Errorf("skills = %v, want [web.fetch]", resp.Skills)
	}
	if resp.GatewayEnabled {
		t.Error("gateway_enabled = true, want false (default)")
	}
}

// TestGetAgentSpec_GatewayEnabled: gateway_enabled=true is mapped through.
func TestGetAgentSpec_GatewayEnabled(t *testing.T) {
	tid := uuid.MustParse(testTenantUUID)
	aid := uuid.MustParse(agentAID)
	store := &fakeAgentStore{
		agents: []*db.Agent{{
			ID:             aid,
			TenantID:       tid,
			Name:           "ext-agent",
			ApprovalMode:   "auto",
			GatewayEnabled: true,
		}},
	}
	svc := buildAgentSpecSvc(t, store)
	ctx := gatewayCtx(testGatewaySpiffeID)

	resp, err := svc.GetAgentSpec(ctx, &brokerv1.GetAgentSpecRequest{
		TenantId: testTenantUUID,
		AgentId:  agentAID,
	})
	if err != nil {
		t.Fatalf("GetAgentSpec: %v", err)
	}
	if !resp.GatewayEnabled {
		t.Error("gateway_enabled = false, want true")
	}
}

// TestGetAgentSpec_NotFound: unknown id → found=false, no error.
func TestGetAgentSpec_NotFound(t *testing.T) {
	store := &fakeAgentStore{}
	svc := buildAgentSpecSvc(t, store)
	ctx := gatewayCtx(testGatewaySpiffeID)

	resp, err := svc.GetAgentSpec(ctx, &brokerv1.GetAgentSpecRequest{
		TenantId: testTenantUUID,
		AgentId:  agentAID,
	})
	if err != nil {
		t.Fatalf("GetAgentSpec: %v", err)
	}
	if resp.Found {
		t.Fatal("want found=false for unknown agent, got true")
	}
}

// TestGetAgentSpec_AgentsNil: Agents dep nil → found=false (feature disabled graceful fallback).
func TestGetAgentSpec_AgentsNil(t *testing.T) {
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("audit.NewEmitter: %v", err)
	}
	svc := NewSandboxService(Deps{
		Logger:          zap.NewNop(),
		Audit:           em,
		TenantID:        "aikonos-dev",
		GatewaySpiffeID: testGatewaySpiffeID,
		// Agents intentionally nil.
	})
	ctx := gatewayCtx(testGatewaySpiffeID)

	resp, err := svc.GetAgentSpec(ctx, &brokerv1.GetAgentSpecRequest{
		TenantId: testTenantUUID,
		AgentId:  agentAID,
	})
	if err != nil {
		t.Fatalf("GetAgentSpec with nil Agents: %v", err)
	}
	if resp.Found {
		t.Fatal("want found=false when Agents is nil, got true")
	}
}

// TestGetAgentSpec_NonUUIDAgentID: a syntactically non-uuid agent_id (e.g. a
// synthetic id like "alice-agent") must fail fast with Found=false (this
// handler's documented not-found shape, nil error) at the handler boundary,
// never reaching the store/DB (which would otherwise 500 with a raw 22P02
// "invalid input syntax for type uuid" from Postgres).
func TestGetAgentSpec_NonUUIDAgentID(t *testing.T) {
	store := &explodingAgentStore{t: t}
	svc := buildAgentSpecSvc(t, store)
	ctx := gatewayCtx(testGatewaySpiffeID)

	resp, err := svc.GetAgentSpec(ctx, &brokerv1.GetAgentSpecRequest{
		TenantId: testTenantUUID,
		AgentId:  "alice-agent",
	})
	if err != nil {
		t.Fatalf("non-uuid agent_id: want nil error, got %v", err)
	}
	if resp.Found {
		t.Fatal("non-uuid agent_id: want Found=false, got true")
	}
}

// explodingAgentStore fails the test if Get is ever called — used to prove a
// non-uuid agent_id is rejected before reaching the store.
type explodingAgentStore struct {
	t *testing.T
}

func (e *explodingAgentStore) Create(context.Context, *db.Agent) error { return nil }
func (e *explodingAgentStore) List(context.Context, string) ([]*db.Agent, error) {
	return nil, nil
}
func (e *explodingAgentStore) Get(context.Context, string, string) (*db.Agent, error) {
	e.t.Fatal("Get must not be called for a non-uuid agent_id")
	return nil, nil
}
func (e *explodingAgentStore) Update(context.Context, *db.Agent) error       { return nil }
func (e *explodingAgentStore) Delete(context.Context, string, string) error { return nil }
func (e *explodingAgentStore) SetSoul(context.Context, string, string, string) error {
	return nil
}

// TestGetAgentSpec_NonGatewayPeer: non-gateway SPIFFE ID → PermissionDenied.
func TestGetAgentSpec_NonGatewayPeer(t *testing.T) {
	svc := buildAgentSpecSvc(t, &fakeAgentStore{})
	ctx := gatewayCtx("spiffe://aikonos.com/some-sandbox")

	_, err := svc.GetAgentSpec(ctx, &brokerv1.GetAgentSpecRequest{
		TenantId: testTenantUUID,
		AgentId:  agentAID,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-gateway peer: want PermissionDenied, got %v", err)
	}
}
