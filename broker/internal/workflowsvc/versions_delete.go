package workflowsvc

import (
	"context"
	"strconv"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ListVersions lists all versions of a lineage, newest first. Skill-gated by
// the wrapper; caller establishes tenantID from its trust anchor. Was
// listWorkflowVersionsCore.
func ListVersions(
	ctx context.Context,
	tenantID string,
	req *brokerv1.ListWorkflowVersionsRequest,
	store Store,
	logger *zap.Logger,
) (*brokerv1.ListWorkflowVersionsResponse, error) {
	if store == nil {
		return nil, status.Error(codes.FailedPrecondition, "workflow storage not configured")
	}

	lineageID, err := uuid.Parse(req.LineageId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid lineage_id: %v", err)
	}

	// F19: additive pagination, following the audit-reader cursor convention
	// (proto/broker.proto QueryAuditRequest / audit/reader.go filterAndPage).
	// The cursor encodes a typed value — the version number — so, deliberately
	// deviating from the audit reader's laissez-faire acceptance of any string, a
	// non-empty cursor that fails to parse as an integer is rejected with
	// InvalidArgument rather than silently mis-filtering.
	beforeVersion := 0
	if req.Cursor != "" {
		cv, cerr := strconv.Atoi(req.Cursor)
		if cerr != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid cursor: %v", cerr)
		}
		beforeVersion = cv
	}

	// Bounding is pushed into SQL (keyset on beforeVersion + LIMIT). Fetch one
	// extra row (limit+1) so the in-memory truncate below can still tell whether
	// more rows remain for next_cursor. limit=0 stays unbounded (legacy).
	fetchLimit := 0
	if req.Limit > 0 {
		fetchLimit = int(req.Limit) + 1
	}
	rows, err := store.ListVersions(ctx, tenantID, lineageID, beforeVersion, fetchLimit)
	if err != nil {
		logger.Error("ListWorkflowVersions: ListVersions failed",
			zap.String("lineage_id", req.LineageId),
			zap.Error(err),
		)
		return nil, status.Errorf(codes.Internal, "failed to list workflow versions")
	}

	var nextCursor string
	if req.Limit > 0 && len(rows) > int(req.Limit) {
		nextCursor = strconv.Itoa(rows[req.Limit-1].Version)
		rows = rows[:req.Limit]
	}

	items := make([]*brokerv1.WorkflowVersionSummary, 0, len(rows))
	for _, r := range rows {
		items = append(items, &brokerv1.WorkflowVersionSummary{
			Version:       int32(r.Version),
			ApprovalState: r.ApprovalState,
			CreatedAt:     r.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return &brokerv1.ListWorkflowVersionsResponse{Items: items, NextCursor: nextCursor}, nil
}

// Delete removes an entire lineage (all versions + per-user pins). Owner-only.
// Deleting a published lineage also withdraws it from every group it was
// shared with — the webui confirms this consequence before calling. Was
// deleteCore.
func Delete(
	ctx context.Context,
	tenantID, ownerUserID string,
	req *brokerv1.DeleteWorkflowRequest,
	store Store,
	emitter AuditEmitter,
	logger *zap.Logger,
) (*brokerv1.DeleteWorkflowResponse, error) {
	if store == nil {
		return nil, status.Error(codes.FailedPrecondition, "workflow storage not configured")
	}

	lineageID, err := uuid.Parse(req.LineageId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid lineage_id: %v", err)
	}

	// Verify caller == lineage owner (same owner-lookup pattern as propose/decide/publish).
	current, err := store.GetCurrent(ctx, tenantID, lineageID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "workflow lineage not found: %v", err)
	}
	if current.OwnerUserID != ownerUserID {
		return nil, status.Error(codes.PermissionDenied, "only the lineage owner may delete a workflow")
	}

	deleted, err := store.DeleteLineage(ctx, tenantID, lineageID)
	if err != nil {
		logger.Error("Delete: DeleteLineage failed",
			zap.String("lineage", lineageID.String()),
			zap.Error(err),
		)
		return nil, status.Errorf(codes.Internal, "failed to delete workflow")
	}

	if emitErr := emitter.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TenantId:    tenantID,
		OccurredAt:  timestamppb.New(time.Now().UTC()),
		ActorUserId: ownerUserID,
		EventType:   "aikonos.broker.workflow.deleted",
		ResourceRef: "aikonos:workflow-lineage:" + lineageID.String(),
		Decision:    auditv1.PolicyDecision_ALLOW,
	}); emitErr != nil {
		audit.RecordEmitFailure(ctx, logger, emitErr, "aikonos.broker.workflow.deleted")
	}

	return &brokerv1.DeleteWorkflowResponse{VersionsDeleted: int32(deleted)}, nil
}
