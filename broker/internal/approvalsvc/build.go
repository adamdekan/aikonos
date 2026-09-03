package approvalsvc

import (
	"context"
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
	"github.com/adamdekan/aikonos/broker/internal/policy"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// BuildInput bundles every input Build needs. Was SubmitPlan's inline block
// (broker/internal/broker/service_plan.go) that ran when a plan resolved to
// NEEDS_HUMAN or NEEDS_STEP_UP.
type BuildInput struct {
	Store  Store
	Policy AccessPolicy // pass a genuinely nil interface (not a typed-nil *policy.Engine) to disable the FGA approver-set lookup — mirrors SubmitPlan's `s.deps.Policy != nil` guard
	Config ConfigProvider
	Audit  AuditEmitter
	Logger *zap.Logger

	TraceID       string
	TenantID      string
	TaskID        string
	TenantUUID    uuid.UUID
	TaskUUID      uuid.UUID
	OwnerUserID   string
	ActorSpiffeID string
	Outcome       planv1.ValidationOutcome
	PlanID        string
	NSteps        int
}

// Build constructs the approval-gate request for a plan outcome that
// resolved to NEEDS_HUMAN or NEEDS_STEP_UP: the n-of-m threshold
// (config-raised, never lowered below the step-up floor of 2), the approver
// set (FGA "approver" tuples on the task, falling back to the task owner
// when FGA is disabled), separation-of-duty (excluding the requester once
// n≥2), and the CreateApprovalRequest persistence call itself.
//
// Was the inline block in SubmitPlan (service_plan.go) — see
//  CP4 (C6). Pure extraction: same threshold,
// same SoD exclusion, same audit event, same CreateApprovalRequest shape.
func Build(ctx context.Context, in BuildInput) error {
	expiryHours := in.Config.GetInt(ctx, in.TenantID, "approval_expiry_hours")

	// FU5: n-of-m approval threshold with separation-of-duty.
	// Policy floor: step-up gates require at least 2 (four-eyes); plain
	// human-approval gates start at 1. Config may only raise, never lower.
	floor := 1
	if in.Outcome == planv1.ValidationOutcome_NEEDS_STEP_UP {
		floor = 2
	}
	cfgN := in.Config.GetInt(ctx, in.TenantID, "approval_required_n")
	requiresN := cfgN
	if floor > requiresN {
		requiresN = floor
	}

	// Build approver set from FGA tuples written at task creation, falling
	// back to the task owner when FGA is disabled. FGA tuples use the
	// "approver" relation on "task:<id>".
	approverSet := []string{in.OwnerUserID}
	if in.Policy != nil && in.Policy.FGAEnabled() {
		rels, rErr := in.Policy.ReadTuples(ctx, policy.ReadFilter{
			Relation: "approver",
			Object:   "task:" + in.TaskID,
		})
		if rErr != nil {
			in.Logger.Warn("approvalsvc.Build: ReadTuples for approver set failed — using owner fallback",
				zap.String("task_id", in.TaskID), zap.Error(rErr))
		} else if len(rels) > 0 {
			approverSet = make([]string, 0, len(rels))
			for _, r := range rels {
				// tuples are written as user:<id>; strip prefix for the set.
				if id, ok := strings.CutPrefix(r.User, "user:"); ok {
					approverSet = append(approverSet, id)
				}
			}
		}
	}

	// Separation of duty: when n≥2, exclude the task owner (requester)
	// from the approver set so they cannot self-approve. If the resulting
	// set is smaller than n, emit a note but keep requiresN — the gate
	// stays PENDING until enough distinct approvers exist.
	if requiresN >= 2 {
		filtered := make([]string, 0, len(approverSet))
		for _, a := range approverSet {
			if a != in.OwnerUserID {
				filtered = append(filtered, a)
			}
		}
		approverSet = filtered
		if len(approverSet) < requiresN {
			in.Logger.Warn("approvalsvc.Build: insufficient_approvers after SoD exclusion",
				zap.String("task_id", in.TaskID),
				zap.Int("approver_set_size", len(approverSet)),
				zap.Int("requires_n", requiresN),
			)
			if err := in.Audit.Emit(ctx, &auditv1.AuditEvent{
				EventId:       ids.EventID(),
				TraceId:       in.TraceID,
				TenantId:      in.TenantID,
				OccurredAt:    timestamppb.New(time.Now().UTC()),
				ActorSpiffeId: in.ActorSpiffeID,
				EventType:     "aikonos.broker.approval.insufficient_approvers",
				ResourceRef:   "aikonos:task:" + in.TaskID,
				Decision:      auditv1.PolicyDecision_APPROVAL_REQUIRED,
			}); err != nil {
				audit.RecordEmitFailure(ctx, in.Logger, err, "aikonos.broker.approval.insufficient_approvers")
			}
		}
	}

	if _, err := in.Store.CreateApprovalRequest(ctx, &db.ApprovalRequest{
		TaskID:      in.TaskUUID,
		TenantID:    in.TenantUUID,
		RequesterID: in.OwnerUserID,
		Summary: map[string]any{
			"plan_id": in.PlanID,
			"reason":  in.Outcome.String(),
			"step_up": in.Outcome == planv1.ValidationOutcome_NEEDS_STEP_UP,
			"n_steps": in.NSteps,
		},
		ApproverSet: approverSet,
		RequiresN:   int16(requiresN),
		ExpiresAt:   time.Now().UTC().Add(time.Duration(expiryHours) * time.Hour),
	}); err != nil {
		in.Logger.Error("approvalsvc.Build: create approval failed", zap.Error(err))
		return status.Errorf(codes.Internal, "failed to create approval request")
	}

	return nil
}
