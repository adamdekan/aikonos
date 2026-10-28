package broker

import (
	"context"

	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
)

// AG-02: an agent never does more than the person it acts for. A task whose
// subject is a human needs the subject's own skill / connector grant next to
// the agent's, so a hijacked agent cannot exceed its user's rights. The
// per-tenant identity.enforce_user_bound setting rolls this out: audit emits
// userBoundWouldDenyEvent and allows, enforce denies.
const (
	userBoundSetting        = "identity.enforce_user_bound"
	userBoundWouldDenyEvent = "aikonos.broker.user_bound.would_deny"
)

// userBoundExemption names why a task's subject is not checked: "service_run"
// for a svc- owner (no person behind it), "service_agent" for an agent an admin
// flagged as a controlled door to its connectors. "" means the subject is a
// human whose own grants bound the agent. An agent lookup failure is no
// exemption (fails closed).
func (s *SandboxService) userBoundExemption(ctx context.Context, tenantID string, task *db.Task) string {
	if task.OnBehalfOf().ServiceRun {
		return "service_run"
	}
	if task.AgentID != nil && s.deps.Agents != nil {
		if a, err := s.deps.Agents.Get(ctx, tenantID, task.AgentID.String()); err == nil && a.ServiceAgent {
			return "service_agent"
		}
	}
	return ""
}

// userBoundMode reads identity.enforce_user_bound. It is read only after a
// check failed, off the hot path. An unreadable setting enforces.
func (s *SandboxService) userBoundMode(ctx context.Context, tenantID string) string {
	mode, err := s.cfg().GetString(ctx, tenantID, userBoundSetting)
	if err != nil {
		s.deps.Logger.Warn("user bound: setting unreadable, enforcing",
			zap.String("tenant_id", tenantID), zap.Error(err))
		return "enforce"
	}
	return mode
}

// userConnectorDecision checks the task owner's own can_access on the
// connector of an mcp: tool — the skill object "skill:mcp:<conn>:<tool>" is
// not a valid OpenFGA id, so the user check targets mcp_connector. Same
// contract as userSkillDecision: "allow"/"deny", fails closed on error, a
// malformed id denies.
func (s *SandboxService) userConnectorDecision(ctx context.Context, ownerUserID, toolID string) (string, *fgaRecord) {
	connID, _, ok := toolregistry.ParseMcpToolID(toolID)
	if !ok {
		return "deny", nil
	}
	return s.userFGADecision(ctx, &fgaRecord{User: "user:" + ownerUserID, Relation: "can_access", Object: "mcp_connector:" + connID})
}

// planUserBound applies the rollout mode to a failed plan-time user check.
// enforce keeps the deny; audit emits a would-deny and lets the step through
// to the other layers. It returns the FGA decision to route with and the
// decision-record note.
func (s *SandboxService) planUserBound(ctx context.Context, tenantID string, task *db.Task, toolID, fgaDec string, rec *fgaRecord) (string, map[string]any) {
	if fgaDec != "deny" {
		return fgaDec, nil
	}
	mode := s.userBoundMode(ctx, tenantID)
	note := map[string]any{"mode": mode}
	if mode == "enforce" {
		return fgaDec, note
	}
	note["would_deny"] = true
	reason, fields := "user_skill_would_deny", map[string]any{"tool_id": toolID}
	if connID, _, ok := toolregistry.ParseMcpToolID(toolID); ok {
		reason, fields = "mcp_user_access_would_deny", map[string]any{"connector": connID}
	}
	fields["reason"], fields["mode"], fields["stage"] = reason, mode, "plan"
	fields["user"], fields["agent"] = "user:"+task.OwnerUserID, task.OnBehalfOf().Actor
	if rec != nil && rec.Error != "" {
		fields["error"] = rec.Error
	}
	s.emitUserBoundWouldDeny(ctx, tenantID, task.OwnerUserID, toolID, fields)
	return "", note
}

// emitUserBoundWouldDeny audits a user-bound check the subject failed while
// the tenant is in audit mode. A dedicated event type so admins can list them
// (Settings → Identity) and fix grants before enforcing. Fire-and-forget.
func (s *SandboxService) emitUserBoundWouldDeny(ctx context.Context, tenantID, userID, toolID string, fields map[string]any) {
	if s.deps.Audit == nil {
		return
	}
	ctxStruct, _ := structpb.NewStruct(fields)
	if err := s.deps.Audit.Emit(ctx, withOBO(&auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TenantId:    tenantID,
		OccurredAt:  timestampNow(),
		ActorUserId: userID,
		EventType:   userBoundWouldDenyEvent,
		ResourceRef: "aikonos:tool:" + toolID,
		Decision:    auditv1.PolicyDecision_DENY,
		Context:     ctxStruct,
	}, oboTask(ctx))); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, userBoundWouldDenyEvent)
	}
}
