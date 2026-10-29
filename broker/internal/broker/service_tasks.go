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
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/adamdekan/aikonos/broker/internal/approvalsvc"
	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/gatewaygrant"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/notify"
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

// Per-field caps on task inputs (G48, F179). The 16 MiB gRPC message cap is
// the only other bound, and these fields are stored and published verbatim.
const (
	maxPromptBytes          = 32 << 10
	maxClientRequestIDBytes = 512
)

// validateTaskFields bounds a task's caller-supplied strings before it is
// persisted; persistManagedTask runs it for north CreateTask and south
// CreateGatewayTask alike.
func validateTaskFields(t *db.Task) error {
	if len(t.Prompt) > maxPromptBytes {
		return status.Errorf(codes.InvalidArgument, "prompt longer than %d bytes", maxPromptBytes)
	}
	if len(t.ClientRequestID) > maxClientRequestIDBytes {
		return status.Errorf(codes.InvalidArgument, "client_request_id longer than %d bytes", maxClientRequestIDBytes)
	}
	return nil
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

	agentID, err := s.northActor(ctx, tenant, owner, req.AgentId)
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
		if env := s.envelopeDelegator(ctx, traceID, tenant, owner, req.EnvelopeId); env != nil {
			applyEnvelopeLimits(t, env)
		}
	}

	return persistManagedTask(ctx, s.deps, s.deps.Tasks, t, traceID)
}

// envelopeDelegator resolves CreateTaskRequest.envelope_id (AG-17): when the
// caller is the addressed recipient of an unexpired envelope in their tenant
// that they have ACCEPTED, it returns the envelope, whose sender and limits
// the task then carries (applyEnvelopeLimits). F139: a delivered or dismissed
// envelope the recipient never accepted does not let them stamp arbitrary
// tasks as delegated_by the sender. Anything else is
// ignored rather than failing the chat — the task is just not marked
// delegated — and audited as aikonos.broker.task.delegation_ignored with the
// reason.
func (s *BrokerService) envelopeDelegator(ctx context.Context, traceID, tenant, owner, envelopeID string) *db.Envelope {
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
		case env.State != db.EnvelopeAccepted:
			reason = "envelope_not_accepted"
		default:
			return env
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

// northActor resolves CreateTaskRequest.agent_id (AG-17, AG-01): a UUID of an
// agent in the tenant that the caller may operate (can_use, tenant-admin
// fallback); empty binds the caller's default agent. A refusal (reason
// actor_not_found or actor_not_permitted) is audited as
// aikonos.broker.task.denied and is PermissionDenied either way, so the error
// does not reveal which agents exist. With agents disabled the task stays
// unbound when none is named (there is no agent to bind).
func (s *BrokerService) northActor(ctx context.Context, tenant, owner, agentID string) (*uuid.UUID, error) {
	if agentID != "" {
		if _, err := uuid.Parse(agentID); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid agent_id: %v", err)
		}
	}
	if s.deps.Agents == nil {
		if agentID == "" {
			return nil, nil
		}
		// ponytail: agents disabled = no agent exists; fail closed
		emitAgentEvent(ctx, s.deps, tenant, owner, agentID, "aikonos.broker.task.denied", "actor_not_found", nil)
		return nil, status.Errorf(codes.PermissionDenied, "you do not have access to agent %s", agentID)
	}
	a, reason, err := runAgent(ctx, s.deps, tenant, owner, agentID)
	if err != nil {
		s.deps.Logger.Error("CreateTask: agent store failed", zap.Error(err))
		return nil, status.Error(codes.Unavailable, "agent could not be resolved; retry")
	}
	if reason != "" {
		emitAgentEvent(ctx, s.deps, tenant, owner, agentID, "aikonos.broker.task.denied", reason, nil)
		return nil, runAgentErr(reason, agentID)
	}
	return &a.ID, nil
}

// writeTaskRelations records the owner/approver relationships for a new task in
// OpenFGA so the ReBAC Checks in ApproveTask resolve. Best-effort and a no-op
// when OpenFGA is disabled. See writeTaskRelationsFromDeps.
func (s *BrokerService) writeTaskRelations(ctx context.Context, owner, taskID string) {
	writeTaskRelationsFromDeps(ctx, s.deps, owner, taskID)
}

// GetTaskState returns the current state and cost counters for a task;
// returns NotFound when the task does not exist under the given tenant and
// PermissionDenied unless the caller owns the task, may approve it, or is a
// tenant admin (M-01).
func (s *BrokerService) GetTaskState(ctx context.Context, req *brokerv1.GetTaskStateRequest) (*brokerv1.TaskState, error) {
	ctx, span := tracer.Start(ctx, "broker.task.get_state")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, req.TenantId, "")
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

	// Owner, an approver of this task (so they can see what they decide), or a
	// tenant admin. An FGA error fails closed.
	if t.OwnerUserID != user {
		ok, ferr := s.deps.Policy.CheckFGA(ctx, "user:"+user, "can_approve", "task:"+req.TaskId)
		switch {
		case ferr != nil:
			err = status.Error(codes.Internal, "authorization check failed")
		case !ok:
			err = s.requireTenantAdmin(ctx, user)
		}
		if err != nil {
			emitTaskAudit(ctx, s.deps, span.SpanContext().TraceID().String(), tenant, req.TaskId, user,
				"aikonos.broker.task.get_denied", auditv1.PolicyDecision_DENY, t)
			if status.Code(err) == codes.PermissionDenied {
				return nil, status.Error(codes.PermissionDenied, "only the task owner, an approver, or a tenant admin may read this task")
			}
			return nil, err
		}
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

	stepUp, err := stepUpRule(ctx, s.configFor(), tenant)
	if err != nil {
		return nil, err
	}
	respStatus, capTokens, eventType, decision, err := approvalsvc.Resolve(
		ctx, s.deps.Tasks, s.deps.Policy, approvalMinter(s.deps.Capability, s.toolReg()), s.deps.Logger,
		tenant, req.TaskId, approver,
		req.Approved, req.Reason, req.ArgsDigest, stepUp,
	)
	if err != nil {
		if approvalsvc.IsStepUpRequired(err) {
			task, _ := s.deps.Tasks.Get(ctx, tenant, req.TaskId)
			emitStepUpRefused(ctx, s.deps, traceID, tenant, req.TaskId, approver, task)
		}
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

// stepUpRule reads the tenant's step-up rule (AG-13). A config read failure
// refuses the approval rather than guessing a laxer rule.
func stepUpRule(ctx context.Context, cfg Config, tenant string) (approvalsvc.StepUp, error) {
	acr, err := cfg.GetString(ctx, tenant, "approvals.step_up_acr")
	if err != nil {
		return approvalsvc.StepUp{}, status.Error(codes.Internal, "failed to load step-up settings")
	}
	mins, err := cfg.GetInt(ctx, tenant, "approvals.step_up_max_age_minutes")
	if err != nil {
		return approvalsvc.StepUp{}, status.Error(codes.Internal, "failed to load step-up settings")
	}
	return approvalsvc.StepUp{ACR: acr, MaxAge: time.Duration(mins) * time.Minute}, nil
}

// emitStepUpRefused records a yes refused for want of a recent MFA sign-in
// (AG-13); the vote was not counted.
func emitStepUpRefused(ctx context.Context, d Deps, traceID, tenant, taskID, approver string, task *db.Task) {
	const eventType = "aikonos.broker.task.step_up_required"
	if err := d.Audit.Emit(ctx, withOBO(&auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: approver,
		EventType:   eventType,
		ResourceRef: "aikonos:task:" + taskID,
		Decision:    auditv1.PolicyDecision_DENY,
	}, task)); err != nil {
		audit.RecordEmitFailure(ctx, d.Logger, err, eventType)
	}
}

// emitTaskAudit records a task-scoped decision stamped with the task's
// on-behalf-of chain. Fire-and-forget; nil-guards deps.Audit.
func emitTaskAudit(ctx context.Context, d Deps, traceID, tenant, taskID, actor, eventType string, decision auditv1.PolicyDecision, task *db.Task) {
	if d.Audit == nil {
		return
	}
	if err := d.Audit.Emit(ctx, withOBO(&auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: actor,
		EventType:   eventType,
		ResourceRef: "aikonos:task:" + taskID,
		Decision:    decision,
	}, task)); err != nil {
		audit.RecordEmitFailure(ctx, d.Logger, err, eventType)
	}
}

// ListPendingApprovals returns the pending, unexpired approval requests the
// caller may decide (AG-07): the same approver_set and FGA can_approve rule
// ApproveTask applies, so every row listed can be approved by this caller with
// the args_digest it carries.
func (s *BrokerService) ListPendingApprovals(ctx context.Context, req *brokerv1.ListPendingApprovalsRequest) (*brokerv1.ListPendingApprovalsResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.approval.list_pending")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	rows, err := s.deps.Tasks.ListPendingApprovalsFor(ctx, tenant, user)
	if err != nil {
		s.deps.Logger.Error("ListPendingApprovals: query failed", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "failed to list approvals")
	}

	// ponytail: one FGA check and one steps read per row (rows capped at 100);
	// batch them if approvers routinely see long queues.
	out := make([]*brokerv1.PendingApproval, 0, len(rows))
	for _, ar := range rows {
		taskID := ar.TaskID.String()
		if ok, ferr := s.deps.Policy.CheckFGA(ctx, "user:"+user, "can_approve", "task:"+taskID); ferr != nil || !ok {
			continue
		}
		steps, serr := s.deps.Tasks.ListMintableSteps(ctx, tenant, taskID)
		if serr != nil {
			s.deps.Logger.Error("ListPendingApprovals: list steps failed", zap.Error(serr))
			return nil, status.Errorf(codes.Internal, "failed to load plan steps")
		}
		pa := &brokerv1.PendingApproval{
			ApprovalId:  ar.ApprovalID.String(),
			TaskId:      taskID,
			RequesterId: ar.RequesterID,
			RequiresN:   int32(ar.RequiresN),
			Approvals:   int32(len(ar.ApprovedBy)),
			ExpiresAt:   timestamppb.New(ar.ExpiresAt),
			ArgsDigest:  approvalsvc.ArgsDigest(steps),
		}
		pa.StepUp, _ = ar.Summary["step_up"].(bool)
		for _, st := range steps {
			args, _ := structpb.NewStruct(st.Args) // decoded from JSON, so always convertible
			pa.Steps = append(pa.Steps, &brokerv1.PendingApprovalStep{Seq: st.Seq, ToolId: st.ToolID, Args: args})
		}
		out = append(out, pa)
	}
	return &brokerv1.ListPendingApprovalsResponse{Approvals: out}, nil
}

// CancelTask transitions a task to CANCELLED (withdrawing a pending approval)
// and tears down its sandbox; returns FailedPrecondition if the task is
// already in a terminal state.
func (s *BrokerService) CancelTask(ctx context.Context, req *brokerv1.CancelTaskRequest) (*brokerv1.CancelTaskResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.task.cancel")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, req.TenantId, "")
	if err != nil {
		return nil, err
	}
	if len(req.Reason) > approvalsvc.MaxReasonBytes {
		return nil, status.Errorf(codes.InvalidArgument, "reason longer than %d bytes", approvalsvc.MaxReasonBytes)
	}
	traceID := span.SpanContext().TraceID().String()

	tasks := s.deps.Tasks

	// Load current state to find the valid transition source
	t, err := tasks.Get(ctx, tenant, req.TaskId)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "task %s not found", req.TaskId)
		}
		return nil, status.Errorf(codes.Internal, "failed to get task")
	}

	// Only the task owner (the subject, also for a delegated task) or a tenant
	// admin may cancel. A delegator has no standing over the recipient's task.
	if t.OwnerUserID != user {
		if err := s.requireTenantAdmin(ctx, user); err != nil {
			emitTaskAudit(ctx, s.deps, traceID, tenant, req.TaskId, user,
				"aikonos.broker.task.cancel_denied", auditv1.PolicyDecision_DENY, t)
			if status.Code(err) == codes.PermissionDenied {
				return nil, status.Error(codes.PermissionDenied, "only the task owner or a tenant admin may cancel this task")
			}
			return nil, err
		}
	}

	// A task waiting on approval: close its gate first, so no approver can
	// decide a request that is being withdrawn. This is how the requester
	// withdraws their own pending request (the chat's Withdraw).
	if t.State == db.TaskStateAwaitingApproval {
		withdrawn, err := tasks.WithdrawApproval(ctx, tenant, req.TaskId, user)
		if err != nil {
			s.deps.Logger.Error("CancelTask: withdraw approval failed", zap.Error(err))
			return nil, status.Error(codes.Internal, "failed to withdraw the pending approval")
		}
		if withdrawn {
			emitTaskAudit(ctx, s.deps, traceID, tenant, req.TaskId, user,
				"aikonos.broker.approval.withdrawn", auditv1.PolicyDecision_DENY, t)
		}
	}

	if err := tasks.Transition(ctx, tenant, req.TaskId, t.State, db.TaskStateCancelled); err != nil {
		s.deps.Logger.Error("CancelTask: transition failed", zap.Error(err))
		return nil, status.Errorf(codes.FailedPrecondition, "cannot cancel task in state %s: %v", t.State, err)
	}
	emitTaskAudit(ctx, s.deps, traceID, tenant, req.TaskId, user,
		"aikonos.broker.task.cancelled", auditv1.PolicyDecision_ALLOW, t)

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
