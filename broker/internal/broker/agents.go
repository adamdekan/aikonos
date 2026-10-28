package broker

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/adamdekan/aikonos/broker/internal/config"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// htmlTagRE matches any HTML tag; compiled once at package init.
var htmlTagRE = regexp.MustCompile(`<[^>]*>`)

const maxSoulBytes = 4096

// sanitizeSoul strips HTML tags, trims whitespace, and rejects inputs that
// exceed maxSoulBytes after cleaning. Empty is valid (clears the SOUL).
func sanitizeSoul(raw string) (string, error) {
	cleaned := strings.TrimSpace(htmlTagRE.ReplaceAllString(raw, ""))
	if len([]byte(cleaned)) > maxSoulBytes {
		return "", status.Errorf(codes.InvalidArgument, "soul exceeds %d bytes", maxSoulBytes)
	}
	return cleaned, nil
}

// requireAgentOwnerOrAdmin allows the caller iff they are a tenant admin OR
// an owner of agent:<agentID> (owner_user, or a member of an owner_group).
// Fails closed on any FGA transport error.
func (s *BrokerService) requireAgentOwnerOrAdmin(ctx context.Context, user, agentID string) error {
	isAdmin, err := s.deps.Policy.CheckFGA(ctx, "user:"+user, "admin", s.fgaTenantObject())
	if err != nil {
		return status.Error(codes.Internal, "authorization check failed")
	}
	if isAdmin {
		return nil
	}
	isOwner, err := s.deps.Policy.CheckFGA(ctx, "user:"+user, "owner", "agent:"+agentID)
	if err != nil {
		return status.Error(codes.Internal, "authorization check failed")
	}
	if isOwner {
		return nil
	}
	return status.Error(codes.PermissionDenied, "must be agent owner or tenant admin")
}

// AgentStore is the persistence seam for agent CRUD. *db.AgentRepo satisfies
// this interface; tests use fakeAgentStore.
type AgentStore interface {
	Create(ctx context.Context, a *db.Agent) error
	List(ctx context.Context, tenantID string) ([]*db.Agent, error)
	Get(ctx context.Context, tenantID, id string) (*db.Agent, error)
	Update(ctx context.Context, a *db.Agent) error
	Delete(ctx context.Context, tenantID, id string) error
	SetSoul(ctx context.Context, tenantID, id, soul string) error
	// AG-01: the tenant default agent and the per-user default-agent setting.
	EnsureDefault(ctx context.Context, tenantID string) (*db.Agent, bool, error)
	UserDefault(ctx context.Context, tenantID, userID string) (string, error)
	SetUserDefault(ctx context.Context, tenantID, userID, agentID string) error
}

// validateAgentSkills rejects any skill the agent editor could not legitimately
// offer. The authoritative vocabulary is the tool registry (baseline + tenant
// overlay + mcp: prefix rule, all via RequiredScope) plus capabilitySkills —
// the FGA-gated authz skills (scheduler/workflows/vision) that ListSkills emits
// but which are absent from the registry because they are not invokable tools.
// Keep this in lockstep with what ListSkills exposes or the editor will show
// selectable skills that fail to save.
func (s *BrokerService) validateAgentSkills(a *brokerv1.Agent) error {
	reg := s.toolReg()
	for _, sk := range a.Skills {
		if _, ok := reg.RequiredScope(sk); ok {
			continue // baseline, overlay, or mcp: prefix
		}
		if slices.Contains(capabilitySkills, sk) {
			continue
		}
		return status.Errorf(codes.InvalidArgument,
			"unknown skill %q: must be a registered tool id, capability skill (%s), or mcp:-prefixed",
			sk, strings.Join(capabilitySkills, ", "))
	}
	return nil
}

func (s *BrokerService) agentsEnabled() bool { return s.deps.Agents != nil }

func toProtoAgent(a *db.Agent) *brokerv1.Agent {
	revoked := ""
	if a.RevokedBefore != nil {
		revoked = a.RevokedBefore.UTC().Format(time.RFC3339)
	}
	return &brokerv1.Agent{
		SessionsRevokedAt: revoked,
		Id:                a.ID.String(),
		Name:              a.Name,
		LlmModel:          a.LLMModel,
		ApprovalMode:      a.ApprovalMode,
		Skills:            a.Skills,
		McpServers:        a.McpServers,
		McpTools:          a.McpTools,
		AllowedProviders:  a.AllowedProviders,
		PreferredProvider: a.PreferredProvider,
		Soul:              a.Soul,
		GatewayEnabled:    a.GatewayEnabled,
		ServiceAgent:      a.ServiceAgent,
		IsDefault:         a.IsDefault,
		Status:            a.Status,
		StatusReason:      a.StatusReason,
		CreatedBy:         a.CreatedBy,
		CreatedAt:         timestamppb.New(a.CreatedAt),
		UpdatedAt:         timestamppb.New(a.UpdatedAt),
	}
}

// validateAgent checks the agent fields that are invariant across create/update.
// approval_mode defaults to "needs_approval" when empty.
func validateAgent(a *brokerv1.Agent) error {
	if a == nil {
		return status.Error(codes.InvalidArgument, "agent is required")
	}
	if strings.TrimSpace(a.Name) == "" {
		return status.Error(codes.InvalidArgument, "agent name is required")
	}
	if len(a.Name) > 128 {
		return status.Errorf(codes.InvalidArgument, "agent name exceeds 128 characters")
	}
	if a.ApprovalMode == "" {
		a.ApprovalMode = "needs_approval"
	}
	if a.ApprovalMode != "auto" && a.ApprovalMode != "needs_approval" {
		return status.Errorf(codes.InvalidArgument, "approval_mode must be \"auto\" or \"needs_approval\", got %q", a.ApprovalMode)
	}
	if a.LlmModel != "" {
		if len(a.LlmModel) > 128 {
			return status.Error(codes.InvalidArgument, "llm_model exceeds 128 characters")
		}
		if !config.ValidLLMModel(a.LlmModel) {
			return status.Errorf(codes.InvalidArgument, "llm_model must match provider/model (^[a-z0-9._-]+/[a-z0-9._:-]+$), got %q", a.LlmModel)
		}
	}
	// dedupe mcp_servers
	seen := make(map[string]bool, len(a.McpServers))
	deduped := make([]string, 0, len(a.McpServers))
	for _, m := range a.McpServers {
		if m == "" {
			return status.Error(codes.InvalidArgument, "mcp_servers entries must be non-empty")
		}
		if !seen[m] {
			seen[m] = true
			deduped = append(deduped, m)
		}
	}
	a.McpServers = deduped
	// AG-08: attaching a connector grants no tool; mcp_tools names each one,
	// and only on a connector this agent attaches.
	toolSeen := make(map[string]bool, len(a.McpTools))
	tools := make([]string, 0, len(a.McpTools))
	for _, t := range a.McpTools {
		connID, _, ok := toolregistry.ParseMcpToolID(t)
		if !ok {
			return status.Errorf(codes.InvalidArgument, "mcp_tools entry %q must be mcp:<connector>:<tool>", t)
		}
		if !seen[connID] {
			return status.Errorf(codes.InvalidArgument, "mcp_tools entry %q: connector %s is not in mcp_servers", t, connID)
		}
		if !toolSeen[t] {
			toolSeen[t] = true
			tools = append(tools, t)
		}
	}
	a.McpTools = tools
	return nil
}

// mcpPermittedAgentTuples builds the FGA tuples that grant an agent access to
// each MCP connector. Runtime authorization (InvokeTool, ListAccessibleMcpServers)
// gates on can_access = permitted_agent or permitted_group; the agents.mcp_servers
// column is otherwise unenforced, so the editor's selection only takes effect once
// these tuples exist.
func mcpPermittedAgentTuples(agentID string, connIDs []string) []policy.Relation {
	rels := make([]policy.Relation, 0, len(connIDs))
	for _, c := range connIDs {
		rels = append(rels, policy.Relation{
			User:     "agent:" + agentID,
			Relation: "permitted_agent",
			Object:   "mcp_connector:" + c,
		})
	}
	return rels
}

// diffMcpServers returns the connector ids added (in desired, not current) and
// removed (in current, not desired) between two mcp_servers lists.
func diffMcpServers(current, desired []string) (added, removed []string) {
	curSet := make(map[string]bool, len(current))
	for _, c := range current {
		curSet[c] = true
	}
	desSet := make(map[string]bool, len(desired))
	for _, d := range desired {
		desSet[d] = true
	}
	for _, d := range desired {
		if !curSet[d] {
			added = append(added, d)
		}
	}
	for _, c := range current {
		if !desSet[c] {
			removed = append(removed, c)
		}
	}
	return added, removed
}

func (s *BrokerService) CreateAgent(ctx context.Context, req *brokerv1.CreateAgentRequest) (*brokerv1.CreateAgentResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.agents.create")
	defer span.End()

	if !s.agentsEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "agents disabled")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	if err := validateAgent(req.Agent); err != nil {
		return nil, err
	}
	if err := s.validateAgentSkills(req.Agent); err != nil {
		return nil, err
	}
	tenantUUID, err := uuid.Parse(tenant)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid tenant_id: %v", err)
	}
	a := &db.Agent{
		ID:                uuid.New(),
		TenantID:          tenantUUID,
		Name:              strings.TrimSpace(req.Agent.Name),
		LLMModel:          req.Agent.LlmModel,
		ApprovalMode:      req.Agent.ApprovalMode,
		Skills:            req.Agent.Skills,
		McpServers:        req.Agent.McpServers,
		McpTools:          req.Agent.McpTools,
		AllowedProviders:  req.Agent.AllowedProviders,
		PreferredProvider: req.Agent.PreferredProvider,
		GatewayEnabled:    req.Agent.GatewayEnabled,
		ServiceAgent:      req.Agent.ServiceAgent,
		CreatedBy:         user,
		Status:            db.AgentStaged, // AG-03: activation needs an owner
	}
	if a.Skills == nil {
		a.Skills = []string{}
	}
	if a.McpServers == nil {
		a.McpServers = []string{}
	}
	if a.AllowedProviders == nil {
		a.AllowedProviders = []string{}
	}
	if err := s.deps.Agents.Create(ctx, a); err != nil {
		return nil, status.Errorf(codes.Internal, "create agent: %v", err)
	}
	// Re-fetch to pick up DB-assigned created_at/updated_at.
	fetched, err := s.deps.Agents.Get(ctx, tenant, a.ID.String())
	if err == nil {
		a = fetched
	}
	// Grant the FGA tuples runtime authorization checks. a.McpServers is the
	// deduped, persisted list (validateAgent dedupes in place before Create), and a
	// new agent has no pre-existing tuples, so this never re-writes. Fail loud
	// (mirrors AssignRole): a silently-skipped grant is the exact bug this fixes.
	rels := mcpPermittedAgentTuples(a.ID.String(), a.McpServers)
	// F9: grant the creator owner_user on the agent. The may-operate-agent gate
	// resolves can_use = usable_by or owner_user; without this tuple the agent's
	// own creator could not run workflows bound to it.
	if a.CreatedBy != "" {
		rels = append(rels, policy.Relation{
			User:     "user:" + a.CreatedBy,
			Relation: "owner_user",
			Object:   "agent:" + a.ID.String(),
		})
	}
	if len(rels) > 0 {
		if werr := s.deps.Policy.WriteRelations(ctx, rels...); werr != nil {
			return nil, status.Errorf(codes.Internal, "grant agent access: %v", werr)
		}
	}
	s.emitAgentAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.broker.agent.created", a, auditv1.PolicyDecision_ALLOW)
	return &brokerv1.CreateAgentResponse{Agent: toProtoAgent(a)}, nil
}

func (s *BrokerService) ListAgents(ctx context.Context, req *brokerv1.ListAgentsRequest) (*brokerv1.ListAgentsResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.agents.list")
	defer span.End()

	_, _, out, err := s.listAgents(ctx, req.TenantId, req.UserId, false)
	if err != nil {
		return nil, err
	}
	return &brokerv1.ListAgentsResponse{Agents: out}, nil
}

// listAgents is ListAgents behind the tenant-admin gate. With strictOwners an
// unreadable FGA fails the call instead of listing no owners (AG-14 export:
// a governance tool must not record an owned agent as ownerless).
func (s *BrokerService) listAgents(ctx context.Context, reqTenant, reqUser string, strictOwners bool) (tenant, user string, out []*brokerv1.Agent, err error) {
	if !s.agentsEnabled() {
		return "", "", nil, status.Error(codes.FailedPrecondition, "agents disabled")
	}
	tenant, user, err = s.callerIdentity(ctx, reqTenant, reqUser)
	if err != nil {
		return "", "", nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return "", "", nil, err
	}
	rows, err := s.deps.Agents.List(ctx, tenant)
	if err != nil {
		return "", "", nil, status.Errorf(codes.Internal, "list agents: %v", err)
	}
	// AG-04: the orphan flag rests on AG-06 block status, a security state, so
	// a read failure fails the listing rather than showing "not orphaned".
	var blocks map[string]db.UserBlock
	if s.deps.Revocation != nil {
		if blocks, err = s.deps.Revocation.UserBlocks(ctx, tenant); err != nil {
			return "", "", nil, status.Errorf(codes.Internal, "list user blocks: %v", err)
		}
	}
	out = make([]*brokerv1.Agent, 0, len(rows))
	for _, row := range rows {
		pa := toProtoAgent(row)
		// Populate usable_by subjects, owners and the orphan flag from FGA
		// (best-effort; FGA disabled or unreadable → empty, not flagged).
		rels, rerr := s.deps.Policy.ReadTuples(ctx, policy.ReadFilter{Object: "agent:" + row.ID.String()})
		if rerr != nil && strictOwners && s.deps.Policy.FGAEnabled() {
			return "", "", nil, status.Errorf(codes.Unavailable, "read agent owners: %v", rerr)
		}
		if rerr == nil && s.deps.Policy.FGAEnabled() {
			for _, r := range rels {
				if r.Relation == "usable_by" {
					pa.UsableBy = append(pa.UsableBy, r.User)
				}
			}
			pa.Owners = agentOwnerRefs(rels)
			// AG-01: the tenant default agent has no owner by design; the
			// tenant admins answer for it.
			pa.Orphaned = !row.IsDefault && agentOrphaned(pa.Owners, blocks)
		}
		out = append(out, pa)
	}
	return tenant, user, out, nil
}

// agentOrphaned reports an agent that no one is left to answer for (AG-04): it
// has no owner, or every owner is a user blocked under AG-06.
// AG-03: activation needs a non-orphaned agent (requireLiveOwner), and
// SweepOrphanedAgents suspends an agent that stays orphaned past a grace period.
// ponytail: a group owner counts as live without expanding its members; expand
// when groups whose members are all blocked turn up.
func agentOrphaned(owners []string, blocks map[string]db.UserBlock) bool {
	for _, o := range owners {
		subject, isUser := strings.CutPrefix(o, "user:")
		if !isUser {
			return false
		}
		if _, blocked := blocks[subject]; !blocked {
			return false
		}
	}
	return true
}

// BackfillAgentOwners grants owner_user to the creator of every agent that has
// no owner tuple (AG-04). The creator grant is written at creation only since
// F9, so older agents may have none. Idempotent (agents with any owner are
// skipped); main runs it once at startup. Each grant is audited as
// aikonos.broker.agent.owner.added. Returns the number of grants written.
func (s *BrokerService) BackfillAgentOwners(ctx context.Context, tenant string) (int, error) {
	if !s.agentsEnabled() || !s.deps.Policy.FGAEnabled() {
		return 0, nil
	}
	rows, err := s.deps.Agents.List(ctx, tenant)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, a := range rows {
		if a.CreatedBy == "" {
			continue // nobody to backfill; ListAgents flags it orphaned
		}
		obj := "agent:" + a.ID.String()
		rels, err := s.deps.Policy.ReadTuples(ctx, policy.ReadFilter{Object: obj})
		if err != nil {
			return n, err
		}
		if len(agentOwnerRefs(rels)) > 0 {
			continue
		}
		t := &brokerv1.RoleTuple{User: "user:" + a.CreatedBy, Relation: "owner_user", Object: obj}
		if err := s.deps.Policy.WriteRelations(ctx, relationOf(t)); err != nil {
			return n, err
		}
		n++
		s.emitAdminAudit(ctx, "", tenant, "system:owner-backfill",
			"aikonos.broker.agent.owner.added", t, auditv1.PolicyDecision_ALLOW)
	}
	return n, nil
}

func (s *BrokerService) UpdateAgent(ctx context.Context, req *brokerv1.UpdateAgentRequest) (*brokerv1.UpdateAgentResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.agents.update")
	defer span.End()

	if !s.agentsEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "agents disabled")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	if req.Agent == nil || req.Agent.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "agent.id is required")
	}
	if err := validateAgent(req.Agent); err != nil {
		return nil, err
	}
	if err := s.validateAgentSkills(req.Agent); err != nil {
		return nil, err
	}
	tenantUUID, err := uuid.Parse(tenant)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid tenant_id: %v", err)
	}
	agentID, err := uuid.Parse(req.Agent.Id)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid agent.id: %v", err)
	}
	// Capture the pre-update mcp_servers so the FGA grant can be reconciled by
	// diff after the row is written.
	existing, err := s.deps.Agents.Get(ctx, tenant, req.Agent.Id)
	if err != nil {
		if errors.Is(err, db.ErrAgentNotFound) {
			return nil, status.Errorf(codes.NotFound, "agent %s not found", req.Agent.Id)
		}
		return nil, status.Errorf(codes.Internal, "fetch agent: %v", err)
	}
	oldMcpServers := existing.McpServers
	skills := req.Agent.Skills
	if skills == nil {
		skills = []string{}
	}
	mcpServers := req.Agent.McpServers
	if mcpServers == nil {
		mcpServers = []string{}
	}
	allowedProviders := req.Agent.AllowedProviders
	if allowedProviders == nil {
		allowedProviders = []string{}
	}
	a := &db.Agent{
		ID:                agentID,
		TenantID:          tenantUUID,
		Name:              strings.TrimSpace(req.Agent.Name),
		LLMModel:          req.Agent.LlmModel,
		ApprovalMode:      req.Agent.ApprovalMode,
		Skills:            skills,
		McpServers:        mcpServers,
		McpTools:          req.Agent.McpTools,
		AllowedProviders:  allowedProviders,
		PreferredProvider: req.Agent.PreferredProvider,
		GatewayEnabled:    req.Agent.GatewayEnabled,
		ServiceAgent:      req.Agent.ServiceAgent,
	}
	if err := s.deps.Agents.Update(ctx, a); err != nil {
		if errors.Is(err, db.ErrAgentNotFound) {
			return nil, status.Errorf(codes.NotFound, "agent %s not found", req.Agent.Id)
		}
		return nil, status.Errorf(codes.Internal, "update agent: %v", err)
	}
	fetched, err := s.deps.Agents.Get(ctx, tenant, a.ID.String())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "re-fetch agent: %v", err)
	}
	// Reconcile the permitted_agent grant: write connectors newly bound, revoke
	// those unbound. Unchanged list → empty diff → no FGA call (idempotent, and
	// avoids OpenFGA rejecting a duplicate write). Fail loud.
	added, removed := diffMcpServers(oldMcpServers, mcpServers)
	if rels := mcpPermittedAgentTuples(agentID.String(), added); len(rels) > 0 {
		if werr := s.deps.Policy.WriteRelations(ctx, rels...); werr != nil {
			return nil, status.Errorf(codes.Internal, "grant mcp access: %v", werr)
		}
	}
	if rels := mcpPermittedAgentTuples(agentID.String(), removed); len(rels) > 0 {
		if derr := s.deps.Policy.DeleteRelations(ctx, rels...); derr != nil {
			return nil, status.Errorf(codes.Internal, "revoke mcp access: %v", derr)
		}
	}
	s.emitAgentAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.broker.agent.updated", fetched, auditv1.PolicyDecision_ALLOW)
	return &brokerv1.UpdateAgentResponse{Agent: toProtoAgent(fetched)}, nil
}

func (s *BrokerService) DeleteAgent(ctx context.Context, req *brokerv1.DeleteAgentRequest) (*brokerv1.DeleteAgentResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.agents.delete")
	defer span.End()

	if !s.agentsEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "agents disabled")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	if req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	// Capture the connector ids and creator before the row is gone so their
	// permitted_agent + owner_user tuples can be cleaned up afterwards.
	var mcpServers []string
	var createdBy string
	if existing, gerr := s.deps.Agents.Get(ctx, tenant, req.Id); gerr == nil {
		if existing.IsDefault {
			// AG-01: chats and schedules that name no agent run as it.
			return nil, status.Error(codes.FailedPrecondition, "the tenant default agent cannot be deleted")
		}
		mcpServers = existing.McpServers
		createdBy = existing.CreatedBy
	} else if !errors.Is(gerr, db.ErrAgentNotFound) {
		s.deps.Logger.Warn("agent tuple cleanup: pre-delete fetch failed",
			zap.String("agent", req.Id), zap.Error(gerr))
	}
	if err := s.deps.Agents.Delete(ctx, tenant, req.Id); err != nil {
		if errors.Is(err, db.ErrAgentNotFound) {
			return nil, status.Errorf(codes.NotFound, "agent %s not found", req.Id)
		}
		return nil, status.Errorf(codes.Internal, "delete agent: %v", err)
	}
	// Best-effort: orphaned permitted_agent / owner_user tuples for a deleted
	// agent are inert (the agent object no longer resolves), so a cleanup failure
	// must not fail the delete.
	rels := mcpPermittedAgentTuples(req.Id, mcpServers)
	if createdBy != "" {
		rels = append(rels, policy.Relation{
			User:     "user:" + createdBy,
			Relation: "owner_user",
			Object:   "agent:" + req.Id,
		})
	}
	if len(rels) > 0 {
		if derr := s.deps.Policy.DeleteRelations(ctx, rels...); derr != nil {
			s.deps.Logger.Warn("agent tuple cleanup failed",
				zap.String("agent", req.Id), zap.Error(derr))
		}
	}
	deletedID, _ := uuid.Parse(req.Id)
	s.emitAgentAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.broker.agent.deleted", &db.Agent{ID: deletedID}, auditv1.PolicyDecision_ALLOW)
	return &brokerv1.DeleteAgentResponse{Success: true}, nil
}

func (s *BrokerService) ListMyAgents(ctx context.Context, req *brokerv1.ListMyAgentsRequest) (*brokerv1.ListMyAgentsResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.agents.list_mine")
	defer span.End()

	if !s.agentsEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "agents disabled")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}

	if !s.deps.Policy.FGAEnabled() {
		// Dev allow-all: return all tenant agents (mirrors CheckFGA stub behaviour).
		rows, err := s.deps.Agents.List(ctx, tenant)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list agents: %v", err)
		}
		out := make([]*brokerv1.MyAgent, 0, len(rows))
		for _, row := range rows {
			if !row.IsDefault {
				out = append(out, &brokerv1.MyAgent{Id: row.ID.String(), Name: row.Name})
			}
		}
		return &brokerv1.ListMyAgentsResponse{Agents: out}, nil
	}

	refs, err := s.deps.Policy.ListObjects(ctx, "user:"+user, "can_use", "agent")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list accessible agents: %v", err)
	}
	if len(refs) == 0 {
		return &brokerv1.ListMyAgentsResponse{Agents: []*brokerv1.MyAgent{}}, nil
	}

	canUse := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if id, ok := strings.CutPrefix(ref, "agent:"); ok {
			canUse[id] = true
		}
	}

	rows, err := s.deps.Agents.List(ctx, tenant)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list agents: %v", err)
	}
	out := make([]*brokerv1.MyAgent, 0)
	for _, row := range rows {
		if canUse[row.ID.String()] && !row.IsDefault {
			out = append(out, &brokerv1.MyAgent{Id: row.ID.String(), Name: row.Name})
		}
	}
	return &brokerv1.ListMyAgentsResponse{Agents: out}, nil
}

func (s *BrokerService) GetAgentSoul(ctx context.Context, req *brokerv1.GetAgentSoulRequest) (*brokerv1.GetAgentSoulResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.agents.get_soul")
	defer span.End()

	if !s.agentsEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "agents disabled")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	// agent_id backs a uuid DB column; a syntactically invalid id must be
	// rejected here as NotFound rather than reaching Postgres, which would
	// 22P02 into codes.Internal.
	if _, err := uuid.Parse(req.AgentId); err != nil {
		return nil, status.Error(codes.NotFound, "agent not found")
	}
	agent, err := s.deps.Agents.Get(ctx, tenant, req.AgentId)
	if err != nil {
		if errors.Is(err, db.ErrAgentNotFound) {
			return nil, status.Errorf(codes.NotFound, "agent not found")
		}
		return nil, status.Errorf(codes.Internal, "fetch agent: %v", err)
	}
	if err := s.requireAgentOwnerOrAdmin(ctx, user, req.AgentId); err != nil {
		return nil, err
	}
	return &brokerv1.GetAgentSoulResponse{Soul: agent.Soul}, nil
}

func (s *BrokerService) SetAgentSoul(ctx context.Context, req *brokerv1.SetAgentSoulRequest) (*brokerv1.SetAgentSoulResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.agents.set_soul")
	defer span.End()

	if !s.agentsEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "agents disabled")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	// agent_id backs a uuid DB column; a syntactically invalid id must be
	// rejected here as NotFound rather than reaching Postgres, which would
	// 22P02 into codes.Internal.
	if _, err := uuid.Parse(req.AgentId); err != nil {
		return nil, status.Error(codes.NotFound, "agent not found")
	}
	agent, err := s.deps.Agents.Get(ctx, tenant, req.AgentId)
	if err != nil {
		if errors.Is(err, db.ErrAgentNotFound) {
			return nil, status.Errorf(codes.NotFound, "agent not found")
		}
		return nil, status.Errorf(codes.Internal, "fetch agent: %v", err)
	}
	if err := s.requireAgentOwnerOrAdmin(ctx, user, req.AgentId); err != nil {
		return nil, err
	}
	cleaned, err := sanitizeSoul(req.Soul)
	if err != nil {
		return nil, err
	}
	if err := s.deps.Agents.SetSoul(ctx, tenant, req.AgentId, cleaned); err != nil {
		if errors.Is(err, db.ErrAgentNotFound) {
			return nil, status.Errorf(codes.NotFound, "agent not found")
		}
		return nil, status.Errorf(codes.Internal, "set soul: %v", err)
	}
	s.emitAgentAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.broker.agent.soul.updated", agent, auditv1.PolicyDecision_ALLOW)
	return &brokerv1.SetAgentSoulResponse{Soul: cleaned}, nil
}

func (s *BrokerService) emitAgentAudit(ctx context.Context, traceID, tenant, actor, eventType string, a *db.Agent, decision auditv1.PolicyDecision) {
	ev := &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: actor,
		EventType:   eventType,
		Decision:    decision,
	}
	if a != nil {
		ev.ResourceRef = "agent:" + a.ID.String()
		mcpTools := make([]any, len(a.McpTools))
		for i, t := range a.McpTools {
			mcpTools[i] = t
		}
		if c, err := structpb.NewStruct(map[string]any{
			"name": a.Name, "approval_mode": a.ApprovalMode, "llm_model": a.LLMModel,
			"service_agent": a.ServiceAgent, "mcp_tools": mcpTools,
		}); err == nil {
			ev.Context = c
		}
	}
	if err := s.deps.Audit.Emit(ctx, ev); err != nil {
		s.deps.Logger.Warn("agent audit emit failed", zap.Error(err))
	}
}
