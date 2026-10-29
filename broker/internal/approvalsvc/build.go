package approvalsvc

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// BuildInput bundles every input Plan and Create need. Was SubmitPlan's inline block
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
	// FGATenant is the tenant's FGA object ("tenant:<name>"), whose approver
	// relation adds candidates for an n >= 2 gate. Empty: task approvers only.
	FGATenant string
	Outcome   planv1.ValidationOutcome
	PlanID    string
	NSteps    int
}

// Gate summarises the approval gate Plan worked out, for the decision record.
type Gate struct {
	RequiredApprovals int  `json:"required_approvals"` // n: raised by config, never below the floor
	Floor             int  `json:"floor"`              // 1 for human approval, 2 for step-up
	ConfiguredN       int  `json:"configured_n"`       // approval_required_n in tenant config; 0 when unset
	Approvers         int  `json:"approvers"`          // eligible approvers after separation of duty
	SeparationOfDuty  bool `json:"separation_of_duty"` // the requester is excluded (n >= 2)
	ExpiryHours       int  `json:"expiry_hours"`
	// ApproverSet is who may vote, for notifying them; not part of the record.
	ApproverSet []string `json:"-"`
}

// InsufficientApproversError: an n >= 2 gate has fewer eligible approvers
// other than the requester than it needs, so nobody could ever decide it.
// Its message is shown to the user.
type InsufficientApproversError struct{ Need, Have int }

func (e *InsufficientApproversError) Error() string {
	return fmt.Sprintf("this step needs %d approvers other than you; only %d eligible — ask an admin to add approvers", e.Need, e.Have)
}

// Plan works out the approval gate for a plan outcome that resolved to
// NEEDS_HUMAN or NEEDS_STEP_UP, without writing anything: the n-of-m
// threshold (config-raised, never lowered below the step-up floor of 2) and
// the approver set.
//
// n = 1: the task's FGA "approver" tuples (the owner, written at task
// creation), falling back to the owner when FGA is disabled.
//
// n >= 2 (separation of duty): the task's approvers plus the tenant's
// (FGA tenant#approver, which includes admins), minus the requester, each
// kept only if FGA can_approve on the task holds, the same check
// ListPendingApprovals and ApproveTask make. Fewer than n left returns
// *InsufficientApproversError (audited) rather than a gate nobody can decide.
func Plan(ctx context.Context, in BuildInput) (Gate, error) {
	// Both settings can only tighten the gate, so their defaults would loosen
	// it: when either cannot be read, no gate is built and the plan fails.
	expiryHours, err := in.Config.GetInt(ctx, in.TenantID, "approval_expiry_hours")
	if err != nil {
		in.Logger.Error("approvalsvc.Plan: read approval_expiry_hours failed", zap.Error(err))
		return Gate{}, status.Error(codes.Unavailable, "approval settings could not be read")
	}

	// FU5: n-of-m approval threshold with separation-of-duty.
	// Policy floor: step-up gates require at least 2 (four-eyes); plain
	// human-approval gates start at 1. Config may only raise, never lower.
	floor := 1
	if in.Outcome == planv1.ValidationOutcome_NEEDS_STEP_UP {
		floor = 2
	}
	cfgN, err := in.Config.GetInt(ctx, in.TenantID, "approval_required_n")
	if err != nil {
		in.Logger.Error("approvalsvc.Plan: read approval_required_n failed", zap.Error(err))
		return Gate{}, status.Error(codes.Unavailable, "approval settings could not be read")
	}
	requiresN := cfgN
	if floor > requiresN {
		requiresN = floor
	}

	fga := in.Policy != nil && in.Policy.FGAEnabled()
	approverSet := []string{in.OwnerUserID}
	if fga {
		if users := readUsers(ctx, in, "approver", "task:"+in.TaskID); len(users) > 0 {
			approverSet = users
		}
	}

	if requiresN >= 2 {
		if fga && in.FGATenant != "" {
			approverSet = append(approverSet, readUsers(ctx, in, "approver", in.FGATenant)...)
			approverSet = append(approverSet, readUsers(ctx, in, "admin", in.FGATenant)...)
		}
		eligible := make([]string, 0, len(approverSet))
		for _, a := range approverSet {
			if a == in.OwnerUserID || slices.Contains(eligible, a) {
				continue
			}
			// Fails closed: an FGA error drops the candidate.
			if fga {
				if ok, cErr := in.Policy.CheckFGA(ctx, "user:"+a, "can_approve", "task:"+in.TaskID); cErr != nil || !ok {
					continue
				}
			}
			eligible = append(eligible, a)
		}
		approverSet = eligible
		if len(approverSet) < requiresN {
			in.Logger.Warn("approvalsvc.Plan: insufficient_approvers after SoD exclusion",
				zap.String("task_id", in.TaskID),
				zap.Int("approver_set_size", len(approverSet)),
				zap.Int("requires_n", requiresN),
			)
			ctxStruct, _ := structpb.NewStruct(map[string]any{"requires_n": requiresN, "eligible": len(approverSet)})
			if err := in.Audit.Emit(ctx, &auditv1.AuditEvent{
				EventId:       ids.EventID(),
				TraceId:       in.TraceID,
				TenantId:      in.TenantID,
				OccurredAt:    timestamppb.New(time.Now().UTC()),
				ActorUserId:   in.OwnerUserID,
				ActorSpiffeId: in.ActorSpiffeID,
				EventType:     "aikonos.broker.approval.insufficient_approvers",
				ResourceRef:   "aikonos:task:" + in.TaskID,
				Decision:      auditv1.PolicyDecision_DENY,
				Context:       ctxStruct,
			}); err != nil {
				audit.RecordEmitFailure(ctx, in.Logger, err, "aikonos.broker.approval.insufficient_approvers")
			}
			return Gate{}, &InsufficientApproversError{Need: requiresN, Have: len(approverSet)}
		}
	}

	return Gate{
		RequiredApprovals: requiresN,
		Floor:             floor,
		ConfiguredN:       cfgN,
		Approvers:         len(approverSet),
		SeparationOfDuty:  requiresN >= 2,
		ExpiryHours:       expiryHours,
		ApproverSet:       approverSet,
	}, nil
}

// readUsers returns the user ids holding relation on object, expanding a
// group#member tuple one level. A read error is logged and yields nothing,
// so it can only shrink the approver set.
// ponytail: one level of group nesting; recurse if nested approver groups appear.
func readUsers(ctx context.Context, in BuildInput, relation, object string) []string {
	rels, err := in.Policy.ReadTuples(ctx, policy.ReadFilter{Relation: relation, Object: object})
	if err != nil {
		in.Logger.Warn("approvalsvc: ReadTuples for approver set failed",
			zap.String("object", object), zap.String("relation", relation), zap.Error(err))
		return nil
	}
	var out []string
	for _, r := range rels {
		if id, ok := strings.CutPrefix(r.User, "user:"); ok {
			out = append(out, id)
		} else if group, ok := strings.CutSuffix(r.User, "#member"); ok {
			out = append(out, readUsers(ctx, in, "member", group)...)
		}
	}
	return out
}

// Create persists the approval request for a gate Plan worked out.
func Create(ctx context.Context, in BuildInput, g Gate) error {
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
		ApproverSet: g.ApproverSet,
		RequiresN:   int16(g.RequiredApprovals),
		ExpiresAt:   time.Now().UTC().Add(time.Duration(g.ExpiryHours) * time.Hour),
	}); err != nil {
		in.Logger.Error("approvalsvc.Create: create approval failed", zap.Error(err))
		return status.Errorf(codes.Internal, "failed to create approval request")
	}
	return nil
}
