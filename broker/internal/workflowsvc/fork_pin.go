package workflowsvc

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// Fork creates a new independent lineage (v1) copied from the source
// lineage's current approved definition. The forker must be able to see the
// source: allowed when source.OwnerUserID == forker, OR when the source is
// published and the forker is a member of one of its visibility_groups (via
// checkFGA). The fork is private, owned by forker.
//
// Uses CreateVersion (routes through scanWorkflow internally) — no inline scan.
// Was forkCore.
func Fork(
	ctx context.Context,
	tenantID, forkerUserID string,
	req *brokerv1.ForkWorkflowRequest,
	store Store,
	checkFGA func(ctx context.Context, user, relation, object string) (bool, error),
	emitter AuditEmitter,
	logger *zap.Logger,
) (*brokerv1.ForkWorkflowResponse, error) {
	if req.NewName == "" {
		return nil, status.Error(codes.InvalidArgument, "new_name required")
	}
	if store == nil {
		return nil, status.Error(codes.FailedPrecondition, "workflow storage not configured")
	}

	srcLineageID, err := uuid.Parse(req.SourceLineageId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid source_lineage_id: %v", err)
	}
	tenantUUID, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid tenant_id: %v", err)
	}

	// Load the source's current approved version.
	src, err := store.GetCurrent(ctx, tenantID, srcLineageID)
	if err != nil {
		// Intentionally opaque: don't reveal whether the lineage exists at all
		// to callers who may not have visibility.
		return nil, status.Error(codes.PermissionDenied, "source workflow not visible or not found")
	}

	// Visibility check: owner may always fork their own workflow. For shared
	// (published) workflows, check that the forker is a member of at least one
	// visibility_group. For private workflows owned by someone else, deny.
	if src.OwnerUserID != forkerUserID {
		if src.Status != "published" {
			return nil, status.Error(codes.PermissionDenied, "source workflow not visible")
		}
		// Parse the visibility_groups JSONB array.
		var groups []string
		if jsonErr := json.Unmarshal(src.VisibilityGroups, &groups); jsonErr != nil {
			return nil, status.Error(codes.PermissionDenied, "source workflow visibility_groups unreadable")
		}
		if len(groups) == 0 {
			return nil, status.Error(codes.PermissionDenied, "source workflow has no visibility groups")
		}
		if checkFGA == nil {
			// FGA disabled — allow-all dev posture: any published workflow is forkable.
		} else {
			memberOfAny := false
			for _, g := range groups {
				ok, fgaErr := checkFGA(ctx, "user:"+forkerUserID, "member", "group:"+g)
				if fgaErr != nil {
					logger.Warn("Fork: group membership CheckFGA error — failing closed",
						zap.String("user", forkerUserID),
						zap.String("group", g),
						zap.Error(fgaErr),
					)
					return nil, status.Error(codes.PermissionDenied, "source workflow not visible")
				}
				if ok {
					memberOfAny = true
					break
				}
			}
			if !memberOfAny {
				return nil, status.Error(codes.PermissionDenied, "source workflow not visible")
			}
		}
	}

	// Build the fork row: fresh lineage, parent_lineage_id = source, owner = forker,
	// private, name = new_name, definition copied from source current.
	freshLineage := uuid.New()
	visGroups, _ := json.Marshal([]string{})
	forkRow := db.WorkflowRow{
		LineageID:        freshLineage,
		TenantID:         tenantUUID,
		OwnerUserID:      forkerUserID,
		Name:             req.NewName,
		Description:      src.Description,
		Status:           "private",
		VisibilityKind:   "private",
		VisibilityGroups: json.RawMessage(visGroups),
		Definition:       src.Definition,
		ParentLineageID:  &srcLineageID,
		// Agent binding is lineage-immutable — a fork inherits the source's
		// binding into the new lineage's v1 (F9).
		BoundAgentID: src.BoundAgentID,
		// ApprovalState defaults to 'approved' inside CreateVersion when empty.
	}

	created, err := store.CreateVersion(ctx, tenantID, forkRow)
	if err != nil {
		logger.Error("Fork: CreateVersion failed",
			zap.String("src_lineage", srcLineageID.String()),
			zap.String("forker", forkerUserID),
			zap.Error(err),
		)
		return nil, status.Errorf(codes.Internal, "failed to create fork")
	}

	if emitErr := emitter.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TenantId:    tenantID,
		OccurredAt:  timestamppb.New(time.Now().UTC()),
		ActorUserId: forkerUserID,
		EventType:   "aikonos.broker.workflow.forked",
		ResourceRef: "aikonos:workflow-lineage:" + created.LineageID.String(),
		Decision:    auditv1.PolicyDecision_ALLOW,
	}); emitErr != nil {
		audit.RecordEmitFailure(ctx, logger, emitErr, "aikonos.broker.workflow.forked")
	}

	return &brokerv1.ForkWorkflowResponse{LineageId: created.LineageID.String()}, nil
}

// Pin validates that the target version is approved, then sets the pin. A pin
// must never target a proposed/rejected version. Was pinCore.
func Pin(
	ctx context.Context,
	tenantID, userID string,
	req *brokerv1.SetWorkflowVersionPinRequest,
	store Store,
	emitter AuditEmitter,
	logger *zap.Logger,
) (*brokerv1.SetWorkflowVersionPinResponse, error) {
	if store == nil {
		return nil, status.Error(codes.FailedPrecondition, "workflow storage not configured")
	}

	lineageID, err := uuid.Parse(req.LineageId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid lineage_id: %v", err)
	}

	// Validate the target version exists and is approved.
	row, err := store.GetVersion(ctx, tenantID, lineageID, int(req.Version))
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "workflow version not found: %v", err)
	}
	if row.ApprovalState != "approved" {
		return nil, status.Errorf(codes.FailedPrecondition,
			"cannot pin version %d: approval_state is %q (only approved versions may be pinned)",
			req.Version, row.ApprovalState)
	}

	if err := store.SetVersionPin(ctx, tenantID, userID, lineageID, int(req.Version)); err != nil {
		logger.Error("Pin: SetVersionPin failed", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "failed to set version pin")
	}

	if emitErr := emitter.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TenantId:    tenantID,
		OccurredAt:  timestamppb.New(time.Now().UTC()),
		ActorUserId: userID,
		EventType:   "aikonos.broker.workflow.version_pinned",
		ResourceRef: "aikonos:workflow-lineage:" + lineageID.String(),
		Decision:    auditv1.PolicyDecision_ALLOW,
	}); emitErr != nil {
		audit.RecordEmitFailure(ctx, logger, emitErr, "aikonos.broker.workflow.version_pinned")
	}

	return &brokerv1.SetWorkflowVersionPinResponse{}, nil
}

// ClearPin removes the caller's version pin for a lineage. Was clearPinCore.
func ClearPin(
	ctx context.Context,
	tenantID, userID string,
	req *brokerv1.ClearWorkflowVersionPinRequest,
	store Store,
	emitter AuditEmitter,
	logger *zap.Logger,
) (*brokerv1.ClearWorkflowVersionPinResponse, error) {
	if store == nil {
		return nil, status.Error(codes.FailedPrecondition, "workflow storage not configured")
	}

	lineageID, err := uuid.Parse(req.LineageId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid lineage_id: %v", err)
	}

	if err := store.ClearVersionPin(ctx, tenantID, userID, lineageID); err != nil {
		logger.Error("ClearPin: ClearVersionPin failed", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "failed to clear version pin")
	}

	if emitErr := emitter.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TenantId:    tenantID,
		OccurredAt:  timestamppb.New(time.Now().UTC()),
		ActorUserId: userID,
		EventType:   "aikonos.broker.workflow.version_unpinned",
		ResourceRef: "aikonos:workflow-lineage:" + lineageID.String(),
		Decision:    auditv1.PolicyDecision_ALLOW,
	}); emitErr != nil {
		audit.RecordEmitFailure(ctx, logger, emitErr, "aikonos.broker.workflow.version_unpinned")
	}

	return &brokerv1.ClearWorkflowVersionPinResponse{}, nil
}
