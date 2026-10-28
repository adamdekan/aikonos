// broker/internal/broker/agent_status.go
// AG-03: agent lifecycle and kill switch. agents.status is staged | active |
// suspended | retired and only an active agent runs. The status is read
// through the revocation Checker (cached like the AG-06 epoch, reset on a
// local write) at every path where a run starts or acts: runAgent (north
// CreateTask, CreateGatewayTask, ResolveRunAgent, scheduled fires),
// verifyOwnerGrant, SubmitPlan, ResolveAgentApiKey and every InvokeTool, so a
// suspension stops in-flight tasks at their next tool call.
package broker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// orphanSweepActor is the audit actor of an automatic orphan suspension.
const orphanSweepActor = "system:orphan-sweep"

// agentRunRefusal returns "" when agentID may run, else the refusal reason
// ("agent_suspended", ...). A state read failure is returned: callers fail
// closed. No agent ("") and no Checker wired (tests, dev) refuse nothing.
func agentRunRefusal(ctx context.Context, deps Deps, tenant, agentID string) (string, error) {
	st, err := deps.Revocation.Agent(ctx, tenant, agentID)
	if err != nil {
		return "", err
	}
	return st.AgentRefusal(), nil
}

// agentRefusedErr is the caller-facing error for an AG-03 refusal reason.
func agentRefusedErr(reason string) error {
	return status.Errorf(codes.PermissionDenied, "agent is %s (%s)", strings.TrimPrefix(reason, "agent_"), reason)
}

// runAgentErr maps a runAgent refusal reason to its caller-facing error.
func runAgentErr(reason, agentID string) error {
	if strings.HasPrefix(reason, "agent_") {
		return agentRefusedErr(reason)
	}
	return status.Errorf(codes.PermissionDenied, "you do not have access to agent %s", agentID)
}

// agentTransition returns the audit event for a lifecycle move, or false
// when the move is not allowed. retired is final.
func agentTransition(from, to string) (string, bool) {
	switch {
	case from == db.AgentStaged && to == db.AgentActive:
		return "aikonos.broker.agent.activated", true
	case from == db.AgentSuspended && to == db.AgentActive:
		return "aikonos.broker.agent.reactivated", true
	case from == db.AgentActive && to == db.AgentSuspended:
		return "aikonos.broker.agent.suspended", true
	case from != db.AgentRetired && to == db.AgentRetired:
		return "aikonos.broker.agent.retired", true
	}
	return "", false
}

// SetAgentStatus moves an agent through its lifecycle (AG-03). Tenant-admin
// gated; suspend and retire need a reason. Activation needs an owner who is
// not blocked (the tenant default agent has none by design); the tenant
// default agent may be suspended but not retired.
func (s *BrokerService) SetAgentStatus(ctx context.Context, req *brokerv1.SetAgentStatusRequest) (*brokerv1.SetAgentStatusResponse, error) {
	if !s.agentsEnabled() {
		return nil, status.Error(codes.FailedPrecondition, "agents disabled")
	}
	if s.deps.Revocation == nil {
		return nil, status.Error(codes.FailedPrecondition, "revocation not configured")
	}
	tenant, admin, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, admin); err != nil {
		return nil, err
	}
	if _, err := uuid.Parse(req.AgentId); err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid agent_id")
	}
	to := req.Status
	if to != db.AgentActive && to != db.AgentSuspended && to != db.AgentRetired {
		return nil, status.Error(codes.InvalidArgument, "status must be active, suspended or retired")
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" && to != db.AgentActive {
		return nil, status.Error(codes.InvalidArgument, "reason required")
	}
	if len(reason) > maxRevocationReason {
		return nil, status.Errorf(codes.InvalidArgument, "reason longer than %d bytes", maxRevocationReason)
	}
	a, err := s.deps.Agents.Get(ctx, tenant, req.AgentId)
	if errors.Is(err, db.ErrAgentNotFound) {
		return nil, status.Errorf(codes.NotFound, "agent %s not found", req.AgentId)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "fetch agent: %v", err)
	}
	event, ok := agentTransition(a.Status, to)
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "agent is %s; cannot set it %s", a.Status, to)
	}
	if to == db.AgentRetired && a.IsDefault {
		return nil, status.Error(codes.FailedPrecondition, "the tenant default agent cannot be retired")
	}
	if to == db.AgentActive && !a.IsDefault {
		if err := s.requireLiveOwner(ctx, tenant, a.ID.String()); err != nil {
			return nil, err
		}
	}
	if err := s.deps.Revocation.SetAgentStatus(ctx, tenant, a.ID.String(), a.Status, to, reason); err != nil {
		if errors.Is(err, db.ErrAgentNotFound) {
			return nil, status.Error(codes.Aborted, "agent status changed meanwhile; reload and retry")
		}
		return nil, status.Errorf(codes.Internal, "set agent status: %v", err)
	}
	s.emitRevocationAudit(ctx, tenant, admin, event, "agent:"+a.ID.String(),
		map[string]any{"reason": reason, "from": a.Status, "to": to, "name": a.Name})
	fetched, err := s.deps.Agents.Get(ctx, tenant, a.ID.String())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "re-fetch agent: %v", err)
	}
	return &brokerv1.SetAgentStatusResponse{Agent: toProtoAgent(fetched)}, nil
}

// requireLiveOwner refuses activation of an orphaned agent (AG-04): no owner,
// or every owner a blocked user. Fails closed when owners or blocks cannot be
// read, and when FGA is off (no owner can be shown).
func (s *BrokerService) requireLiveOwner(ctx context.Context, tenant, agentID string) error {
	if !s.deps.Policy.FGAEnabled() {
		return status.Error(codes.FailedPrecondition, "agent owners cannot be verified (authorization store disabled)")
	}
	rels, err := s.deps.Policy.ReadTuples(ctx, policy.ReadFilter{Object: "agent:" + agentID})
	if err != nil {
		return status.Error(codes.Internal, "read agent owners failed")
	}
	blocks, err := s.deps.Revocation.UserBlocks(ctx, tenant)
	if err != nil {
		return status.Errorf(codes.Internal, "list user blocks: %v", err)
	}
	if agentOrphaned(agentOwnerRefs(rels), blocks) {
		return status.Error(codes.FailedPrecondition, "agent needs at least one owner who is not blocked before it can be activated")
	}
	return nil
}

// SweepOrphanedAgents suspends every active agent that has stayed orphaned
// (no owner, or every owner blocked) for identity.orphan_grace_days (AG-03,
// AG-04). The orphan clock starts at the latest block among the owners, or
// at creation when the agent has no owner at all. The tenant default agent is
// exempt. Each suspension is audited as aikonos.broker.agent.suspended with
// actor system:orphan-sweep. Returns the number suspended.
func (s *BrokerService) SweepOrphanedAgents(ctx context.Context, tenant string, now time.Time) (int, error) {
	if !s.agentsEnabled() || s.deps.Revocation == nil || !s.deps.Policy.FGAEnabled() {
		return 0, nil
	}
	days, err := s.configFor().GetInt(ctx, tenant, "identity.orphan_grace_days")
	if err != nil {
		return 0, fmt.Errorf("read identity.orphan_grace_days: %w", err)
	}
	grace := time.Duration(days) * 24 * time.Hour
	rows, err := s.deps.Agents.List(ctx, tenant)
	if err != nil {
		return 0, err
	}
	blocks, err := s.deps.Revocation.UserBlocks(ctx, tenant)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, a := range rows {
		if a.IsDefault || a.Status != db.AgentActive {
			continue
		}
		rels, err := s.deps.Policy.ReadTuples(ctx, policy.ReadFilter{Object: "agent:" + a.ID.String()})
		if err != nil {
			return n, err
		}
		owners := agentOwnerRefs(rels)
		if !agentOrphaned(owners, blocks) {
			continue
		}
		since := a.CreatedAt // no owner at all
		if len(owners) > 0 {
			since = time.Time{} // every owner is a blocked user: the last block
			for _, o := range owners {
				if at := blocks[strings.TrimPrefix(o, "user:")].At; at.After(since) {
					since = at
				}
			}
		}
		if now.Sub(since) < grace {
			continue
		}
		reason := fmt.Sprintf("orphaned: no owner who is not blocked for %d days", days)
		if err := s.deps.Revocation.SetAgentStatus(ctx, tenant, a.ID.String(), db.AgentActive, db.AgentSuspended, reason); err != nil {
			if errors.Is(err, db.ErrAgentNotFound) {
				continue // changed meanwhile
			}
			return n, err
		}
		n++
		s.emitRevocationAudit(ctx, tenant, orphanSweepActor, "aikonos.broker.agent.suspended", "agent:"+a.ID.String(),
			map[string]any{"reason": reason, "from": db.AgentActive, "to": db.AgentSuspended, "name": a.Name})
		s.deps.Logger.Info("orphan sweep: agent suspended", zap.String("agent_id", a.ID.String()))
	}
	return n, nil
}
