package broker

// submit_ifaces.go — narrow interfaces for SubmitPlan dependencies (plus the
// package-wide taskStore interface backing Deps.Tasks).
// Extracted so unit tests can inject fakes without Postgres or OPA.
// *db.TaskRepo and *policy.Engine satisfy these interfaces; production
// code uses those concrete types via Deps.Tasks / Deps.Policy and the
// accessor methods below. *netacl.Checker satisfies aclDecider, which
// is the type of Deps.ACL.

import (
	"context"

	"github.com/google/uuid"

	"github.com/adamdekan/aikonos/broker/internal/config"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/netacl"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// taskStore is the single production interface backing Deps.Tasks — the union
// of every *db.TaskRepo method any handler or shared helper in this package
// calls (SubmitPlan, GetDecisionTrace, CreateTask/CreateGatewayTask,
// ApproveTask/ApproveGatewayTask, GetTaskState, CancelTask, EmitStatus,
// InvokeTool's cost-budget gate, and the envelope RPCs — SendEnvelope's
// persist and DismissEnvelope's lookup+transition, which are the production
// path for envelopes: there is no separate envelope-store Deps field).
// *db.TaskRepo satisfies it (compile-time-asserted in service.go). Collapses
// the eight former per-op override fields (tasks, approveTasks, createTasks,
// getTasks, cancelTasks, emitStatusTasks, invokeToolTasks, gatewayApproveTasks,
// CP2) and the two former envelope override fields (createEnvelopes,
// dismissEnvelopes, CP3) into this one production field — tests inject fakes
// directly through Deps.Tasks, embedding stubTaskStore (task_store_stub_test.go)
// to cover methods they don't exercise, mirroring the workflowStore/
// workflowsvc.Store collapse (fable-rpc-twins CP1).
type taskStore interface {
	Get(ctx context.Context, tenantID, taskID string) (*db.Task, error)
	Transition(ctx context.Context, tenantID, taskID string, from, to db.TaskState) error
	Create(ctx context.Context, t *db.Task) error
	IncrementCost(ctx context.Context, tenantID, taskID string, units int64) error
	InsertPlanSteps(ctx context.Context, tenantID string, steps []db.PlanStepRecord) error
	CreateApprovalRequest(ctx context.Context, ar *db.ApprovalRequest) (uuid.UUID, error)
	ListMintableSteps(ctx context.Context, tenantID, taskID string) ([]db.ExecutableStep, error)
	GetPlanStepTraces(ctx context.Context, tenantID, taskID string) ([]db.PlanStepTrace, error)
	GetPendingApprovalByTask(ctx context.Context, tenantID, taskID string) (*db.ApprovalRequest, error)
	ResolveApproval(ctx context.Context, tenantID, approvalID, approver string, approved bool, reason string) (db.ApprovalState, bool, error)
	ListInbox(ctx context.Context, tenantID, userID string, includeResolved bool) ([]*db.Envelope, error)
	GetEnvelope(ctx context.Context, tenantID, envelopeID string) (*db.Envelope, error)
	RespondEnvelope(ctx context.Context, tenantID, envelopeID, userID string, accepted bool) (db.EnvelopeState, error)
	CreateEnvelope(ctx context.Context, e *db.Envelope, initial db.EnvelopeState) (uuid.UUID, error)
	DismissEnvelope(ctx context.Context, tenantID, envelopeID, userID string) (db.EnvelopeState, error)
	// CountEnvelopesByPayloadRef backs the skill-transfer staging GC refcount
	// check (personal_skills.go): how many PENDING/DELIVERED envelopes still
	// point at a shared xfer-<uuid> staging segment.
	CountEnvelopesByPayloadRef(ctx context.Context, tenantID, payloadRef string) (int, error)
}

// planPolicyEngine is the minimal policy.Engine surface SubmitPlan needs.
// Satisfied by *policy.Engine.
type planPolicyEngine interface {
	DecidePlan(ctx context.Context, plan *planv1.Plan, pctx policy.PlanContext) (*policy.PlanDecision, error)
	DecideToolCall(ctx context.Context, q policy.ToolQuery) (*policy.Decision, error)
}

// simPolicyEngine is the minimal policy.Engine surface SimulatePolicy needs.
// Satisfied by *policy.Engine. Distinct from planPolicyEngine because it
// requires CheckFGA (the FGA gate is optional in SubmitPlan but central here).
type simPolicyEngine interface {
	DecideToolCall(ctx context.Context, q policy.ToolQuery) (*policy.Decision, error)
	CheckFGA(ctx context.Context, user, relation, object string) (bool, error)
}

// aclDecider is the minimal netacl.Checker surface SubmitPlan needs.
// Satisfied by *netacl.Checker (Deps.ACL is typed aclDecider, not *netacl.Checker,
// so callers pass *netacl.Checker which satisfies the interface implicitly).
type aclDecider interface {
	Enabled() bool
	Decide(ctx context.Context, tenantID, userID, host string) netacl.Action
}

// stepCategory is the per-step precedence bucket used by both SubmitPlan and
// SimulatePolicy. Precedence: deny > step-up > approval > allow.
type stepCategory int

const (
	stepAllow    stepCategory = iota
	stepApproval              // needs human
	stepStepUp                // needs step-up auth
	stepDeny
)

// classifyDecision maps a *policy.Decision to its stepCategory using the same
// precedence rule routeStep's callers (SubmitPlan and SimulatePolicy) both
// rely on. Single source of truth — change here, both callers update
// automatically.
func classifyDecision(dec *policy.Decision) stepCategory {
	switch {
	case dec.Deny || (!dec.Allow && !dec.NeedsApproval && !dec.NeedsStepUp):
		return stepDeny
	case dec.NeedsStepUp:
		return stepStepUp
	case dec.NeedsApproval:
		return stepApproval
	default:
		return stepAllow
	}
}

// decisionSeverity maps a *policy.Decision to the same 1-4 severity buckets
// used by effectclass.Severity so FU3 can compare config Posture.Severity()
// against the current routing decision.
//
//	1 — auto (Allow=true, nothing else)
//	2 — approval (NeedsApproval, not StepUp)
//	3 — step-up  (NeedsStepUp)
//	4 — deny     (Deny, or all-false fail-closed)
func decisionSeverity(dec *policy.Decision) int {
	switch classifyDecision(dec) {
	case stepAllow:
		return 1
	case stepApproval:
		return 2
	case stepStepUp:
		return 3
	default: // stepDeny
		return 4
	}
}

// applyPosture mutates dec to reflect the given config.Posture escalation.
// Only called when posture.Severity() > decisionSeverity(dec) — i.e. strictly
// stricter — so this can never relax.
func applyPosture(dec *policy.Decision, p config.Posture) {
	switch p {
	case config.PostureApproval:
		dec.Allow, dec.NeedsApproval, dec.NeedsStepUp, dec.Deny = false, true, false, false
		dec.Reason = "effect_class_routing config requires approval"
	case config.PostureStepUp:
		dec.Allow, dec.NeedsApproval, dec.NeedsStepUp, dec.Deny = false, true, true, false
		dec.Reason = "effect_class_routing config requires step-up"
	case config.PostureDeny:
		dec.Allow, dec.NeedsApproval, dec.NeedsStepUp, dec.Deny = false, false, false, true
		dec.Reason = "effect_class_routing config denies this effect class"
	}
}

// skillGatePolicy is the FGA surface userSkillDecision needs.
// Satisfied by *policy.Engine. Extracted so tests can inject a fake without
// starting an OPA/OpenFGA process.
type skillGatePolicy interface {
	FGAEnabled() bool
	CheckFGA(ctx context.Context, user, relation, object string) (bool, error)
}

// skillPolicyFor returns the skillGatePolicy to use: the test-injected fake if
// set, otherwise the concrete Policy from Deps. Returns nil when neither is
// set — the nil check in userSkillDecision treats nil as "gate disabled".
func (s *SandboxService) skillPolicyFor() skillGatePolicy {
	if s.skillPolicy != nil {
		return s.skillPolicy
	}
	if s.deps.Policy != nil {
		return s.deps.Policy
	}
	return nil
}

// planPolicyFor returns the planPolicyEngine to use.
func (s *SandboxService) planPolicyFor() planPolicyEngine {
	if s.policy != nil {
		return s.policy
	}
	return s.deps.Policy
}
