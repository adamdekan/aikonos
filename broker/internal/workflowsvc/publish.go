package workflowsvc

import (
	"context"
	"errors"
	"strings"
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

// ComputeRequires builds the Requires manifest from a workflow's step slice.
// Skills is the distinct set of step.Skill values. Scopes is the distinct set
// of RequiredScope results for those skills (unregistered tools are omitted —
// they will fail at execution time regardless). Agents is always empty for v1.
// The registry parameter must not be nil; callers guard before calling. Was
// computeRequires.
func ComputeRequires(def *workflow.Workflow, reg *toolregistry.Registry) workflow.Requires {
	skillSeen := make(map[string]struct{})
	scopeSeen := make(map[string]struct{})
	var skills, scopes []string

	for _, step := range def.Steps {
		if step.Kind == "reason" {
			continue
		}
		if _, seen := skillSeen[step.Skill]; !seen {
			skillSeen[step.Skill] = struct{}{}
			skills = append(skills, step.Skill)
		}
		if scope, ok := reg.RequiredScope(step.Skill); ok {
			if _, seen := scopeSeen[scope]; !seen {
				scopeSeen[scope] = struct{}{}
				scopes = append(scopes, scope)
			}
		}
	}
	return workflow.Requires{
		Skills: skills,
		Scopes: scopes,
		Agents: []string{},
	}
}

// Publish is the shared publish logic for north and south paths. It:
//  1. Gates on skill:workflows (callers pass the owner's FGA check function).
//  2. Verifies caller == lineage owner (owner lookup pattern).
//  3. For each group_id, CheckFGA(user:owner, "member", "group:"+id) — PermissionDenied on any miss.
//  4. Loads the version's definition, computes requires, merges into the definition.
//  5. Calls PublishVersion (no rating gate — rating is informational only).
//  6. Emits workflow.published audit event.
//
// Was publishCore.
func Publish(
	ctx context.Context,
	tenantID, ownerUserID string,
	req *brokerv1.PublishWorkflowRequest,
	ownerStore Store,
	publishStore Store,
	reg *toolregistry.Registry,
	checkFGA func(ctx context.Context, user, relation, object string) (bool, error),
	emitter AuditEmitter,
	logger *zap.Logger,
) (*brokerv1.PublishWorkflowResponse, error) {
	lineageID, err := uuid.Parse(req.LineageId)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid lineage_id: %v", err)
	}
	if ownerStore == nil || publishStore == nil {
		return nil, status.Error(codes.FailedPrecondition, "workflow storage not configured")
	}

	// Verify caller == lineage owner.
	current, err := ownerStore.GetCurrent(ctx, tenantID, lineageID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "workflow lineage not found: %v", err)
	}
	if current.OwnerUserID != ownerUserID {
		return nil, status.Error(codes.PermissionDenied, "only the lineage owner may publish a workflow")
	}

	// Normalize target group ids to the bare form ("security-team"), tolerating a
	// "group:"-prefixed input. The webui picker sources groups from
	// ListDelegatableUsers, which returns FGA-object ids ("group:security-team"),
	// while the workflow Pi tool passes bare ids. Publish re-adds the "group:"
	// prefix for the FGA membership check below and stores the bare form so it
	// matches the bare member-group ids ListVisibleShared resolves at read time.
	groupIDs := make([]string, 0, len(req.GroupIds))
	for _, g := range req.GroupIds {
		groupIDs = append(groupIDs, strings.TrimPrefix(g, "group:"))
	}

	// Verify owner is a member of every target group.
	// When checkFGA is nil (FGA disabled), membership is not enforced — intentional
	// parity with the codebase's FGA-off allow-all dev posture (same as checkWorkflowSkill).
	for _, groupID := range groupIDs {
		if checkFGA != nil {
			ok, fgaErr := checkFGA(ctx, "user:"+ownerUserID, "member", "group:"+groupID)
			if fgaErr != nil {
				logger.Warn("Publish: group membership CheckFGA error — failing closed",
					zap.String("user", ownerUserID),
					zap.String("group", groupID),
					zap.Error(fgaErr),
				)
				return nil, status.Errorf(codes.PermissionDenied, "group membership check failed for group %s", groupID)
			}
			if !ok {
				return nil, status.Errorf(codes.PermissionDenied, "owner is not a member of group %s", groupID)
			}
		}
	}

	// Load the target version to compute requires from its steps.
	targetRow, err := publishStore.GetVersion(ctx, tenantID, lineageID, int(req.Version))
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "workflow version not found: %v", err)
	}

	def, err := workflow.FromJSON(targetRow.Definition)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "definition parse error: %v", err)
	}

	// Compute and merge the requires manifest. Use PkgDefault when no registry
	// is injected (older tests that don't wire ToolRegistry).
	effectiveReg := reg
	if effectiveReg == nil {
		effectiveReg = toolregistry.PkgDefault()
	}
	def.Requires = ComputeRequires(def, effectiveReg)

	updatedDefJSON, err := workflow.ToJSON(def)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "definition re-encode error: %v", err)
	}

	// Publish — no rating gate; rating is informational only. Stores the bare
	// (normalized) group ids so the read-time visibility match works.
	if err := publishStore.PublishVersion(ctx, tenantID, lineageID, int(req.Version), groupIDs, updatedDefJSON); err != nil {
		if errors.Is(err, db.ErrVersionNotFound) {
			return nil, status.Errorf(codes.NotFound, "workflow version %d not found for lineage %s", req.Version, req.LineageId)
		}
		logger.Error("Publish: PublishVersion failed", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "failed to publish workflow: %v", err)
	}

	if emitErr := emitter.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TenantId:    tenantID,
		OccurredAt:  timestamppb.New(time.Now().UTC()),
		ActorUserId: ownerUserID,
		EventType:   "aikonos.broker.workflow.published",
		ResourceRef: "aikonos:workflow-lineage:" + lineageID.String(),
		Decision:    auditv1.PolicyDecision_ALLOW,
	}); emitErr != nil {
		audit.RecordEmitFailure(ctx, logger, emitErr, "aikonos.broker.workflow.published")
	}

	return &brokerv1.PublishWorkflowResponse{
		VisibilityKind: "shared",
		Groups:         groupIDs,
	}, nil
}
