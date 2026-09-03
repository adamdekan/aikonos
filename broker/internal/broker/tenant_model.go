package broker

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// GetTenantModel returns the tenant's configured LLM model override.
// SPIFFE-gated to the agent-gateway so arbitrary south callers cannot probe
// tenant config. Returns an empty model string when no override is set —
// the gateway falls back to its env default.
// Convenience-only: this value never affects any gate, scope, or approval.
func (s *SandboxService) GetTenantModel(ctx context.Context, req *brokerv1.GetTenantModelRequest) (*brokerv1.GetTenantModelResponse, error) {
	if err := s.requireGatewayPeer(ctx); err != nil {
		return nil, err
	}
	if req.TenantId == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id required")
	}
	model := s.cfg().GetString(ctx, req.TenantId, "llm_model")
	return &brokerv1.GetTenantModelResponse{Model: model}, nil
}
