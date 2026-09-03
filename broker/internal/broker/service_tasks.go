package broker

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/approvalsvc"
	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
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

	t := &db.Task{
		TaskID:          uuid.New(),
		TenantID:        tenantID,
		OwnerUserID:     owner,
		Prompt:          req.Prompt,
		CostBudget:      costBudget,
		TraceID:         traceID,
		GatewayManaged:  hasGatewayHint(req.SkillHints),
		ClientRequestID: req.ClientRequestId,
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

	return persistManagedTask(ctx, s.deps, s.deps.Tasks, t, traceID)
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

	// North-only pre-check: ReBAC gate before touching the approval record.
	// resolveApprovalAndMint re-runs CheckFGA internally; this early check gives
	// a cleaner PermissionDenied before any DB load on the north surface.
	allowed, err := s.deps.Policy.CheckFGA(ctx, "user:"+req.ApproverUserId, "can_approve", "task:"+req.TaskId)
	if err != nil || !allowed {
		return nil, status.Errorf(codes.PermissionDenied, "approver not authorized for task %s", req.TaskId)
	}

	respStatus, capTokens, eventType, decision, err := approvalsvc.Resolve(
		ctx, s.deps.Tasks, s.deps.Policy, approvalMinter(s.deps.Capability, s.toolReg()), s.deps.Logger,
		req.TenantId, req.TaskId, req.ApproverUserId,
		req.Approved, req.Reason,
	)
	if err != nil {
		return nil, err
	}

	s.deps.Logger.Info("ApproveTask",
		zap.String("task_id", req.TaskId),
		zap.Bool("approved", req.Approved),
		zap.String("approver", req.ApproverUserId),
		zap.String("new_status", respStatus.String()),
	)

	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    req.TenantId,
		OccurredAt:  timestampNow(),
		ActorUserId: req.ApproverUserId,
		EventType:   eventType,
		ResourceRef: "aikonos:task:" + req.TaskId,
		Decision:    decision,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, eventType)
	}

	publishTaskEvent(ctx, s.deps, req.TenantId, req.TaskId, "APPROVAL_RECEIVED", map[string]any{
		"approved":   req.Approved,
		"approver":   req.ApproverUserId,
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
