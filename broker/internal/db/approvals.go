// broker/internal/db/approvals.go
// Persistence for plan steps and human-approval requests.
// Phase 3: plan validation records per-step policy decisions; plans that need
// human sign-off create an approval_request that ApproveTask resolves.
// Every query sets app.current_tenant for RLS — no raw SQL outside this package.
package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// ErrVoterNotInApproverSet is returned by ResolveApproval when an explicit
// approver_set is configured and the caller is not a member. Callers should
// map this to PermissionDenied, not Internal.
var ErrVoterNotInApproverSet = errors.New("approval: voter is not in the approver set")

// PolicyDecision values mirror the plan_steps.policy_decision CHECK constraint.
type PolicyDecision string

const (
	PolicyAllow        PolicyDecision = "ALLOW"
	PolicyDeny         PolicyDecision = "DENY"
	PolicyNeedApproval PolicyDecision = "APPROVAL_REQUIRED"
	PolicyNeedStepUp   PolicyDecision = "STEP_UP_REQUIRED"
)

// ApprovalState values mirror the approval_requests.state CHECK constraint.
type ApprovalState string

const (
	ApprovalPending  ApprovalState = "PENDING"
	ApprovalApproved ApprovalState = "APPROVED"
	ApprovalDenied   ApprovalState = "DENIED"
	ApprovalExpired  ApprovalState = "EXPIRED"
)

// PlanStepRecord is one persisted step with its policy decision.
type PlanStepRecord struct {
	TaskID         uuid.UUID
	TenantID       uuid.UUID
	Seq            int32
	ToolID         string
	ArgsHash       string
	EffectClass    string
	Justification  string
	EstimatedCost  int64
	PolicyDecision PolicyDecision
	PolicyRuleID   string
	// Args is the full tool argument set, persisted as args_json so the executor
	// can replay the step through the Tool Proxy. nil → stored NULL.
	Args map[string]any
	// PolicyReason is the human-readable reason text from policy.Decision.Reason.
	// Empty string stored as '' (NOT NULL DEFAULT '').
	PolicyReason string
	// DecisionTrace is structured per-gate detail: reasons, network, capability_scope.
	// nil → stored NULL (JSONB nullable).
	DecisionTrace map[string]any
}

// ApprovalRequest is a pending human-approval gate for a task.
type ApprovalRequest struct {
	ApprovalID  uuid.UUID
	TaskID      uuid.UUID
	TenantID    uuid.UUID
	RequesterID string
	Summary     map[string]any
	ApproverSet []string
	RequiresN   int16
	ApprovedBy  []string
	DeniedBy    []string
	State       ApprovalState
	ExpiresAt   time.Time
}

// InsertPlanSteps persists all steps of a validated plan in one transaction.
// Replaces any prior steps for the same (task, seq) via upsert so a replan
// overwrites the previous attempt's decisions.
func (r *TaskRepo) InsertPlanSteps(ctx context.Context, tenantID string, steps []PlanStepRecord) error {
	if len(steps) == 0 {
		return nil
	}
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		batch := &pgx.Batch{}
		for _, s := range steps {
			var argsJSON []byte
			if s.Args != nil {
				if b, mErr := json.Marshal(s.Args); mErr == nil {
					argsJSON = b
				}
			}
			var traceJSON []byte
			if s.DecisionTrace != nil {
				if b, mErr := json.Marshal(s.DecisionTrace); mErr == nil {
					traceJSON = b
				}
			}
			batch.Queue(`
			INSERT INTO plan_steps (
				task_id, tenant_id, seq, tool_id, args_hash,
				effect_class, justification, estimated_cost,
				policy_decision, policy_rule_id, state, args_json,
				policy_reason, decision_trace
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
			ON CONFLICT (task_id, seq) DO UPDATE SET
				tool_id = EXCLUDED.tool_id,
				args_hash = EXCLUDED.args_hash,
				effect_class = EXCLUDED.effect_class,
				justification = EXCLUDED.justification,
				estimated_cost = EXCLUDED.estimated_cost,
				policy_decision = EXCLUDED.policy_decision,
				policy_rule_id = EXCLUDED.policy_rule_id,
				state = EXCLUDED.state,
				args_json = EXCLUDED.args_json,
				policy_reason = EXCLUDED.policy_reason,
				decision_trace = EXCLUDED.decision_trace`,
				s.TaskID, s.TenantID, s.Seq, s.ToolID, s.ArgsHash,
				s.EffectClass, s.Justification, s.EstimatedCost,
				string(s.PolicyDecision), s.PolicyRuleID, stepStateFor(s.PolicyDecision), argsJSON,
				s.PolicyReason, traceJSON,
			)
		}

		br := conn.SendBatch(ctx, batch)
		defer br.Close()
		for range steps {
			if _, err := br.Exec(); err != nil {
				return fmt.Errorf("insert plan step: %w", err)
			}
		}
		return nil
	})
}

// PlanStepTrace is the read-side projection of a plan_steps row for the
// decision-trace RPC: the fields needed to assemble the four-gate per-step view.
type PlanStepTrace struct {
	Seq            int32
	ToolID         string
	EffectClass    string
	PolicyDecision string
	PolicyRuleID   string
	PolicyReason   string
	Justification  string
	// DecisionTrace is decoded from the JSONB column; empty map when NULL.
	DecisionTrace map[string]any
}

// GetPlanStepTraces returns the trace rows for all steps of a task, ordered by
// seq. Under withTenant for RLS — the task_id query is tenant-scoped. Returns
// an empty slice (not an error) when no rows exist.
func (r *TaskRepo) GetPlanStepTraces(ctx context.Context, tenantID, taskID string) ([]PlanStepTrace, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) ([]PlanStepTrace, error) {
		rows, err := conn.Query(ctx, `
		SELECT seq, tool_id, effect_class, policy_decision, policy_rule_id, policy_reason,
		       COALESCE(decision_trace, '{}'::jsonb), justification
		FROM plan_steps
		WHERE task_id = $1
		ORDER BY seq ASC`, taskID)
		if err != nil {
			return nil, fmt.Errorf("query plan step traces: %w", err)
		}
		defer rows.Close()

		var out []PlanStepTrace
		for rows.Next() {
			var s PlanStepTrace
			var traceRaw []byte
			if err := rows.Scan(&s.Seq, &s.ToolID, &s.EffectClass, &s.PolicyDecision,
				&s.PolicyRuleID, &s.PolicyReason, &traceRaw, &s.Justification); err != nil {
				return nil, fmt.Errorf("scan plan step trace: %w", err)
			}
			s.DecisionTrace = make(map[string]any)
			if len(traceRaw) > 0 {
				if err := json.Unmarshal(traceRaw, &s.DecisionTrace); err != nil {
					return nil, fmt.Errorf("unmarshal decision_trace: %w", err)
				}
			}
			out = append(out, s)
		}
		return out, rows.Err()
	})
}

// ExecutableStep is a persisted step ready for execution, with its full args.
type ExecutableStep struct {
	StepID      uuid.UUID
	Seq         int32
	ToolID      string
	EffectClass string
	Args        map[string]any
	State       string
}

// ListExecutableSteps returns a task's APPROVED steps (policy ALLOW), ordered by
// seq, with their full args decoded from args_json. These are the steps the
// executor drives through the Tool Proxy.
func (r *TaskRepo) ListExecutableSteps(ctx context.Context, tenantID, taskID string) ([]ExecutableStep, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) ([]ExecutableStep, error) {
		rows, err := conn.Query(ctx, `
		SELECT step_id, seq, tool_id, effect_class, COALESCE(args_json, '{}'::jsonb), state
		FROM plan_steps
		WHERE task_id = $1 AND policy_decision = 'ALLOW'
		ORDER BY seq ASC`, taskID)
		if err != nil {
			return nil, fmt.Errorf("query plan steps: %w", err)
		}
		defer rows.Close()

		var out []ExecutableStep
		for rows.Next() {
			var s ExecutableStep
			var argsRaw []byte
			if err := rows.Scan(&s.StepID, &s.Seq, &s.ToolID, &s.EffectClass, &argsRaw, &s.State); err != nil {
				return nil, fmt.Errorf("scan plan step: %w", err)
			}
			if len(argsRaw) > 0 {
				if err := json.Unmarshal(argsRaw, &s.Args); err != nil {
					return nil, fmt.Errorf("unmarshal args_json: %w", err)
				}
			}
			out = append(out, s)
		}
		return out, rows.Err()
	})
}

// ListMintableSteps returns a task's steps that are eligible for a capability
// token once the task is APPROVED — i.e. every step the policy did NOT deny.
// Unlike ListExecutableSteps (ALLOW only, for the server-side executor), this
// includes APPROVAL_REQUIRED / STEP_UP_REQUIRED steps, which become mintable
// after a human resolves the approval gate (used by ApproveTask mint-on-approve).
func (r *TaskRepo) ListMintableSteps(ctx context.Context, tenantID, taskID string) ([]ExecutableStep, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) ([]ExecutableStep, error) {
		rows, err := conn.Query(ctx, `
		SELECT step_id, seq, tool_id, effect_class, COALESCE(args_json, '{}'::jsonb), state
		FROM plan_steps
		WHERE task_id = $1 AND policy_decision <> 'DENY'
		ORDER BY seq ASC`, taskID)
		if err != nil {
			return nil, fmt.Errorf("query plan steps: %w", err)
		}
		defer rows.Close()

		var out []ExecutableStep
		for rows.Next() {
			var s ExecutableStep
			var argsRaw []byte
			if err := rows.Scan(&s.StepID, &s.Seq, &s.ToolID, &s.EffectClass, &argsRaw, &s.State); err != nil {
				return nil, fmt.Errorf("scan plan step: %w", err)
			}
			if len(argsRaw) > 0 {
				if err := json.Unmarshal(argsRaw, &s.Args); err != nil {
					return nil, fmt.Errorf("unmarshal args_json: %w", err)
				}
			}
			out = append(out, s)
		}
		return out, rows.Err()
	})
}

// StepExecState values for UpdateStepExecution (subset of the state CHECK).
const (
	StepExecuting = "EXECUTING"
	StepCompleted = "COMPLETED"
	StepFailed    = "FAILED"
)

// MarkStepExecuting sets a step EXECUTING and stamps started_at.
func (r *TaskRepo) MarkStepExecuting(ctx context.Context, tenantID string, stepID uuid.UUID) error {
	return r.updateStep(ctx, tenantID, stepID,
		`UPDATE plan_steps SET state='EXECUTING', started_at=NOW() WHERE step_id=$1`)
}

// MarkStepDone sets a step COMPLETED/FAILED, records actual_cost + error, stamps
// ended_at.
func (r *TaskRepo) MarkStepDone(ctx context.Context, tenantID string, stepID uuid.UUID, state string, cost int64, errMsg string) error {
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		_, err := conn.Exec(ctx,
			`UPDATE plan_steps SET state=$2, actual_cost=$3, error_message=NULLIF($4,''), ended_at=NOW() WHERE step_id=$1`,
			stepID, state, cost, errMsg)
		return err
	})
}

func (r *TaskRepo) updateStep(ctx context.Context, tenantID string, stepID uuid.UUID, query string) error {
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		_, err := conn.Exec(ctx, query, stepID)
		return err
	})
}

// stepStateFor maps a policy decision to the plan_steps.state it implies.
func stepStateFor(d PolicyDecision) string {
	switch d {
	case PolicyAllow:
		return "APPROVED"
	case PolicyDeny:
		return "DENIED"
	default:
		return "PENDING" // APPROVAL_REQUIRED / STEP_UP_REQUIRED
	}
}

// CreateApprovalRequest inserts a PENDING approval gate and returns its ID.
func (r *TaskRepo) CreateApprovalRequest(ctx context.Context, ar *ApprovalRequest) (uuid.UUID, error) {
	return withConn(ctx, r.pool, ar.TenantID.String(), func(conn *pgxpool.Conn) (uuid.UUID, error) {
		summary, err := json.Marshal(ar.Summary)
		if err != nil {
			return uuid.Nil, fmt.Errorf("marshal summary: %w", err)
		}
		requiresN := ar.RequiresN
		if requiresN < 1 {
			requiresN = 1
		}

		var id uuid.UUID
		err = conn.QueryRow(ctx, `
		INSERT INTO approval_requests (
			task_id, tenant_id, requester_id, summary,
			approver_set, requires_n, state, expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,'PENDING',$7)
		RETURNING approval_id`,
			ar.TaskID, ar.TenantID, ar.RequesterID, summary,
			ar.ApproverSet, requiresN, ar.ExpiresAt,
		).Scan(&id)
		if err != nil {
			return uuid.Nil, fmt.Errorf("insert approval request: %w", err)
		}

		r.logger.Info("approval request created",
			zap.String("approval_id", id.String()),
			zap.String("task_id", ar.TaskID.String()),
		)
		return id, nil
	})
}

// GetPendingApprovalByTask returns the open approval gate for a task, if any.
func (r *TaskRepo) GetPendingApprovalByTask(ctx context.Context, tenantID, taskID string) (*ApprovalRequest, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (*ApprovalRequest, error) {
		var ar ApprovalRequest
		var summary []byte
		err := conn.QueryRow(ctx, `
		SELECT approval_id, task_id, tenant_id, requester_id, summary,
		       approver_set, requires_n, approved_by, denied_by, state, expires_at
		FROM approval_requests
		WHERE task_id = $1 AND state = 'PENDING'
		ORDER BY created_at DESC
		LIMIT 1`, taskID,
		).Scan(&ar.ApprovalID, &ar.TaskID, &ar.TenantID, &ar.RequesterID, &summary,
			&ar.ApproverSet, &ar.RequiresN, &ar.ApprovedBy, &ar.DeniedBy, &ar.State, &ar.ExpiresAt)
		if err != nil {
			return nil, err // pgx.ErrNoRows when none pending
		}
		if len(summary) > 0 {
			if err := json.Unmarshal(summary, &ar.Summary); err != nil {
				return nil, fmt.Errorf("unmarshal summary: %w", err)
			}
		}
		return &ar, nil
	})
}

// ResolveApproval records one approver's vote and, once the n-of-m threshold
// is met (or any denial), resolves the gate. Returns the resulting state and
// whether the gate is now resolved. Idempotent per approver — a repeated vote
// from the same user does not double-count.
func (r *TaskRepo) ResolveApproval(ctx context.Context, tenantID, approvalID, approver string, approved bool, reason string) (ApprovalState, bool, error) {
	return withConn2(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (ApprovalState, bool, error) {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return "", false, fmt.Errorf("begin tx: %w", err)
		}
		defer tx.Rollback(ctx)

		var (
			requiresN   int16
			approverSet []string
			approvedBy  []string
			deniedBy    []string
			state       ApprovalState
		)
		err = tx.QueryRow(ctx, `
		SELECT requires_n, approver_set, approved_by, denied_by, state
		FROM approval_requests
		WHERE approval_id = $1
		FOR UPDATE`, approvalID,
		).Scan(&requiresN, &approverSet, &approvedBy, &deniedBy, &state)
		if err != nil {
			return "", false, err
		}
		if state != ApprovalPending {
			return state, true, nil // already resolved
		}

		// SoD: when an explicit approver_set is configured, only members may vote.
		// An empty set (legacy rows) skips the check — any voter satisfies the gate.
		if !voterAllowed(approverSet, approver) {
			return ApprovalPending, false, fmt.Errorf("%w (approval %s)", ErrVoterNotInApproverSet, approvalID)
		}

		if approved {
			approvedBy = appendUnique(approvedBy, approver)
		} else {
			deniedBy = appendUnique(deniedBy, approver)
		}

		newState := ApprovalPending
		resolved := false
		switch {
		case len(deniedBy) > 0: // any denial resolves to DENIED
			newState, resolved = ApprovalDenied, true
		case int16(len(approvedBy)) >= requiresN:
			newState, resolved = ApprovalApproved, true
		}

		if resolved {
			_, err = tx.Exec(ctx, `
			UPDATE approval_requests
			SET approved_by = $1, denied_by = $2, state = $3,
			    resolved_at = NOW(), denial_reason = $4
			WHERE approval_id = $5`,
				approvedBy, deniedBy, string(newState), nullIfEmpty(reason), approvalID,
			)
		} else {
			_, err = tx.Exec(ctx, `
			UPDATE approval_requests
			SET approved_by = $1, denied_by = $2
			WHERE approval_id = $3`,
				approvedBy, deniedBy, approvalID,
			)
		}
		if err != nil {
			return "", false, fmt.Errorf("update approval: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return "", false, fmt.Errorf("commit: %w", err)
		}
		return newState, resolved, nil
	})
}

// voterAllowed reports whether voter may cast a vote on an approval request.
// An empty approverSet means legacy / unrestricted — any voter is allowed.
// A non-empty set enforces explicit membership (SoD boundary).
func voterAllowed(approverSet []string, voter string) bool {
	if len(approverSet) == 0 {
		return true
	}
	return inSet(approverSet, voter)
}

// inSet reports whether v is contained in xs.
func inSet(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func appendUnique(xs []string, v string) []string {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
