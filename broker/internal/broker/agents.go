package broker

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"

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
// hold owner_user on agent:<agentID>. Fails closed on any FGA transport error.
func (s *BrokerService) requireAgentOwnerOrAdmin(ctx context.Context, user, agentID string) error {
	isAdmin, err := s.deps.Policy.CheckFGA(ctx, "user:"+user, "admin", s.fgaTenantObject())
	if err != nil {
		return status.Error(codes.Internal, "authorization check failed")
	}
	if isAdmin {
		return nil
	}
	isOwner, err := s.deps.Policy.CheckFGA(ctx, "user:"+user, "owner_user", "agent:"+agentID)
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
	return &brokerv1.Agent{
		Id:                a.ID.String(),
		Name:              a.Name,
		LlmModel:          a.LLMModel,
		ApprovalMode:      a.ApprovalMode,
		Skills:            a.Skills,
		McpServers:        a.McpServers,
		AllowedProviders:  a.AllowedProviders,
		PreferredProvider: a.PreferredProvider,
		Soul:              a.Soul,
		GatewayEnabled:    a.GatewayEnabled,
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
		AllowedProviders:  req.Agent.AllowedProviders,
		PreferredProvider: req.Agent.PreferredProvider,
		GatewayEnabled:    req.Agent.GatewayEnabled,
		CreatedBy:         user,
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
	rows, err := s.deps.Agents.List(ctx, tenant)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list agents: %v", err)
	}
	out := make([]*brokerv1.Agent, 0, len(rows))
	for _, row := range rows {
		pa := toProtoAgent(row)
		// Populate usable_by subjects from FGA (best-effort; FGA-disabled → empty).
		rels, rerr := s.deps.Policy.ReadTuples(ctx, policy.ReadFilter{
			Relation: "usable_by",
			Object:   "agent:" + row.ID.String(),
		})
		if rerr == nil {
			for _, r := range rels {
				pa.UsableBy = append(pa.UsableBy, r.User)
			}
		}
		out = append(out, pa)
	}
	return &brokerv1.ListAgentsResponse{Agents: out}, nil
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
		AllowedProviders:  allowedProviders,
		PreferredProvider: req.Agent.PreferredProvider,
		GatewayEnabled:    req.Agent.GatewayEnabled,
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
			out = append(out, &brokerv1.MyAgent{Id: row.ID.String(), Name: row.Name})
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
		if canUse[row.ID.String()] {
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
		if c, err := structpb.NewStruct(map[string]any{
			"name": a.Name, "approval_mode": a.ApprovalMode, "llm_model": a.LLMModel,
		}); err == nil {
			ev.Context = c
		}
	}
	if err := s.deps.Audit.Emit(ctx, ev); err != nil {
		s.deps.Logger.Warn("agent audit emit failed", zap.Error(err))
	}
}
