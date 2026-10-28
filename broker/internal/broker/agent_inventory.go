package broker

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ExportAgentInventory lists every agent with its owners and credential state
// for identity-governance tools (AG-14). Tenant-admin gated through
// listAgents; owners are read strictly. The soul (system prompt) is left out:
// the export is meant to leave aikonOS.
// ponytail: two key reads per agent; one tenant-wide query when tenants hold
// thousands of agents.
func (s *BrokerService) ExportAgentInventory(ctx context.Context, req *brokerv1.ExportAgentInventoryRequest) (*brokerv1.ExportAgentInventoryResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.agents.inventory")
	defer span.End()

	tenant, user, agents, err := s.listAgents(ctx, req.TenantId, req.UserId, true)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]*brokerv1.AgentInventoryEntry, 0, len(agents))
	for _, a := range agents {
		a.Soul = ""
		e := &brokerv1.AgentInventoryEntry{Agent: a}
		var nearest, last time.Time
		note := func(expires time.Time, used *time.Time) bool {
			if used != nil && used.After(last) {
				last = *used
			}
			if !expires.After(now) {
				return false
			}
			if nearest.IsZero() || expires.Before(nearest) {
				nearest = expires
			}
			return true
		}
		if s.deps.ApiKeys != nil {
			keys, err := s.deps.ApiKeys.ListByAgent(ctx, tenant, a.Id)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "list api keys: %v", err)
			}
			for _, k := range keys {
				if note(k.ExpiresAt, k.LastUsedAt) {
					e.ApiKeys++
				}
			}
		}
		if s.deps.PublicKeys != nil {
			keys, err := s.deps.PublicKeys.ListByAgent(ctx, tenant, a.Id)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "list public keys: %v", err)
			}
			for _, k := range keys {
				if note(k.ExpiresAt, k.LastUsedAt) {
					e.PublicKeys++
				}
			}
		}
		if !nearest.IsZero() {
			e.NearestKeyExpiry = timestamppb.New(nearest)
		}
		if !last.IsZero() {
			e.LastUsedAt = timestamppb.New(last)
		}
		out = append(out, e)
	}
	s.emitAdminAudit(ctx, "", tenant, user, "aikonos.broker.agent.inventory.exported", nil, auditv1.PolicyDecision_ALLOW)
	return &brokerv1.ExportAgentInventoryResponse{Agents: out}, nil
}
