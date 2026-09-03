package workflowsvc

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
	"github.com/adamdekan/aikonos/broker/internal/workflow"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// Rate records an audit rating signal. When rating == RATING_SUCCESS it also
// stamps success_rated_at on the version as an informational marker (the
// publish gate on it was removed 2026-07-21 — rating no longer affects
// publishability). No proposal is created here — BAD is a signal for the
// owner/agent to act on separately via Propose. publishStore may be nil (e.g.
// tests that don't inject it); the success-stamp is then skipped non-fatally.
// Was rateCore.
func Rate(
	ctx context.Context,
	tenantID, ownerUserID string,
	req *brokerv1.RateWorkflowRunRequest,
	publishStore Store,
	emitter AuditEmitter,
	logger *zap.Logger,
) (*brokerv1.RateWorkflowRunResponse, error) {
	lineageID, err := uuid.Parse(req.LineageId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid lineage_id: %v", err)
	}

	// On SUCCESS, stamp success_rated_at as an informational marker (it no longer
	// gates publish). BAD and unrated versions are never stamped.
	if req.Rating == brokerv1.WorkflowRating_RATING_SUCCESS && publishStore != nil {
		if stampErr := publishStore.MarkSuccessRated(ctx, tenantID, lineageID, int(req.Version)); stampErr != nil {
			// Non-fatal: the audit event still fires, the rating is still recorded.
			// The stamp is idempotent on retry.
			logger.Warn("Rate: MarkSuccessRated failed",
				zap.String("lineage", lineageID.String()),
				zap.Int32("version", req.Version),
				zap.Error(stampErr),
			)
		}
	}

	// Encode rating as a suffix in EventType so the audit query surface can
	// filter by rating without an extra column.
	ratingEventType := "aikonos.broker.workflow.rated." + req.Rating.String()
	if err := emitter.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TenantId:    tenantID,
		OccurredAt:  timestamppb.New(time.Now().UTC()),
		ActorUserId: ownerUserID,
		EventType:   ratingEventType,
		ResourceRef: "aikonos:workflow-lineage:" + lineageID.String(),
		Decision:    auditv1.PolicyDecision_ALLOW,
	}); err != nil {
		audit.RecordEmitFailure(ctx, logger, err, ratingEventType)
	}

	return &brokerv1.RateWorkflowRunResponse{}, nil
}

// Propose inserts a 'proposed' version, notifies the owner via an inbox
// envelope, and emits a workflow.proposed audit event. Was proposeCore.
func Propose(
	ctx context.Context,
	tenantID, ownerUserID string,
	req *brokerv1.ProposeWorkflowVersionRequest,
	ownerStore Store,
	proposeStore Store,
	envStore EnvelopeStore,
	reg *toolregistry.Registry,
	emitter AuditEmitter,
	logger *zap.Logger,
) (*brokerv1.ProposeWorkflowVersionResponse, error) {
	if req.DefinitionJson == "" {
		return nil, status.Error(codes.InvalidArgument, "definition_json required")
	}
	def, wfErr := workflow.FromJSON([]byte(req.DefinitionJson))
	if wfErr != nil {
		return nil, status.Errorf(codes.InvalidArgument, "definition_json invalid: %v", wfErr)
	}
	if err := validateStepSkills(def, reg); err != nil {
		return nil, err
	}

	lineageID, err := uuid.Parse(req.LineageId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid lineage_id: %v", err)
	}
	tenantUUID, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid tenant_id: %v", err)
	}

	if ownerStore == nil || proposeStore == nil {
		return nil, status.Error(codes.FailedPrecondition, "workflow storage not configured")
	}

	// Resolve the lineage to verify caller == owner. Fetch the current approved
	// row; its OwnerUserID is the authoritative lineage owner.
	current, err := ownerStore.GetCurrent(ctx, tenantID, lineageID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "workflow lineage not found: %v", err)
	}
	if current.OwnerUserID != ownerUserID {
		return nil, status.Error(codes.PermissionDenied, "only the lineage owner may propose a version")
	}

	// Copy metadata from the current approved row; the definition is the proposed one.
	// Status is always forced to "private" regardless of current.Status — a proposed
	// version must never inherit a published state before the owner approves it.
	visGroups := current.VisibilityGroups
	if len(visGroups) == 0 {
		visGroups = json.RawMessage("[]")
	}
	row := db.WorkflowRow{
		LineageID:        lineageID,
		TenantID:         tenantUUID,
		OwnerUserID:      ownerUserID,
		Name:             current.Name,
		Description:      current.Description,
		Status:           "private",
		VisibilityKind:   current.VisibilityKind,
		VisibilityGroups: visGroups,
		Definition:       json.RawMessage(req.DefinitionJson),
		// Agent binding is lineage-immutable — a proposal inherits the current
		// version's binding (F9).
		BoundAgentID: current.BoundAgentID,
	}

	proposed, err := proposeStore.ProposeVersion(ctx, tenantID, row)
	if err != nil {
		logger.Error("Propose: ProposeVersion failed", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "failed to propose workflow version")
	}

	// Notify the owner via an inbox envelope so the review request lands in the
	// owner's inbox. The envelope is a non-delegation notification: no attenuated
	// scopes, no capability token, no auto-accept, minimal task spec.
	if envStore != nil {
		intent := fmt.Sprintf("Workflow '%s' v%d proposed — review and approve or reject", current.Name, proposed.Version)
		env := &db.Envelope{
			TenantID:   tenantUUID,
			FromUserID: ownerUserID,
			ToTarget:   map[string]any{"type": "user", "id": ownerUserID},
			TaskSpec: map[string]any{
				"intent":  intent,
				"lineage": lineageID.String(),
				"version": proposed.Version,
			},
			DelegationSpec: nil,
			Depth:          0,
			ExpiresAt:      time.Now().UTC().Add(72 * time.Hour),
		}
		if _, envErr := envStore.CreateEnvelope(ctx, env, db.EnvelopeDelivered); envErr != nil {
			// Non-fatal: the proposal is persisted; only the notification failed.
			logger.Warn("Propose: owner notify envelope failed",
				zap.String("lineage", lineageID.String()),
				zap.Int("version", proposed.Version),
				zap.Error(envErr),
			)
		}
	}

	if err := emitter.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TenantId:    tenantID,
		OccurredAt:  timestamppb.New(time.Now().UTC()),
		ActorUserId: ownerUserID,
		EventType:   "aikonos.broker.workflow.proposed",
		ResourceRef: "aikonos:workflow-lineage:" + lineageID.String(),
		Decision:    auditv1.PolicyDecision_ALLOW,
	}); err != nil {
		audit.RecordEmitFailure(ctx, logger, err, "aikonos.broker.workflow.proposed")
	}

	return &brokerv1.ProposeWorkflowVersionResponse{Version: int32(proposed.Version)}, nil
}

// Decide approves or rejects a proposed version and emits the corresponding
// audit event. Was decideCore.
func Decide(
	ctx context.Context,
	tenantID, ownerUserID string,
	req *brokerv1.DecideWorkflowVersionRequest,
	ownerStore Store,
	decideStore Store,
	emitter AuditEmitter,
	logger *zap.Logger,
) (*brokerv1.DecideWorkflowVersionResponse, error) {
	lineageID, err := uuid.Parse(req.LineageId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid lineage_id: %v", err)
	}

	if ownerStore == nil || decideStore == nil {
		return nil, status.Error(codes.FailedPrecondition, "workflow storage not configured")
	}

	// Verify caller == lineage owner.
	current, err := ownerStore.GetCurrent(ctx, tenantID, lineageID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "workflow lineage not found: %v", err)
	}
	if current.OwnerUserID != ownerUserID {
		return nil, status.Error(codes.PermissionDenied, "only the lineage owner may decide on a proposed version")
	}

	var newState string
	var eventType string
	if req.Approved {
		if err := decideStore.ApproveVersion(ctx, tenantID, lineageID, int(req.Version)); err != nil {
			logger.Error("Decide: ApproveVersion failed", zap.Error(err))
			return nil, status.Errorf(codes.Internal, "failed to approve workflow version: %v", err)
		}
		newState = "approved"
		eventType = "aikonos.broker.workflow.approved"
	} else {
		if err := decideStore.RejectVersion(ctx, tenantID, lineageID, int(req.Version), req.Reason); err != nil {
			logger.Error("Decide: RejectVersion failed", zap.Error(err))
			return nil, status.Errorf(codes.Internal, "failed to reject workflow version: %v", err)
		}
		newState = "rejected"
		eventType = "aikonos.broker.workflow.rejected"
	}

	decision := auditv1.PolicyDecision_ALLOW
	if !req.Approved {
		decision = auditv1.PolicyDecision_DENY
	}
	if err := emitter.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TenantId:    tenantID,
		OccurredAt:  timestamppb.New(time.Now().UTC()),
		ActorUserId: ownerUserID,
		EventType:   eventType,
		ResourceRef: "aikonos:workflow-lineage:" + lineageID.String(),
		Decision:    decision,
	}); err != nil {
		audit.RecordEmitFailure(ctx, logger, err, eventType)
	}

	return &brokerv1.DecideWorkflowVersionResponse{ApprovalState: newState}, nil
}
