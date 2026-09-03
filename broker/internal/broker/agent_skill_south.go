package broker

// agent_skill_south.go — SPIFFE-gated south RPC for the agent-gateway.
// ListUserAgentSkills returns the SKILL.md bundles the given user may use:
//   - FGA disabled: all tenant bundles (no per-item gate).
//   - FGA enabled: per-bundle CheckFGA(user:<id>, can_use, agentskill:<id>);
//     bundles where the check fails or errors are silently omitted (fail-closed
//     per-item, not fail-closed for the whole request — the goal is to return
//     the subset the user actually holds, not to abort the session build).
//
// Note: disable_model_invocation is propagated as-is; the catalog-vs-/command
// filtering that excludes DMI=true bundles from the auto-catalog happens
// gateway-side in CP7, not here.

import (
	"context"
	"encoding/base64"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ListUserAgentSkills is the SPIFFE-south variant of the agent-skill-bundle
// listing surface. The gateway calls this during session build to discover which
// SKILL.md bundles to offer to the user (via the load_skill catalog or /command
// palette).
func (s *SandboxService) ListUserAgentSkills(ctx context.Context, req *brokerv1.ListUserAgentSkillsRequest) (*brokerv1.ListUserAgentSkillsResponse, error) {
	if err := s.requireGatewayPeer(ctx); err != nil {
		return nil, err
	}
	if req.TenantId == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id required")
	}
	if req.UserId == "" {
		return nil, status.Error(codes.InvalidArgument, "user_id required")
	}

	repo := s.deps.AgentSkillBundles
	if repo == nil {
		return nil, status.Error(codes.FailedPrecondition, "agent skill bundle store not configured")
	}

	rows, err := repo.ListByTenant(ctx, req.TenantId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list agent_skill bundles: %v", err)
	}

	sp := s.skillPolicyFor()
	fgaOn := sp != nil && sp.FGAEnabled()

	out := make([]*brokerv1.AgentSkillBundle, 0, len(rows))
	userRef := "user:" + req.UserId
	for _, row := range rows {
		if fgaOn {
			granted, err := sp.CheckFGA(ctx, userRef, "can_use", "agentskill:"+row.ID.String())
			if err != nil {
				s.deps.Logger.Warn("ListUserAgentSkills: CheckFGA error — excluding bundle",
					zap.String("user", userRef), zap.String("bundle", row.ID.String()), zap.Error(err))
				continue
			}
			if !granted {
				continue
			}
		}
		out = append(out, agentSkillToProto(row))
	}

	return &brokerv1.ListUserAgentSkillsResponse{Bundles: out}, nil
}

// GetAgentSkillFileSouth reads one file out of an agent skill bundle's full
// tree for the read_skill_file Pi tool.
// Re-checks the same per-bundle CheckFGA(user, can_use, agentskill:<id>) gate
// ListUserAgentSkills applies, then validates path and looks it up in the
// bundle's extras JSONB. NotFound for an unknown bundle id, an ungranted
// user, or an unknown/invalid path — never a transport error distinguishing
// those cases, mirroring personal_skills_south.go's posture.
func (s *SandboxService) GetAgentSkillFileSouth(ctx context.Context, req *brokerv1.GetAgentSkillFileSouthRequest) (*brokerv1.GetAgentSkillFileSouthResponse, error) {
	if err := s.requireGatewayPeer(ctx); err != nil {
		return nil, err
	}
	if req.TenantId == "" || req.UserId == "" || req.Id == "" || req.Path == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id, user_id, id, and path required")
	}
	notFound := status.Error(codes.NotFound, "agent skill file not found")
	if !validSkillFilePath(req.Path) {
		return nil, notFound
	}

	repo := s.deps.AgentSkillBundles
	if repo == nil {
		return nil, status.Error(codes.FailedPrecondition, "agent skill bundle store not configured")
	}
	id, err := uuid.Parse(req.Id)
	if err != nil {
		return nil, notFound
	}
	row, err := repo.Get(ctx, req.TenantId, id)
	if err != nil {
		return nil, notFound
	}

	sp := s.skillPolicyFor()
	if sp != nil && sp.FGAEnabled() {
		granted, ferr := sp.CheckFGA(ctx, "user:"+req.UserId, "can_use", "agentskill:"+row.ID.String())
		if ferr != nil {
			s.deps.Logger.Warn("GetAgentSkillFileSouth: CheckFGA error — treating as not found",
				zap.String("user", req.UserId), zap.String("bundle", row.ID.String()), zap.Error(ferr))
			return nil, notFound
		}
		if !granted {
			return nil, notFound
		}
	}

	b64, ok := agentSkillExtras(row.Extras)[req.Path]
	if !ok {
		return nil, notFound
	}
	content, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, notFound
	}
	return &brokerv1.GetAgentSkillFileSouthResponse{Content: content}, nil
}
