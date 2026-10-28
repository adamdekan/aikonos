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
	"go.opentelemetry.io/otel/trace"
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

	if err := deps.Audit.Emit(ctx, withOBO(&auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    t.TenantID.String(),
		OccurredAt:  timestamppb.New(time.Now().UTC()),
		ActorUserId: t.OwnerUserID,
		EventType:   "aikonos.broker.task.created",
		ResourceRef: "aikonos:task:" + t.TaskID.String(),
		Decision:    auditv1.PolicyDecision_ALLOW,
	}, t)); err != nil {
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
	// AG-01: agent_id is a bare agent UUID or empty. A non-UUID (the retired
	// "<user>-agent" pseudo-id) is refused, never coerced to an unbound task.
	if req.AgentId != "" {
		if _, err := uuid.Parse(req.AgentId); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid agent_id: %v", err)
		}
	}

	// Require a broker-issued owner grant — without one the gateway could assert
	// any owner and impersonate arbitrary users (the pre-CP2 attack vector).
	grant, err := s.verifyOwnerGrant(ctx, req.TenantId, req.OwnerGrant)
	if err != nil {
		return nil, err
	}
	grantOwner := grant.Owner()
	// AG-17: a grant naming an actor fixes the agent; the gateway may echo it
	// (any UUID spelling) or leave agent_id empty, nothing else.
	if grant.Actor != "" && req.AgentId != "" {
		asked, errA := uuid.Parse(req.AgentId)
		bound, errB := uuid.Parse(grant.Actor)
		if errA != nil || errB != nil || asked != bound {
			s.emitGrantDenied(ctx, req.TenantId, grantOwner, grant.Actor, "grant_actor_mismatch",
				map[string]any{"grant_actor": grant.Actor, "requested_agent": req.AgentId})
			return nil, status.Error(codes.PermissionDenied, "agent_id does not match owner grant actor")
		}
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

	// AG-01: every gateway task binds a registered agent. The grant's actor
	// fixes it; otherwise the requested agent (which the grant owner must be
	// able to use) or the owner's default agent.
	agentRef := grant.Actor
	if agentRef == "" {
		if s.deps.Agents == nil {
			return nil, status.Error(codes.FailedPrecondition, "agents are not enabled")
		}
		a, reason, rerr := runAgent(ctx, s.deps, req.TenantId, grantOwner, req.AgentId)
		if rerr != nil {
			s.deps.Logger.Error("CreateGatewayTask: agent store failed", zap.Error(rerr))
			return nil, status.Error(codes.Unavailable, "agent could not be resolved; retry")
		}
		if reason != "" {
			s.emitGrantDenied(ctx, req.TenantId, grantOwner, req.AgentId, reason, nil)
			return nil, runAgentErr(reason, req.AgentId)
		}
		agentRef = a.ID.String()
	}
	parsedAgent, err := uuid.Parse(agentRef)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid grant actor: %v", err)
	}
	agentID := &parsedAgent

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
	if id, err := uuid.Parse(grant.GrantID); err == nil {
		switch grant.Kind {
		case gatewaygrant.KindScheduled:
			t.GrantID = &id
		case gatewaygrant.KindExternal:
			t.ExternalClientID = &id
		}
	}
	// Workflow and interactive grants leave no other trace on the row; stamp
	// the kind so OnBehalfOf can tell them apart (scheduled/external derive).
	if grant.Kind == gatewaygrant.KindWorkflow || grant.Kind == gatewaygrant.KindInteractive {
		kind := grant.Kind
		t.OBOKind = &kind
	}

	return persistManagedTask(ctx, s.deps, s.deps.Tasks, t, traceID)
}

// verifyOwnerGrant requires an owner grant, verifies it with the configured key
// and v1 policy, and checks it names tenantID. Every rejection is audited
// (invalid_grant / grant_tenant_mismatch / grant_v1_refused /
// grant_actor_missing / subject_blocked / grant_revoked / agent_<status>) and returned as
// PermissionDenied; grant_v1_used is
// logged when a legacy grant is accepted.
// ponytail: plain log per v1 use; rate-limit per gateway SVID if it gets noisy.
func (s *SandboxService) verifyOwnerGrant(ctx context.Context, tenantID, token string) (gatewaygrant.Grant, error) {
	if token == "" {
		return gatewaygrant.Grant{}, status.Error(codes.PermissionDenied, "owner grant required")
	}
	g, err := gatewaygrant.Verify(s.deps.GatewayGrantKey, token, s.deps.GatewayGrantAcceptV1)
	if err != nil {
		s.emitGrantDenied(ctx, tenantID, "", "", "invalid_grant", map[string]any{"error": err.Error()})
		return gatewaygrant.Grant{}, status.Errorf(codes.PermissionDenied, "invalid owner grant: %v", err)
	}
	if g.TenantID != tenantID {
		s.emitGrantDenied(ctx, tenantID, g.Owner(), g.Actor, "grant_tenant_mismatch",
			map[string]any{"grant_tenant": g.TenantID})
		return gatewaygrant.Grant{}, status.Error(codes.PermissionDenied, "owner grant tenant mismatch")
	}
	// AG-17 S8: identity.obo=enforce refuses a grant with no actor: a v1 grant
	// (accept_v1 forced off) or a scheduled grant minted while in audit mode.
	// Read only for those, off the hot path.
	if g.Kind == gatewaygrant.KindLegacy || (g.Kind == gatewaygrant.KindScheduled && g.Actor == "") {
		mode, cerr := s.cfg().GetString(ctx, g.TenantID, "identity.obo")
		if cerr != nil {
			s.emitGrantDenied(ctx, tenantID, g.Owner(), "", "settings_check_failed", map[string]any{"error": cerr.Error()})
			return gatewaygrant.Grant{}, status.Error(codes.Unavailable, "tenant settings could not be read")
		}
		if mode == "enforce" {
			reason := "grant_v1_refused"
			if g.Kind == gatewaygrant.KindScheduled {
				reason = "grant_actor_missing"
			}
			s.emitGrantDenied(ctx, tenantID, g.Owner(), "", reason, map[string]any{"mode": mode})
			return gatewaygrant.Grant{}, status.Error(codes.PermissionDenied, "owner grant refused: identity.obo=enforce requires a v2 grant naming an actor")
		}
		if g.Kind == gatewaygrant.KindLegacy {
			s.deps.Logger.Info("grant_v1_used", zap.String("tenant_id", g.TenantID))
		}
	}
	// AG-06: a blocked subject (or tenant) loses every outstanding grant, and
	// a revoke-all on the subject or actor voids grants issued before it. The
	// token carries only its expiry; every mint site uses GatewayGrantTTL.
	// ponytail: issued = expiry - TTL; a TTL change across a restart skews it
	// by the difference until old grants expire.
	st, rerr := s.deps.Revocation.OnBehalfOf(ctx, tenantID, g.Subject, g.Actor)
	if rerr != nil {
		s.deps.Logger.Error("verifyOwnerGrant: revocation state read failed", zap.Error(rerr))
		return gatewaygrant.Grant{}, status.Error(codes.Unavailable, "access state could not be read; retry")
	}
	if st.Blocked {
		s.emitGrantDenied(ctx, tenantID, g.Owner(), g.Actor, "subject_blocked", nil)
		return gatewaygrant.Grant{}, status.Error(codes.PermissionDenied, "owner grant refused: user is blocked")
	}
	if st.Revoked(g.Expires.Add(-s.deps.GatewayGrantTTL)) {
		s.emitGrantDenied(ctx, tenantID, g.Owner(), g.Actor, "grant_revoked", nil)
		return gatewaygrant.Grant{}, status.Error(codes.PermissionDenied, "owner grant revoked")
	}
	// AG-03: a grant naming an actor dies with the actor's activity. An
	// actorless grant binds its agent through runAgent, which checks the same.
	if r := st.AgentRefusal(); r != "" {
		s.emitGrantDenied(ctx, tenantID, g.Owner(), g.Actor, r, nil)
		return gatewaygrant.Grant{}, agentRefusedErr(r)
	}
	return g, nil
}

// emitGrantDenied fires a DENY audit event for an owner-grant rejection.
// Fire-and-forget, nil-guards deps.Audit (same shape as emitToolDenied).
func (s *SandboxService) emitGrantDenied(ctx context.Context, tenantID, owner, actor, reason string, extras map[string]any) {
	if s.deps.Audit == nil {
		return
	}
	fields := map[string]any{"reason": reason}
	for k, v := range extras {
		fields[k] = v
	}
	ctxStruct, _ := structpb.NewStruct(fields)
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:      ids.EventID(),
		TenantId:     tenantID,
		OccurredAt:   timestamppb.New(time.Now().UTC()),
		ActorUserId:  owner,
		ActorAgentId: actor,
		TraceId:      trace.SpanContextFromContext(ctx).TraceID().String(),
		EventType:    "aikonos.broker.task.denied",
		Decision:     auditv1.PolicyDecision_DENY,
		Context:      ctxStruct,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, "aikonos.broker.task.denied")
	}
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
		req.Approved, req.Reason, req.ArgsDigest,
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

	if err := s.deps.Audit.Emit(ctx, withOBO(&auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    req.TenantId,
		OccurredAt:  timestamppb.New(time.Now().UTC()),
		ActorUserId: approverUserID,
		EventType:   eventType,
		ResourceRef: "aikonos:task:" + req.TaskId,
		Decision:    decision,
	}, task)); err != nil {
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

// GetGatewayApproval reports the broker-held state of a gateway task's
// approval (AG-07 slice 2). The gateway's waiting run polls it, so a vote cast
// anywhere (the chat dialog, the Inbox, another session) wakes the run. Once
// the task is APPROVED it mints the step tokens for that run, as the deciding
// vote did. SPIFFE-gated to the gateway SVID, which may already vote as the
// task owner (ApproveGatewayTask), so the tokens grant it nothing more.
func (s *SandboxService) GetGatewayApproval(ctx context.Context, req *brokerv1.GetGatewayApprovalRequest) (*brokerv1.GatewayApproval, error) {
	if err := s.requireGatewayPeer(ctx); err != nil {
		return nil, err
	}
	if req.TaskId == "" || req.TenantId == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id and task_id required")
	}
	store := s.deps.Tasks
	task, err := store.Get(ctx, req.TenantId, req.TaskId)
	if err != nil || task == nil {
		if errors.Is(err, pgx.ErrNoRows) || task == nil {
			return nil, status.Errorf(codes.NotFound, "task %s not found", req.TaskId)
		}
		s.deps.Logger.Error("GetGatewayApproval: load task failed", zap.Error(err))
		return nil, status.Error(codes.Internal, "failed to load task")
	}
	if !task.GatewayManaged {
		return nil, status.Errorf(codes.FailedPrecondition, "task %s is not gateway-managed", req.TaskId)
	}

	out := &brokerv1.GatewayApproval{Status: dbStateToProtoStatus(task.State)}
	switch task.State {
	case db.TaskStateAwaitingApproval:
		ar, err := store.GetPendingApprovalByTask(ctx, req.TenantId, req.TaskId)
		if errors.Is(err, pgx.ErrNoRows) {
			// Past expires_at but not yet swept: no vote can land any more.
			out.Status = brokerv1.TaskStatus_TIMEOUT
			return out, nil
		}
		if err != nil {
			s.deps.Logger.Error("GetGatewayApproval: load approval failed", zap.Error(err))
			return nil, status.Error(codes.Internal, "failed to load approval")
		}
		out.RequiresN = int32(ar.RequiresN)
		out.Approvals = int32(len(ar.ApprovedBy))
		out.StepUp, _ = ar.Summary["step_up"].(bool)
		out.OwnerMayApprove = db.MayVote(ar.ApproverSet, ar.RequesterID, ar.RequiresN, task.OwnerUserID)
	case db.TaskStateApproved:
		steps, err := store.ListMintableSteps(ctx, req.TenantId, req.TaskId)
		if err != nil {
			s.deps.Logger.Error("GetGatewayApproval: list steps failed", zap.Error(err))
			return nil, status.Error(codes.Internal, "failed to load plan steps")
		}
		scopes := make([]approvalsvc.StepScope, 0, len(steps))
		for _, st := range steps {
			scopes = append(scopes, approvalsvc.StepScope{Seq: st.Seq, ToolID: st.ToolID})
		}
		mint := approvalMinter(s.deps.Capability, s.toolReg())
		if out.CapabilityTokenIds, err = mint(task.OwnerUserID, req.TaskId, task.OnBehalfOf(), scopes); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to mint capability tokens: %v", err)
		}
	}
	return out, nil
}
