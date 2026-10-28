package broker

import (
	"context"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// AG-01: every task binds a registered agent. A run that names none binds the
// user's default agent: their default-agent setting while they may still use
// it, else the tenant default agent (agents.is_default).
//
// The tenant default agent carries the user's own grants, not its own: a task
// bound to it takes the personal skill gate at SubmitPlan, and memory sees no
// agent (no shared agent bundle across users). Its Skills list is ignored.
// MCP connectors attached to it are granted to every user that may use it.
// Every user may use it through the FGA tuple user:* usable_by agent:<id>,
// which an admin can replace with group grants to narrow it.

// runAgent resolves the agent a run binds to. agentID "" resolves the default.
// A refusal comes back as a reason (actor_not_found, actor_not_permitted) with
// a nil error; err is a store failure (callers return Unavailable).
func runAgent(ctx context.Context, deps Deps, tenant, user, agentID string) (*db.Agent, string, error) {
	if agentID != "" {
		if _, err := uuid.Parse(agentID); err != nil {
			return nil, "actor_not_found", nil
		}
		a, err := deps.Agents.Get(ctx, tenant, agentID)
		if err != nil {
			return nil, "actor_not_found", nil
		}
		if !mayOperateActor(ctx, deps, user, a.ID.String()) {
			return nil, "actor_not_permitted", nil
		}
		return a, "", nil
	}
	pref, err := deps.Agents.UserDefault(ctx, tenant, user)
	if err != nil {
		return nil, "", err
	}
	if pref != "" {
		// A setting the user may no longer use falls back to the tenant
		// default rather than refusing the chat; it is re-checked every run.
		if a, err := deps.Agents.Get(ctx, tenant, pref); err == nil && mayOperateActor(ctx, deps, user, pref) {
			return a, "", nil
		}
		deps.Logger.Info("default-agent setting not usable; using the tenant default agent",
			zap.String("user", user), zap.String("agent_id", pref))
	}
	a, err := ensureDefaultAgent(ctx, deps, tenant, false)
	if err != nil {
		return nil, "", err
	}
	if !mayOperateActor(ctx, deps, user, a.ID.String()) {
		return nil, "actor_not_permitted", nil
	}
	return a, "", nil
}

// ensureDefaultAgent returns the tenant default agent, creating it when
// missing. On create (or with repair, at startup) it grants user:* usable_by
// when the agent has no usable_by tuple at all, so an admin's narrower group
// grants are kept. The grant is best-effort and audited; until it lands, FGA
// refuses default-agent runs (fail closed) and the next startup repairs it.
func ensureDefaultAgent(ctx context.Context, deps Deps, tenant string, repair bool) (*db.Agent, error) {
	a, created, err := deps.Agents.EnsureDefault(ctx, tenant)
	if err != nil {
		return nil, err
	}
	if (!created && !repair) || deps.Policy == nil || !deps.Policy.FGAEnabled() {
		return a, nil
	}
	obj := "agent:" + a.ID.String()
	rels, err := deps.Policy.ReadTuples(ctx, policy.ReadFilter{Relation: "usable_by", Object: obj})
	if err == nil && len(rels) == 0 {
		err = deps.Policy.WriteRelations(ctx, policy.Relation{User: "user:*", Relation: "usable_by", Object: obj})
		if err == nil {
			emitAgentEvent(ctx, deps, tenant, "system:default-agent", a.ID.String(),
				"aikonos.broker.agent.usable_by.added", "", map[string]any{"user": "user:*", "relation": "usable_by", "object": obj})
		}
	}
	if err != nil {
		deps.Logger.Warn("default agent: usable_by grant failed", zap.String("agent_id", a.ID.String()), zap.Error(err))
	}
	return a, nil
}

// isDefaultAgent reports whether agentID is the tenant default agent. A lookup
// failure reports false, so the caller applies the stricter agent-bound rules.
func isDefaultAgent(ctx context.Context, deps Deps, tenant, agentID string) bool {
	if agentID == "" || deps.Agents == nil {
		return false
	}
	a, err := deps.Agents.Get(ctx, tenant, agentID)
	return err == nil && a.IsDefault
}

// emitAgentEvent audits an agent binding decision. reason, when set, makes it
// a DENY. Fire-and-forget.
func emitAgentEvent(ctx context.Context, deps Deps, tenant, user, agentID, eventType, reason string, extras map[string]any) {
	if deps.Audit == nil {
		return
	}
	fields := map[string]any{}
	for k, v := range extras {
		fields[k] = v
	}
	decision := auditv1.PolicyDecision_ALLOW
	if reason != "" {
		fields["reason"] = reason
		decision = auditv1.PolicyDecision_DENY
	}
	ctxStruct, _ := structpb.NewStruct(fields)
	ev := &auditv1.AuditEvent{
		EventId:      ids.EventID(),
		TraceId:      trace.SpanContextFromContext(ctx).TraceID().String(),
		TenantId:     tenant,
		OccurredAt:   timestampNow(),
		ActorUserId:  user,
		ActorAgentId: agentID,
		EventType:    eventType,
		Decision:     decision,
		Context:      ctxStruct,
	}
	if agentID != "" {
		ev.ResourceRef = "agent:" + agentID
	}
	if err := deps.Audit.Emit(ctx, ev); err != nil {
		audit.RecordEmitFailure(ctx, deps.Logger, err, eventType)
	}
}

// EnsureDefaultAgent creates the tenant default agent when missing and repairs
// its user:* usable_by grant. Idempotent; main runs it once at startup.
func (s *BrokerService) EnsureDefaultAgent(ctx context.Context, tenant string) error {
	if !s.agentsEnabled() {
		return nil
	}
	_, err := ensureDefaultAgent(ctx, s.deps, tenant, true)
	return err
}

// ResolveRunAgent resolves the agent a gateway run binds to (AG-01, H-05):
// a named agent the user can_use, else the user's default agent. A refusal is
// audited as aikonos.broker.task.denied and returned as PermissionDenied.
func (s *SandboxService) ResolveRunAgent(ctx context.Context, req *brokerv1.ResolveRunAgentRequest) (*brokerv1.ResolveRunAgentResponse, error) {
	if err := s.requireGatewayPeer(ctx); err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(req.TenantId); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid tenant_id: %v", err)
	}
	if req.UserId == "" {
		return nil, status.Error(codes.InvalidArgument, "user_id required")
	}
	if s.deps.Agents == nil {
		return nil, status.Error(codes.FailedPrecondition, "agents are not enabled")
	}
	a, reason, err := runAgent(ctx, s.deps, req.TenantId, req.UserId, req.AgentId)
	if err != nil {
		s.deps.Logger.Error("ResolveRunAgent: agent store failed", zap.Error(err))
		return nil, status.Error(codes.Unavailable, "agent could not be resolved; retry")
	}
	if reason != "" {
		emitAgentEvent(ctx, s.deps, req.TenantId, req.UserId, req.AgentId, "aikonos.broker.task.denied", reason, nil)
		return nil, status.Errorf(codes.PermissionDenied, "you do not have access to agent %s", req.AgentId)
	}
	return &brokerv1.ResolveRunAgentResponse{AgentId: a.ID.String(), IsDefault: a.IsDefault}, nil
}

// GetMyDefaultAgent returns the caller's default-agent setting ("" = the
// tenant default agent).
func (s *BrokerService) GetMyDefaultAgent(ctx context.Context, req *brokerv1.GetMyDefaultAgentRequest) (*brokerv1.MyDefaultAgent, error) {
	if !s.agentsEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "agents disabled")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	id, err := s.deps.Agents.UserDefault(ctx, tenant, user)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get default agent: %v", err)
	}
	return &brokerv1.MyDefaultAgent{AgentId: id}, nil
}

// SetMyDefaultAgent stores the caller's default-agent setting. The agent must
// be one the caller can_use (PermissionDenied otherwise, audited); "" or the
// tenant default agent clears the setting. Audited as
// aikonos.broker.agent.default.set.
func (s *BrokerService) SetMyDefaultAgent(ctx context.Context, req *brokerv1.SetMyDefaultAgentRequest) (*brokerv1.MyDefaultAgent, error) {
	if !s.agentsEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "agents disabled")
	}
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	id := req.AgentId
	if id != "" {
		a, reason, rerr := runAgent(ctx, s.deps, tenant, user, id)
		if rerr != nil {
			return nil, status.Error(codes.Unavailable, "agent could not be resolved; retry")
		}
		if reason != "" {
			emitAgentEvent(ctx, s.deps, tenant, user, id, "aikonos.broker.agent.default.set", reason, nil)
			return nil, status.Errorf(codes.PermissionDenied, "you do not have access to agent %s", id)
		}
		id = a.ID.String()
		if a.IsDefault {
			id = ""
		}
	}
	if err := s.deps.Agents.SetUserDefault(ctx, tenant, user, id); err != nil {
		return nil, status.Errorf(codes.Internal, "set default agent: %v", err)
	}
	emitAgentEvent(ctx, s.deps, tenant, user, id, "aikonos.broker.agent.default.set", "", nil)
	return &brokerv1.MyDefaultAgent{AgentId: id}, nil
}
