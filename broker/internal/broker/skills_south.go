package broker

// skills_south.go — SPIFFE-gated south RPC for the agent-gateway.
// ListUserSkills returns the tools the given user may invoke:
//   - FGA disabled: the full named registry (baseline + overlay).
//   - FGA enabled: per-tool CheckFGA(user:<id>, can_invoke, skill:<toolId>);
//     tools where the check fails or errors are silently omitted (fail-closed
//     per-tool, not fail-closed for the whole request — the goal is to return
//     the subset the user actually holds, not to abort the session build).

import (
	"context"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ListUserSkills is the SPIFFE-south variant of the skill listing surface.
// The gateway calls this during session build to discover which tools to
// expose to Pi for the requesting user.
func (s *SandboxService) ListUserSkills(ctx context.Context, req *brokerv1.ListUserSkillsRequest) (*brokerv1.ListUserSkillsResponse, error) {
	if err := s.requireGatewayPeer(ctx); err != nil {
		return nil, err
	}
	if req.TenantId == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id required")
	}
	if req.UserId == "" {
		return nil, status.Error(codes.InvalidArgument, "user_id required")
	}

	reg := s.deps.ToolRegistry
	if reg == nil {
		reg = toolregistry.PkgDefault()
	}

	all := reg.ListTools()
	sp := s.skillPolicyFor()
	fgaOn := sp != nil && sp.FGAEnabled()

	out := make([]*brokerv1.SkillItem, 0, len(all))
	userRef := "user:" + req.UserId
	for _, e := range all {
		if fgaOn {
			granted, err := sp.CheckFGA(ctx, userRef, "can_invoke", "skill:"+e.ToolID)
			if err != nil {
				s.deps.Logger.Warn("ListUserSkills: CheckFGA error — excluding tool",
					zap.String("user", userRef), zap.String("tool", e.ToolID), zap.Error(err))
				continue
			}
			if !granted {
				continue
			}
		}
		out = append(out, &brokerv1.SkillItem{
			ToolId: e.ToolID,
			Scope:  e.Scope,
		})
	}

	// Capability skills (e.g. "scheduler", "workflows") are FGA-gated authz
	// constructs that live outside the tool registry. Apply the same gate logic:
	// FGA on → CheckFGA per skill, fail-closed per item; FGA off → include all.
	for _, id := range capabilitySkills {
		if fgaOn {
			granted, err := sp.CheckFGA(ctx, userRef, "can_invoke", "skill:"+id)
			if err != nil {
				s.deps.Logger.Warn("ListUserSkills: CheckFGA error — excluding tool",
					zap.String("user", userRef), zap.String("tool", id), zap.Error(err))
				continue
			}
			if !granted {
				continue
			}
		}
		out = append(out, &brokerv1.SkillItem{ToolId: id, Scope: ""})
	}

	return &brokerv1.ListUserSkillsResponse{Skills: out}, nil
}
