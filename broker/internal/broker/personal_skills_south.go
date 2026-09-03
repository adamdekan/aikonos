package broker

// personal_skills_south.go — ListPersonalSkillsSouth / GetPersonalSkillSouth,
// the SPIFFE-gated session-build twins the gateway calls to union a user's
// own Skills/<name>/ folders into the catalog.
//
// Mirrors memory_south.go's posture exactly: requireGatewayPeer first, then
// CheckFGA(user, can_invoke, skill:personal-skills) — ungranted or an FGA
// error yields an empty list / NotFound get, never a transport error, so a
// withdrawn grant or an OpenFGA hiccup cannot fail a chat turn. Both RPCs also
// reject a svc-/group- prefixed user_id: personal skills have no agent/group
// concept, unlike memory's three-scope model.

import (
	"context"
	"strings"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/personalskill"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// personalSkillsSkill is the FGA skill id every personal-skill verb
// chokepoints on.
const personalSkillsSkill = "personal-skills"

// isSyntheticUser reports whether user_id is a svc-/group- synthetic subject
// (agent-bound run or group segment, mirroring memory_rpcs.go's
// memoryAgentSegPrefix/memoryGroupSegPrefix) — personal skills belong to a
// real user only, so both south RPCs reject these before touching the
// workspace.
func isSyntheticUser(user string) bool {
	return strings.HasPrefix(user, "svc-") || strings.HasPrefix(user, "group-")
}

// personalSkillsGranted reports whether user holds skill:personal-skills,
// failing closed (false) on a nil Workspace/Policy or an FGA error — the
// empty-not-error posture memory_south.go established.
func (s *SandboxService) personalSkillsGranted(ctx context.Context, user string) bool {
	if s.deps.Workspace.Local == nil || s.deps.Policy == nil {
		return false
	}
	ok, err := s.deps.Policy.CheckFGA(ctx, "user:"+user, "can_invoke", "skill:"+personalSkillsSkill)
	return err == nil && ok
}

// ListPersonalSkillsSouth returns the caller's own Skills/*/SKILL.md catalog
// rows (frontmatter only — no bodies), capped at personalskill.MaxSkills.
func (s *SandboxService) ListPersonalSkillsSouth(ctx context.Context, req *brokerv1.ListPersonalSkillsSouthRequest) (*brokerv1.ListPersonalSkillsSouthResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.personalskill.list_south")
	defer span.End()

	if err := s.requireGatewayPeer(ctx); err != nil {
		return nil, err
	}
	tenant, user := req.GetTenantId(), req.GetUserId()
	if tenant == "" || user == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id and user_id required")
	}

	out := &brokerv1.ListPersonalSkillsSouthResponse{}
	if isSyntheticUser(user) || !s.personalSkillsGranted(ctx, user) {
		return out, nil
	}

	entries, err := personalskill.Scan(ctx, s.deps.Workspace.Local, tenant, user)
	if err != nil {
		s.deps.Logger.Warn("ListPersonalSkillsSouth: scan failed, returning empty",
			zap.String("user", user), zap.Error(err))
		return out, nil
	}
	for _, e := range entries {
		out.Skills = append(out.Skills, &brokerv1.PersonalSkillEntry{
			Name:                   e.Name,
			Description:            e.Description,
			Keywords:               e.Keywords,
			AllowedTools:           e.AllowedTools,
			DisableModelInvocation: e.DisableModelInvocation,
			Valid:                  e.Valid,
			Warning:                e.Warning,
		})
	}
	return out, nil
}

// GetPersonalSkillSouth fetches one skill's body for on-demand load_skill
// activation. NotFound for a missing/invalid skill, an ungranted user, an FGA
// error, or a svc-/group- prefixed user_id — never a transport error
// distinguishing those cases, so a caller cannot probe grant state through
// error shape.
func (s *SandboxService) GetPersonalSkillSouth(ctx context.Context, req *brokerv1.GetPersonalSkillSouthRequest) (*brokerv1.GetPersonalSkillSouthResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.personalskill.get_south")
	defer span.End()

	if err := s.requireGatewayPeer(ctx); err != nil {
		return nil, err
	}
	notFound := status.Error(codes.NotFound, "personal skill not found")

	tenant, user, name := req.GetTenantId(), req.GetUserId(), req.GetName()
	if tenant == "" || user == "" || name == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id, user_id, and name required")
	}
	if isSyntheticUser(user) || !s.personalSkillsGranted(ctx, user) {
		return nil, notFound
	}
	if !personalskill.ValidSkillName(name) {
		return nil, notFound
	}

	data, _, err := s.deps.Workspace.Local.Read(ctx, tenant, user, personalskill.SkillMdPath(name))
	if err != nil {
		return nil, notFound
	}
	if len(data) > personalskill.MaxSkillMdBytes {
		return nil, notFound
	}
	fm, err := personalskill.ParseFrontmatter(data)
	if err != nil || fm.Description == "" {
		return nil, notFound
	}
	body, err := personalskill.SplitBody(data)
	if err != nil {
		// Defensive, not an expected path: ParseFrontmatter above already
		// validated the fence.
		return nil, notFound
	}
	filePaths, err := personalskill.ListFiles(ctx, s.deps.Workspace.Local, tenant, user, name)
	if err != nil {
		// Never fail the whole Get over a listing hiccup — the body above
		// already resolved successfully; degrade to no manifest.
		s.deps.Logger.Warn("GetPersonalSkillSouth: list files failed, returning no file_paths",
			zap.String("user", user), zap.String("name", name), zap.Error(err))
		filePaths = nil
	}
	return &brokerv1.GetPersonalSkillSouthResponse{Body: body, AllowedTools: fm.AllowedTools, FilePaths: filePaths}, nil
}

// GetPersonalSkillFileSouth reads one file out of a personal skill's full
// tree for the read_skill_file Pi tool. Same
// svc-/group- rejection and skill:personal-skills gating posture as
// GetPersonalSkillSouth. NotFound for a missing skill, an ungranted user, or
// an unknown/invalid path; InvalidArgument (naming the cap) for a file over
// personalskill.MaxSkillFileBytes — distinct from NotFound since the file does
// exist, it is simply too large to return.
func (s *SandboxService) GetPersonalSkillFileSouth(ctx context.Context, req *brokerv1.GetPersonalSkillFileSouthRequest) (*brokerv1.GetPersonalSkillFileSouthResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.personalskill.get_file_south")
	defer span.End()

	if err := s.requireGatewayPeer(ctx); err != nil {
		return nil, err
	}
	notFound := status.Error(codes.NotFound, "personal skill file not found")

	tenant, user, name, relPath := req.GetTenantId(), req.GetUserId(), req.GetName(), req.GetPath()
	if tenant == "" || user == "" || name == "" || relPath == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id, user_id, name, and path required")
	}
	if isSyntheticUser(user) || !s.personalSkillsGranted(ctx, user) {
		return nil, notFound
	}
	if !personalskill.ValidSkillName(name) || !validSkillFilePath(relPath) {
		return nil, notFound
	}

	data, _, err := s.deps.Workspace.Local.Read(ctx, tenant, user, personalskill.SkillDir(name)+"/"+relPath)
	if err != nil {
		return nil, notFound
	}
	if len(data) > personalskill.MaxSkillFileBytes {
		return nil, status.Errorf(codes.InvalidArgument,
			"file %q is %d bytes, exceeds the %d byte cap", relPath, len(data), personalskill.MaxSkillFileBytes)
	}
	return &brokerv1.GetPersonalSkillFileSouthResponse{Content: data}, nil
}
