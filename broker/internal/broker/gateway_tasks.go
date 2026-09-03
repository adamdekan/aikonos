package broker

// gateway_tasks.go — south-bound task twins for the agent-gateway (CP2).
//
// CreateGatewayTask and ApproveGatewayTask mirror the north-bound CreateTask /
// ApproveTask RPCs but are SPIFFE-gated to the gateway SVID.  The gateway
// asserts the owner because it holds the verified user identity from the
// interactive session; the SPIFFE gate makes that assertion trustworthy.
//
// persistManagedTask is the shared helper called by both the north-bound and
// south-bound create handlers. The shared approve-and-mint logic moved to
// approvalsvc.Resolve (CP4/C6) — ApproveTask/ApproveGatewayTask call it
// directly instead of a package-local helper.

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/adamdekan/aikonos/broker/internal/approvalsvc"
	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/gatewaygrant"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ── shared helper: persistManagedTask ────────────────────────────────────────

// persistManagedTask inserts a pre-built Task record, emits the creation audit
// event, seeds the owner/approver OpenFGA relations, and publishes the CREATED
// event.  Called by both north CreateTask and south CreateGatewayTask so the
// two paths stay identical.
func persistManagedTask(
	ctx context.Context,
	deps Deps,
	store taskStore,
	t *db.Task,
	traceID string,
) (*brokerv1.TaskHandle, error) {
	if err := store.Create(ctx, t); err != nil {
		if errors.Is(err, db.ErrClientRequestIDExists) {
			// Idempotent replay: a task with this (tenant, client_request_id) pair
			// already exists. store.Create already mutated t.TaskID/t.TraceID to
			// the existing row. Re-run the FGA relation write as a check-and-repair
			// — if the original call crashed after the INSERT committed but before
			// writeTaskRelationsFromDeps ran, the task would otherwise be
			// permanently missing its owner/approver tuples (unapprovable) on every
			// future replay. This is safe to repeat: fgaClient.write treats a
			// duplicate tuple (OpenFGA's "already_exists"/"write_failed_*" error
			// codes) as benign (see policy/fga.go, pinned by
			// TestWriteRelations_AlreadyExistsIsOK). Audit emission and event
			// publish are NOT re-run — those already happened on the original
			// create and must not be duplicated.
			writeTaskRelationsFromDeps(ctx, deps, t.OwnerUserID, t.TaskID.String())
			deps.Logger.Info("task create idempotent replay",
				zap.String("task_id", t.TaskID.String()),
				zap.String("tenant_id", t.TenantID.String()),
			)
			return &brokerv1.TaskHandle{TaskId: t.TaskID.String(), TraceId: t.TraceID}, nil
		}
		deps.Logger.Error("persistManagedTask: DB insert failed", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "failed to create task")
	}

	deps.Logger.Info("task created",
		zap.String("task_id", t.TaskID.String()),
		zap.String("tenant_id", t.TenantID.String()),
		zap.String("user_id", t.OwnerUserID),
		zap.String("trace_id", traceID),
		zap.Bool("gateway_managed", t.GatewayManaged),
	)

	if err := deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    t.TenantID.String(),
		OccurredAt:  timestamppb.New(time.Now().UTC()),
		ActorUserId: t.OwnerUserID,
		EventType:   "aikonos.broker.task.created",
		ResourceRef: "aikonos:task:" + t.TaskID.String(),
		Decision:    auditv1.PolicyDecision_ALLOW,
	}); err != nil {
		audit.RecordEmitFailure(ctx, deps.Logger, err, "aikonos.broker.task.created")
	}

	writeTaskRelationsFromDeps(ctx, deps, t.OwnerUserID, t.TaskID.String())

	publishTaskEvent(ctx, deps, t.TenantID.String(), t.TaskID.String(), "STATUS_CHANGED", map[string]any{
		"status": brokerv1.TaskStatus_CREATED.String(),
	})

	return &brokerv1.TaskHandle{TaskId: t.TaskID.String(), TraceId: traceID}, nil
}

// writeTaskRelationsFromDeps is the package-level equivalent of
// BrokerService.writeTaskRelations so persistManagedTask can call it without a
// receiver.
func writeTaskRelationsFromDeps(ctx context.Context, deps Deps, owner, taskID string) {
	if err := deps.Policy.WriteRelations(ctx,
		policy.Relation{User: "user:" + owner, Relation: "owner", Object: "task:" + taskID},
		policy.Relation{User: "user:" + owner, Relation: "approver", Object: "task:" + taskID},
	); err != nil {
		deps.Logger.Warn("writeTaskRelations failed", zap.String("task_id", taskID), zap.Error(err))
	}
}

// ── SandboxService: south twin RPCs ──────────────────────────────────────────

// CreateGatewayTask creates a gateway-managed task on behalf of a user.
// SPIFFE-gated to the gateway SVID.  The task owner is bound from the
// broker-issued owner-grant token (req.OwnerGrant), NOT from req.OwnerUserId —
// req.OwnerUserId is kept for wire compat / logging only.
func (s *SandboxService) CreateGatewayTask(ctx context.Context, req *brokerv1.CreateGatewayTaskRequest) (*brokerv1.TaskHandle, error) {
	ctx, span := tracer.Start(ctx, "broker.task.create_gateway")
	defer span.End()

	if err := s.requireGatewayPeer(ctx); err != nil {
		return nil, err
	}

	tenantID, err := uuid.Parse(req.TenantId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid tenant_id: %v", err)
	}
	if req.Prompt == "" {
		return nil, status.Error(codes.InvalidArgument, "prompt required")
	}

	// Require a broker-issued owner grant — without one the gateway could assert
	// any owner and impersonate arbitrary users (the pre-CP2 attack vector).
	if req.OwnerGrant == "" {
		return nil, status.Error(codes.PermissionDenied, "owner grant required")
	}
	grantTenant, grantOwner, grantErr := gatewaygrant.Verify(s.deps.GatewayGrantKey, req.OwnerGrant)
	if grantErr != nil {
		return nil, status.Errorf(codes.PermissionDenied, "invalid owner grant: %v", grantErr)
	}
	if grantTenant != req.TenantId {
		return nil, status.Error(codes.PermissionDenied, "owner grant tenant mismatch")
	}

	traceID := span.SpanContext().TraceID().String()
	span.SetAttributes(
		attribute.String("tenant.id", req.TenantId),
		attribute.String("user.id", grantOwner),
	)

	budget := int64(req.CostBudget)
	if budget == 0 {
		budget = 1000
	}

	var agentID *uuid.UUID
	if req.AgentId != "" {
		parsed, parseErr := uuid.Parse(req.AgentId)
		if parseErr == nil {
			agentID = &parsed
		}
		// Invalid / non-UUID agent_id → treat as unbound (NULL). A malformed id
		// cannot satisfy the skill-boundary check at SubmitPlan — the agent lookup
		// will fail and the plan will be denied then. We do not error here so that
		// callers with an empty-string agent_id (no agent) pass through cleanly.
	}

	t := &db.Task{
		TaskID:          uuid.New(),
		TenantID:        tenantID,
		OwnerUserID:     grantOwner, // bound from the verified grant, not the asserted field
		Prompt:          req.Prompt,
		CostBudget:      budget,
		TraceID:         traceID,
		GatewayManaged:  true, // south task twins are always gateway-driven
		AgentID:         agentID,
		ClientRequestID: req.ClientRequestId,
	}

	return persistManagedTask(ctx, s.deps, s.deps.Tasks, t, traceID)
}

// ApproveGatewayTask approves a gateway-managed task and mints per-step tokens.
// SPIFFE-gated to the gateway SVID.  The approver is derived from the stored
// task row (task.OwnerUserID), NOT from req.OwnerUserId — req.OwnerUserId is
// kept for wire compat only and is ignored for authorization.
func (s *SandboxService) ApproveGatewayTask(ctx context.Context, req *brokerv1.ApproveGatewayTaskRequest) (*brokerv1.ApproveTaskResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.task.approve_gateway")
	defer span.End()
	traceID := span.SpanContext().TraceID().String()

	if err := s.requireGatewayPeer(ctx); err != nil {
		return nil, err
	}
	if req.TaskId == "" {
		return nil, status.Error(codes.InvalidArgument, "task_id required")
	}
	if req.TenantId == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id required")
	}

	// Derive the approver from the stored task row — the gateway cannot assert
	// an arbitrary approver (req.OwnerUserId is intentionally ignored for authz).
	store := s.deps.Tasks
	task, err := store.Get(ctx, req.TenantId, req.TaskId)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || task == nil {
			return nil, status.Errorf(codes.FailedPrecondition, "task %s not found", req.TaskId)
		}
		// Already-coded gRPC status errors (e.g. from the fake store in tests)
		// pass through unchanged so callers see the original code.
		if c := status.Code(err); c != codes.OK && c != codes.Unknown {
			return nil, err
		}
		s.deps.Logger.Error("ApproveGatewayTask: load task failed", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "failed to load task")
	}
	approverUserID := task.OwnerUserID

	respStatus, capTokens, eventType, decision, err := approvalsvc.Resolve(
		ctx, store, s.deps.Policy, approvalMinter(s.deps.Capability, s.toolReg()), s.deps.Logger,
		req.TenantId, req.TaskId, approverUserID,
		req.Approved, req.Reason,
	)
	if err != nil {
		return nil, err
	}

	s.deps.Logger.Info("ApproveGatewayTask",
		zap.String("task_id", req.TaskId),
		zap.Bool("approved", req.Approved),
		zap.String("owner", approverUserID),
		zap.String("new_status", respStatus.String()),
	)

	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    req.TenantId,
		OccurredAt:  timestamppb.New(time.Now().UTC()),
		ActorUserId: approverUserID,
		EventType:   eventType,
		ResourceRef: "aikonos:task:" + req.TaskId,
		Decision:    decision,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, eventType)
	}

	publishTaskEvent(ctx, s.deps, req.TenantId, req.TaskId, "APPROVAL_RECEIVED", map[string]any{
		"approved":   req.Approved,
		"approver":   approverUserID,
		"resolved":   true,
		"new_status": respStatus.String(),
	})

	return &brokerv1.ApproveTaskResponse{
		Success:            true,
		NewStatus:          respStatus,
		CapabilityTokenIds: capTokens,
	}, nil
}
