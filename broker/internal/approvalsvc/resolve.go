package approvalsvc

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/db"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// Resolve encapsulates the approve-and-mint logic shared by north
// ApproveTask and south ApproveGatewayTask. It loads the pending approval,
// checks FGA can_approve, resolves the vote, transitions the task, and mints
// per-step capability tokens when the approval resolves to APPROVED. It
// returns the resolved status, minted tokens, the correct audit event type,
// and the policy decision — so both callers emit the right audit event.
//
// Was resolveApprovalAndMint (broker/internal/broker/gateway_tasks.go), moved
// as-is — see  CP4 (C6). mint replaces the
// original `reg *toolregistry.Registry` + `deps.Capability` pair: the broker
// package still owns capability.Minter/toolregistry.Registry construction
// (mintStepTokens, unchanged) and passes it in as this narrow closure.
func Resolve(
	ctx context.Context,
	store Store,
	pol AccessPolicy,
	mint Minter,
	logger *zap.Logger,
	tenantID, taskID, approverUserID string,
	approved bool,
	reason string,
	argsDigest string,
	stepUp StepUp,
) (respStatus brokerv1.TaskStatus, capTokens map[int32]string, eventType string, decision auditv1.PolicyDecision, err error) {
	ar, err := store.GetPendingApprovalByTask(ctx, tenantID, taskID)
	if err != nil {
		if err == pgx.ErrNoRows {
			return 0, nil, "", 0, status.Errorf(codes.FailedPrecondition, "no pending approval for task %s", taskID)
		}
		logger.Error("approvalsvc.Resolve: load approval failed", zap.Error(err))
		return 0, nil, "", 0, status.Errorf(codes.FailedPrecondition, "no pending approval for task %s", taskID)
	}

	allowed, fgaErr := pol.CheckFGA(ctx, "user:"+approverUserID, "can_approve", "task:"+taskID)
	if fgaErr != nil || !allowed {
		return 0, nil, "", 0, status.Errorf(codes.PermissionDenied, "approver not authorized for task %s", taskID)
	}

	// AG-07 (M-28): an approval counts only for the arguments the approver
	// was shown. Re-read the stored args and refuse a digest that does not
	// match them; a denial needs no digest.
	steps, serr := store.ListMintableSteps(ctx, tenantID, taskID)
	if serr != nil {
		logger.Error("approvalsvc.Resolve: list steps failed", zap.Error(serr))
		return 0, nil, "", 0, status.Errorf(codes.Internal, "failed to load plan steps")
	}
	if approved && argsDigest != ArgsDigest(steps) {
		return 0, nil, "", 0, status.Errorf(codes.FailedPrecondition, "arguments for task %s do not match the approved digest", taskID)
	}

	// AG-13: a yes on a step-up approval needs the approver's recent MFA
	// sign-in; a no lowers risk and needs none.
	if isStepUp, _ := ar.Summary["step_up"].(bool); approved && isStepUp {
		if err := stepUp.requireStepUp(ctx); err != nil {
			return 0, nil, "", 0, err
		}
	}

	newState, resolved, err := store.ResolveApproval(ctx, tenantID, ar.ApprovalID.String(), approverUserID, approved, reason)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) { // expired between the load and the vote (M-09)
			return 0, nil, "", 0, status.Errorf(codes.FailedPrecondition, "no pending approval for task %s", taskID)
		}
		if errors.Is(err, db.ErrVoterNotInApproverSet) {
			return 0, nil, "", 0, status.Errorf(codes.PermissionDenied, "approver not in the approver set for task %s", taskID)
		}
		logger.Error("approvalsvc.Resolve: resolve failed", zap.Error(err))
		return 0, nil, "", 0, status.Errorf(codes.Internal, "failed to record approval")
	}

	// Default: n-of-m not yet met.
	respStatus = brokerv1.TaskStatus_AWAITING_APPROVAL
	eventType = "aikonos.broker.task.approval_recorded"
	decision = auditv1.PolicyDecision_ALLOW
	if !approved {
		decision = auditv1.PolicyDecision_DENY
	}

	if resolved {
		switch newState {
		case db.ApprovalApproved:
			if terr := store.Transition(ctx, tenantID, taskID, db.TaskStateAwaitingApproval, db.TaskStateApproved); terr != nil {
				return 0, nil, "", 0, status.Errorf(codes.FailedPrecondition, "transition failed: %v", terr)
			}
			respStatus = brokerv1.TaskStatus_APPROVED
			eventType = "aikonos.broker.task.approved"
			decision = auditv1.PolicyDecision_ALLOW

			task, terr := store.Get(ctx, tenantID, taskID)
			if terr != nil {
				logger.Error("approvalsvc.Resolve: load task failed", zap.Error(terr))
				return 0, nil, "", 0, status.Errorf(codes.Internal, "failed to load task")
			}
			scopes := make([]StepScope, 0, len(steps))
			for _, st := range steps {
				scopes = append(scopes, StepScope{Seq: st.Seq, ToolID: st.ToolID})
			}
			if capTokens, err = mint(task.OwnerUserID, taskID, task.OnBehalfOf(), scopes); err != nil {
				return 0, nil, "", 0, status.Errorf(codes.Internal, "failed to mint capability tokens: %v", err)
			}

		case db.ApprovalDenied:
			if terr := store.Transition(ctx, tenantID, taskID, db.TaskStateAwaitingApproval, db.TaskStateDenied); terr != nil {
				return 0, nil, "", 0, status.Errorf(codes.FailedPrecondition, "transition failed: %v", terr)
			}
			respStatus = brokerv1.TaskStatus_DENIED
			eventType = "aikonos.broker.task.denied"
			decision = auditv1.PolicyDecision_DENY
		}
	}
	return respStatus, capTokens, eventType, decision, nil
}
