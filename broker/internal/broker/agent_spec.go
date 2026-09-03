package broker

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/db"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// GetAgentSpec returns the config of a named agent for the gateway to consume
// when building an agent-scoped Pi session. SPIFFE-gated to the agent-gateway
// identity (same posture as GetTenantModel). Returns found=false rather than an
// error when the agent is unknown so the gateway can degrade gracefully.
func (s *SandboxService) GetAgentSpec(ctx context.Context, req *brokerv1.GetAgentSpecRequest) (*brokerv1.GetAgentSpecResponse, error) {
	if err := s.requireGatewayPeer(ctx); err != nil {
		return nil, err
	}
	if req.TenantId == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id required")
	}
	if req.AgentId == "" {
		return nil, status.Error(codes.InvalidArgument, "agent_id required")
	}
	// agent_id backs a uuid DB column; a syntactically invalid id (e.g. a
	// synthetic id like "alice-agent") must be rejected here as not-found
	// rather than reaching Postgres, which would 22P02 into codes.Internal.
	// Found=false (not an error) matches this handler's documented not-found
	// shape — the gateway's namedAgentId caller checks .found with no
	// try/catch.
	if _, err := uuid.Parse(req.AgentId); err != nil {
		return &brokerv1.GetAgentSpecResponse{Found: false}, nil
	}

	// Agents feature disabled → treat as not found so the gateway falls back to
	// default session behaviour rather than hard-erroring.
	if s.deps.Agents == nil {
		return &brokerv1.GetAgentSpecResponse{Found: false}, nil
	}

	a, err := s.deps.Agents.Get(ctx, req.TenantId, req.AgentId)
	if err != nil {
		if errors.Is(err, db.ErrAgentNotFound) {
			return &brokerv1.GetAgentSpecResponse{Found: false}, nil
		}
		return nil, status.Errorf(codes.Internal, "get agent: %v", err)
	}

	return &brokerv1.GetAgentSpecResponse{
		Found:          true,
		Name:           a.Name,
		LlmModel:       a.LLMModel,
		ApprovalMode:   a.ApprovalMode,
		Skills:         a.Skills,
		Soul:           a.Soul,
		GatewayEnabled: a.GatewayEnabled,
	}, nil
}
