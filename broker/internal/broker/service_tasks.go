package broker

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/approvalsvc"
	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/gatewaygrant"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/notify"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// toolReg returns the injected Registry, falling back to the package-default
// when ToolRegistry is nil (unit tests that predate FU9 and don't inject it).
// The fallback is baseline-only: PkgDefault() has no manifest overlay loaded,
// so overlay tools resolve only through the production-injected Registry.
func (s *BrokerService) toolReg() *toolregistry.Registry {
	if s.deps.ToolRegistry != nil {
		return s.deps.ToolRegistry
	}
	return toolregistry.PkgDefault()
}

// CreateTask creates a new task for the authenticated caller, binding the
// owner to the OIDC identity on the context (request user_id must match or be
// absent); returns InvalidArgument if tenant_id is not a valid UUID.
func (s *BrokerService) CreateTask(ctx context.Context, req *brokerv1.CreateTaskRequest) (*brokerv1.TaskHandle, error) {
	ctx, span := tracer.Start(ctx, "broker.task.create")
	defer span.End()
	traceID := span.SpanContext().TraceID().String()

	// sub→owner binding: derive the acting identity from the OIDC context.
	// When no identity is present (dev / passthrough), fall back to the request
	// fields so existing unit tests and south callers are unaffected.
	tenant, owner, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}

	tenantID, err := uuid.Parse(tenant)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid tenant_id: %v", err)
	}

	span.SetAttributes(
		attribute.String("tenant.id", tenant),
		attribute.String("user.id", owner),
	)

	costBudget := req.CostBudget
	if costBudget == 0 {
		costBudget = 1000
	}

	agentID, err := s.northActor(ctx, traceID, tenant, owner, req.AgentId)
	if err != nil {
		return nil, err
	}

	// A north task is always a signed-in user acting directly (AG-17).
	kind := gatewaygrant.KindInteractive
	t := &db.Task{
		TaskID:          uuid.New(),
		TenantID:        tenantID,
		OwnerUserID:     owner,
		Prompt:          req.Prompt,
		CostBudget:      costBudget,
		TraceID:         traceID,
		GatewayManaged:  hasGatewayHint(req.SkillHints),
		AgentID:         agentID,
		ClientRequestID: req.ClientRequestId,
		OBOKind:         &kind,
	}
	if req.Deadline != nil {
		ts := req.Deadline.AsTime()
		t.Deadline = &ts
	}
	if req.ParentTaskId != "" {
		pid, err := uuid.Parse(req.ParentTaskId)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid parent_task_id: %v", err)
		}
		t.ParentTaskID = &pid
	}
	if req.EnvelopeId != "" {
		t.DelegatedByUserID = s.envelopeDelegator(ctx, traceID, tenant, owner, req.EnvelopeId)
	}

	return persistManagedTask(ctx, s.deps, s.deps.Tasks, t, traceID)
}

// envelopeDelegator resolves CreateTaskRequest.envelope_id (AG-17): when the
// caller is the addressed recipient of a live (pending, delivered, accepted or
// dismissed) envelope in their tenant, it returns the envelope sender as the
// task's delegated_by. Anything else is ignored rather than failing the chat —
// the task is just not marked delegated — and audited as
// aikonos.broker.task.delegation_ignored with the reason.
func (s *BrokerService) envelopeDelegator(ctx context.Context, traceID, tenant, owner, envelopeID string) *string {
	reason := "envelope_not_found"
	var env *db.Envelope
	if _, err := uuid.Parse(envelopeID); err == nil {
		env, _ = s.deps.Tasks.GetEnvelope(ctx, tenant, envelopeID)
	}
	if env != nil && env.TenantID.String() == tenant {
		toType, _ := env.ToTarget["type"].(string)
		toID, _ := env.ToTarget["id"].(string)
		switch {
		// Same recipient test as RespondEnvelope/DismissEnvelope; group sends
		// fan out to one user envelope per member, so there is no group arm.
		case toType != "user" || toID != owner:
			reason = "envelope_not_recipient"
		case !envelopeUsable(env):
			reason = "envelope_not_active"
		default:
			from := env.FromUserID
			return &from
		}
	}
	ctxStruct, _ := structpb.NewStruct(map[string]any{"reason": reason, "envelope_id": envelopeID})
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: owner,
		EventType:   "aikonos.broker.task.delegation_ignored",
		ResourceRef: "envelope:" + envelopeID,
		Decision:    auditv1.PolicyDecision_DENY,
		Context:     ctxStruct,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, "aikonos.broker.task.delegation_ignored")
	}
	s.deps.Logger.Warn("CreateTask: envelope_id ignored",
		zap.String("envelope_id", envelopeID), zap.String("user", owner), zap.String("reason", reason))
	return nil
}

// envelopeUsable: the inbox "Send to agent" path dismisses before the chat
// runs, so DISMISSED counts alongside the open and accepted states. Rejected,
// completed and expired envelopes (or past expires_at) do not.
func envelopeUsable(env *db.Envelope) bool {
	if !env.ExpiresAt.IsZero() && time.Now().After(env.ExpiresAt) {
		return false
	}
	switch env.State {
	case db.EnvelopePending, db.EnvelopeDelivered, db.EnvelopeAccepted, db.EnvelopeDismissed:
		return true
	}
	return false
}

// northActor resolves CreateTaskRequest.agent_id (AG-17): empty is unbound;
// otherwise it must be the UUID of an agent in the tenant that the caller may
// operate (can_use, tenant-admin fallback). A refusal (reason actor_not_found
// or actor_not_permitted) is audited as aikonos.broker.task.denied and is
// PermissionDenied either way, so the error does not reveal which agents exist.
func (s *BrokerService) northActor(ctx context.Context, traceID, tenant, owner, agentID string) (*uuid.UUID, error) {
	if agentID == "" {
		return nil, nil
	}
	id, err := uuid.Parse(agentID)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid agent_id: %v", err)
	}
	reason := ""
	if s.deps.Agents == nil {
		reason = "actor_not_found" // ponytail: agents disabled = no agent exists; fail closed
	} else if _, err := s.deps.Agents.Get(ctx, tenant, id.String()); err != nil {
		reason = "actor_not_found"
	} else if !mayOperateActor(ctx, s.deps, owner, id.String()) {
		reason = "actor_not_permitted"
	}
	if reason != "" {
		ctxStruct, _ := structpb.NewStruct(map[string]any{"reason": reason})
		if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
			EventId:      ids.EventID(),
			TraceId:      traceID,
			TenantId:     tenant,
			OccurredAt:   timestampNow(),
			ActorUserId:  owner,
			ActorAgentId: id.String(),
			EventType:    "aikonos.broker.task.denied",
			ResourceRef:  "agent:" + id.String(),
			Decision:     auditv1.PolicyDecision_DENY,
			Context:      ctxStruct,
		}); err != nil {
			audit.RecordEmitFailure(ctx, s.deps.Logger, err, "aikonos.broker.task.denied")
		}
		return nil, status.Errorf(codes.PermissionDenied, "you do not have access to agent %s", id)
	}
	return &id, nil
}

// writeTaskRelations records the owner/approver relationships for a new task in
// OpenFGA so the ReBAC Checks in ApproveTask/CancelTask resolve. Best-effort and
// a no-op when OpenFGA is disabled. The owner is also seeded as approver to
// match the default ApproverSet=[owner] (dev self-approval; separation-of-duty
// approver assignment is a future policy concern).
func (s *BrokerService) writeTaskRelations(ctx context.Context, owner, taskID string) {
	if err := s.deps.Policy.WriteRelations(ctx,
		policy.Relation{User: "user:" + owner, Relation: "owner", Object: "task:" + taskID},
		policy.Relation{User: "user:" + owner, Relation: "approver", Object: "task:" + taskID},
	); err != nil {
		s.deps.Logger.Warn("writeTaskRelations failed", zap.String("task_id", taskID), zap.Error(err))
	}
}

// GetTaskState returns the current state and cost counters for a task;
// returns NotFound when the task does not exist under the given tenant.
func (s *BrokerService) GetTaskState(ctx context.Context, req *brokerv1.GetTaskStateRequest) (*brokerv1.TaskState, error) {
	ctx, span := tracer.Start(ctx, "broker.task.get_state")
	defer span.End()

	tenant, _, err := s.callerIdentity(ctx, req.TenantId, "")
	if err != nil {
		return nil, err
	}

	t, err := s.deps.Tasks.Get(ctx, tenant, req.TaskId)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "task %s not found", req.TaskId)
		}
		s.deps.Logger.Error("GetTaskState: DB query failed", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "failed to get task")
	}

	return taskToProto(t), nil
}

// ApproveTask records a human approval or rejection for a task awaiting human
// review; returns PermissionDenied if the approver lacks the FGA `can_approve`
// relation on the task.
func (s *BrokerService) ApproveTask(ctx context.Context, req *brokerv1.ApproveTaskRequest) (*brokerv1.ApproveTaskResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.task.approve")
	defer span.End()
	traceID := span.SpanContext().TraceID().String()

	// C-01: bind tenant and approver to the authenticated identity — a forged
	// approver_user_id (e.g. the task owner) must never satisfy the FGA gate or
	// be counted as a voter.
	tenant, approver, err := s.callerIdentity(ctx, req.TenantId, req.ApproverUserId)
	if err != nil {
		return nil, err
	}

	// North-only pre-check: ReBAC gate before touching the approval record.
	// resolveApprovalAndMint re-runs CheckFGA internally; this early check gives
	// a cleaner PermissionDenied before any DB load on the north surface.
	allowed, err := s.deps.Policy.CheckFGA(ctx, "user:"+approver, "can_approve", "task:"+req.TaskId)
	if err != nil || !allowed {
		return nil, status.Errorf(codes.PermissionDenied, "approver not authorized for task %s", req.TaskId)
	}

	respStatus, capTokens, eventType, decision, err := approvalsvc.Resolve(
		ctx, s.deps.Tasks, s.deps.Policy, approvalMinter(s.deps.Capability, s.toolReg()), s.deps.Logger,
		tenant, req.TaskId, approver,
		req.Approved, req.Reason,
	)
	if err != nil {
		return nil, err
	}

	s.deps.Logger.Info("ApproveTask",
		zap.String("task_id", req.TaskId),
		zap.Bool("approved", req.Approved),
		zap.String("approver", approver),
		zap.String("new_status", respStatus.String()),
	)

	// Audit-only re-read for the on-behalf-of chain (AG-17), matching
	// ApproveGatewayTask; a failed read just leaves the chain off the event.
	task, _ := s.deps.Tasks.Get(ctx, tenant, req.TaskId)
	if err := s.deps.Audit.Emit(ctx, withOBO(&auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: approver,
		EventType:   eventType,
		ResourceRef: "aikonos:task:" + req.TaskId,
		Decision:    decision,
	}, task)); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, eventType)
	}

	publishTaskEvent(ctx, s.deps, tenant, req.TaskId, "APPROVAL_RECEIVED", map[string]any{
		"approved":   req.Approved,
		"approver":   approver,
		"new_status": respStatus.String(),
	})

	return &brokerv1.ApproveTaskResponse{
		Success:            true,
		NewStatus:          respStatus,
		CapabilityTokenIds: capTokens,
	}, nil
}

// CancelTask transitions a task to CANCELLED and tears down its sandbox;
// returns FailedPrecondition if the task is already in a terminal state.
func (s *BrokerService) CancelTask(ctx context.Context, req *brokerv1.CancelTaskRequest) (*brokerv1.CancelTaskResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.task.cancel")
	defer span.End()

	tenant, _, err := s.callerIdentity(ctx, req.TenantId, "")
	if err != nil {
		return nil, err
	}

	tasks := s.deps.Tasks

	// Load current state to find the valid transition source
	t, err := tasks.Get(ctx, tenant, req.TaskId)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "task %s not found", req.TaskId)
		}
		return nil, status.Errorf(codes.Internal, "failed to get task")
	}

	if err := tasks.Transition(ctx, tenant, req.TaskId, t.State, db.TaskStateCancelled); err != nil {
		s.deps.Logger.Error("CancelTask: transition failed", zap.Error(err))
		return nil, status.Errorf(codes.FailedPrecondition, "cannot cancel task in state %s: %v", t.State, err)
	}

	s.deps.Logger.Info("CancelTask", zap.String("task_id", req.TaskId), zap.String("reason", req.Reason))
	publishTaskEvent(ctx, s.deps, tenant, req.TaskId, "STATUS_CHANGED", map[string]any{
		"status": brokerv1.TaskStatus_CANCELLED.String(),
		"reason": req.Reason,
	})
	return &brokerv1.CancelTaskResponse{Success: true}, nil
}

// StreamTaskEvents subscribes to the task's NATS subject and forwards lifecycle
// events to the client until it disconnects. Requires NATS — with the bus
// disabled it reports Unimplemented (the broker can't push events without it).
func (s *BrokerService) StreamTaskEvents(req *brokerv1.StreamTaskEventsRequest, stream brokerv1.BrokerService_StreamTaskEventsServer) error {
	if s.deps.Notify == nil || !s.deps.Notify.Enabled() {
		return status.Error(codes.Unimplemented, "event streaming requires NATS (not configured)")
	}

	ctx := stream.Context()
	tenant, _, err := s.callerIdentity(ctx, req.TenantId, "")
	if err != nil {
		return err
	}

	sub, err := s.deps.Notify.Subscribe(notify.TaskSubject(tenant, req.TaskId))
	if err != nil {
		return status.Errorf(codes.Internal, "subscribe failed: %v", err)
	}
	defer sub.Unsubscribe()

	s.deps.Logger.Info("StreamTaskEvents: subscribed",
		zap.String("task_id", req.TaskId),
		zap.String("tenant_id", tenant),
	)

	for {
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case raw, ok := <-sub.Events():
			if !ok {
				return nil
			}
			var ev notify.Event
			if err := json.Unmarshal(raw, &ev); err != nil {
				s.deps.Logger.Debug("StreamTaskEvents: drop malformed event", zap.Error(err))
				continue
			}
			if err := stream.Send(notifyEventToProto(&ev)); err != nil {
				return err
			}
		}
	}
}
