package broker

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/approvalsvc"
	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/auth"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/effectclass"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/netacl"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// toolReg returns the injected Registry, falling back to the package-default
// when ToolRegistry is nil (unit tests that predate FU9 and don't inject it).
// The fallback is baseline-only: PkgDefault() has no manifest overlay loaded,
// so overlay tools resolve only through the production-injected Registry.
func (s *SandboxService) toolReg() *toolregistry.Registry {
	if s.deps.ToolRegistry != nil {
		return s.deps.ToolRegistry
	}
	return toolregistry.PkgDefault()
}

// SubmitPlan validates a proposed plan against OPA and OpenFGA, enforces the
// agent skill boundary (if task is agent-bound), records per-step policy
// decisions, and either mints capability tokens (APPROVED), creates an approval
// request (NEEDS_HUMAN/NEEDS_STEP_UP), or returns DENIED for the agent to replan.
func (s *SandboxService) SubmitPlan(ctx context.Context, req *brokerv1.SubmitPlanRequest) (*planv1.PlanValidationResult, error) {
	ctx, span := tracer.Start(ctx, "broker.plan.validate")
	defer span.End()
	traceID := span.SpanContext().TraceID().String()

	if req.Plan == nil {
		return nil, status.Error(codes.InvalidArgument, "plan is required")
	}
	plan := req.Plan
	tenantID := plan.TenantId // the plan carries its tenant; south-bound has no OIDC tenant

	s.deps.Logger.Info("SubmitPlan",
		zap.String("task_id", req.TaskId),
		zap.String("sandbox_spiffe_id", req.SandboxSpiffeId),
		zap.Int("steps", len(plan.Steps)),
	)

	taskStore := s.deps.Tasks
	policyEng := s.planPolicyFor()

	task, err := taskStore.Get(ctx, tenantID, req.TaskId)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "task %s not found", req.TaskId)
		}
		return nil, status.Errorf(codes.Internal, "failed to load task")
	}

	// Move the task into VALIDATING (CREATED→PLANNING→VALIDATING as needed).
	if err := s.advanceToValidating(ctx, tenantID, task, taskStore); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "task not in a plannable state: %v", err)
	}

	// CP2: server-side skill boundary — independent of gateway claims.
	// If the task is bound to a named agent, reject any step whose tool_id is not
	// in that agent's Skills.  The check runs before OPA so a compromised gateway
	// cannot forge an approval for a tool the agent was never authorised to use.
	// Tasks without an agent_id (normal human north tasks) are completely unaffected.
	if task.AgentID != nil {
		if violation := s.checkAgentSkills(ctx, tenantID, task.AgentID.String(), plan); violation != "" {
			s.emitPlanAudit(ctx, traceID, tenantID, req, auditv1.PolicyDecision_DENY)
			return &planv1.PlanValidationResult{
				PlanId:     plan.PlanId,
				Outcome:    planv1.ValidationOutcome_DENIED,
				Violations: []string{violation},
			}, nil
		}
	}

	// 1. Whole-plan structural validation (aikonos/plan_validation).
	planDec, err := policyEng.DecidePlan(ctx, plan, policy.PlanContext{
		CostBudget:          task.CostBudget,
		EffectClassCeiling:  plan.EffectClassCeiling,
		ActorIsControlPlane: false, // sandboxes are never control-plane
		ActorSpiffeID:       req.SandboxSpiffeId,
	})
	if err != nil {
		s.deps.Logger.Error("SubmitPlan: plan policy error", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "policy evaluation failed")
	}
	if !planDec.Allow {
		// Structural violations — agent must replan. Task stays in VALIDATING.
		s.emitPlanAudit(ctx, traceID, tenantID, req, auditv1.PolicyDecision_DENY)
		return &planv1.PlanValidationResult{
			PlanId:     plan.PlanId,
			Outcome:    planv1.ValidationOutcome_DENIED,
			Violations: planDec.Violations,
		}, nil
	}

	// 2. Per-step effect-class evaluation (aikonos/tool_invocation).
	stepDecisions := make([]*planv1.StepDecision, 0, len(plan.Steps))
	stepRecords := make([]db.PlanStepRecord, 0, len(plan.Steps))
	anyDeny, anyStepUp, anyApproval := false, false, false

	tenantUUID, _ := uuid.Parse(tenantID)
	taskUUID, _ := uuid.Parse(req.TaskId)

	// Lazy-load the tenant overlay once before the step loop (I6). One DB read
	// per tenant per process lifetime; subsequent calls are no-ops.
	if s.deps.OverlayLoader != nil {
		s.deps.OverlayLoader.ensureLoaded(ctx, tenantID, s.deps.Logger)
	}

	for _, step := range plan.Steps {
		// Compute the routing class: take the stricter of what the planner
		// declared and what the registry knows (tenant overlay if present, else
		// global authoritative). This prevents a malicious or buggy planner from
		// understating a tool's effect class to obtain auto-approval. MCP and
		// unknown tools return ok=false and keep the plan-declared class.
		routingClass := step.EffectClass
		authEC, authKnown := s.toolReg().EffectClassForTenant(tenantID, step.ToolId)
		if authKnown {
			routingClass = effectclass.Stricter(step.EffectClass, authEC)
		}

		// User skill gate: deny-by-default FGA check for personal tasks.
		// Skipped for agent-bound tasks — those are governed by the CP2
		// agent.Skills list (checkAgentSkills); their owners may be svc-
		// service principals with no group membership.
		//
		// mcp: tools are also skipped: their authorization is the connector
		// permitted_agent/permitted_group grant, enforced at InvokeTool (and the
		// tool is only offered when the agent can_access the connector). The
		// per-user skill object "skill:mcp:<conn>:<tool>" is not even a valid
		// OpenFGA object (the id carries colons) — checking it 400s and fails
		// closed, which would deny every MCP tool from an interactive session.
		fgaDec := ""
		if task.AgentID == nil && !strings.HasPrefix(step.ToolId, "mcp:") {
			fgaDec = s.userSkillDecision(ctx, task.OwnerUserID, step.ToolId)
		}

		// Network access-list (web egress): governed plan-time decision so the
		// agent gets a clear reason. The Tool Proxy re-enforces DENY at fetch +
		// every redirect (the unbypassable boundary), since the capability token
		// is scope-based, not URL-bound. Only extracted for web.fetch steps —
		// routeStep's netacl layer is a no-op when NetaclHost is "".
		var netaclHost string
		if s.deps.ACL != nil && s.deps.ACL.Enabled() && step.ToolId == "web.fetch" {
			netaclHost = webFetchHost(step.Args)
		}

		// CP2: routeStep runs the unified 5-layer escalation pipeline (OPA →
		// netacl → disabled_tools → overlay-disable → effect_class_routing),
		// deriving reads_sensitive server-side instead of trusting
		// step.ReadsSensitive. It has no side effects — the trace below drives
		// the same audits/decision-trace notes this loop always emitted.
		dec, rtrace, err := routeStep(ctx, routeStepInput{
			Engine:             policyEng,
			Reg:                s.toolReg(),
			Cfg:                s.cfg(),
			ACL:                s.deps.ACL,
			TenantID:           tenantID,
			UserID:             task.OwnerUserID,
			ToolID:             step.ToolId,
			EffectClass:        routingClass,
			ActorSpiffeID:      req.SandboxSpiffeId,
			FGADecision:        fgaDec,
			HasDLPAttestation:  plan.HasDlpAttestation,
			NetaclHost:         netaclHost,
			OverlayGateEnabled: s.deps.OverlayLoader != nil,
		})
		if err != nil {
			s.deps.Logger.Error("SubmitPlan: step policy error", zap.Error(err), zap.Int32("seq", step.Seq))
			return nil, status.Errorf(codes.Internal, "policy evaluation failed")
		}

		// Emit the audits routeStep itself never emits (it is a pure decision
		// function) — same events, same ordering as before the extraction.
		if rtrace.NetaclMutated {
			eventType, auditDecision := "aikonos.network.fetch.denied", auditv1.PolicyDecision_DENY
			if rtrace.NetaclAction == netacl.Ask {
				eventType, auditDecision = "aikonos.network.fetch.ask", auditv1.PolicyDecision_APPROVAL_REQUIRED
			}
			s.emitNetworkAudit(ctx, traceID, tenantID, task.OwnerUserID, rtrace.NetaclHost, eventType, auditDecision)
		}
		if rtrace.DisabledToolsHit {
			scope, _ := s.toolReg().RequiredScope(step.ToolId)
			s.emitToolDenied(ctx, &brokerv1.InvokeToolRequest{
				ToolId:   step.ToolId,
				TenantId: tenantID,
				UserId:   task.OwnerUserID,
			}, scope, "tool_disabled", nil, nil)
		}
		if rtrace.OverlayDisableHit {
			scope, _ := s.toolReg().RequiredScope(step.ToolId)
			s.emitToolDenied(ctx, &brokerv1.InvokeToolRequest{
				ToolId:   step.ToolId,
				TenantId: tenantID,
				UserId:   task.OwnerUserID,
			}, scope, "skill_disabled", nil, nil)
		}

		pd := db.PolicyAllow
		switch classifyDecision(dec) {
		case stepDeny:
			anyDeny = true
			pd = db.PolicyDeny
		case stepStepUp:
			anyStepUp = true
			pd = db.PolicyNeedStepUp
		case stepApproval:
			anyApproval = true
			pd = db.PolicyNeedApproval
		}

		stepDecisions = append(stepDecisions, &planv1.StepDecision{
			Seq:          step.Seq,
			Approved:     dec.Allow,
			DenyReason:   dec.Reason,
			PolicyRuleId: dec.PolicyRuleID,
		})
		var stepArgs map[string]any
		if step.Args != nil {
			stepArgs = step.Args.AsMap()
		}

		// CP1: build decision_trace from already-computed dec + netacl capture.
		// Omit empty sub-keys; do not alter dec.* or outcomes.
		trace := map[string]any{}
		if len(dec.Reasons) > 0 {
			trace["reasons"] = dec.Reasons
		}
		if scope, ok := s.toolReg().RequiredScope(step.ToolId); ok {
			trace["capability_scope"] = scope
		}
		if rtrace.NetaclMutated {
			trace["network"] = map[string]any{
				"host":   rtrace.NetaclHost,
				"action": string(rtrace.NetaclAction),
				"reason": dec.Reason,
			}
		}
		// FU2: record when the authoritative registry escalated the declared class.
		if routingClass != step.EffectClass {
			authStr := routingClass.String()
			if authKnown {
				authStr = authEC.String()
			}
			trace["effect_class_override"] = map[string]string{
				"declared":      step.EffectClass.String(),
				"authoritative": authStr,
				"routed":        routingClass.String(),
			}
		}
		// FU3: if config escalated, record it (overwrites FU2 note — config is last and strictest).
		if rtrace.ConfigOverride != nil {
			trace["effect_class_override"] = rtrace.ConfigOverride
		}

		var traceVal map[string]any
		if len(trace) > 0 {
			traceVal = trace
		}

		stepRecords = append(stepRecords, db.PlanStepRecord{
			TaskID:         taskUUID,
			TenantID:       tenantUUID,
			Seq:            int32(step.Seq),
			ToolID:         step.ToolId,
			ArgsHash:       argsHash(step.Args),
			EffectClass:    routingClass.String(), // persist the routed class, not the declared one
			Justification:  step.Justification,
			EstimatedCost:  step.EstimatedCost,
			PolicyDecision: pd,
			PolicyRuleID:   dec.PolicyRuleID,
			Args:           stepArgs,
			PolicyReason:   dec.Reason,
			DecisionTrace:  traceVal,
		})
	}

	if err := taskStore.InsertPlanSteps(ctx, tenantID, stepRecords); err != nil {
		s.deps.Logger.Error("SubmitPlan: persist steps failed", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "failed to persist plan steps")
	}

	// 3. Aggregate the per-step decisions into a plan outcome + drive state.
	var outcome planv1.ValidationOutcome
	switch {
	case anyDeny:
		outcome = planv1.ValidationOutcome_DENIED // agent may replan; task stays VALIDATING
	case anyStepUp:
		outcome = planv1.ValidationOutcome_NEEDS_STEP_UP
	case anyApproval:
		outcome = planv1.ValidationOutcome_NEEDS_HUMAN
	default:
		outcome = planv1.ValidationOutcome_APPROVED
	}
	if s.deps.TaskMetrics != nil {
		s.deps.TaskMetrics.RecordPlanOutcome(ctx, outcome.String())
	}

	var capTokens map[int32]string
	switch outcome {
	case planv1.ValidationOutcome_APPROVED:
		if err := taskStore.Transition(ctx, tenantID, req.TaskId, db.TaskStateValidating, db.TaskStateApproved); err != nil {
			return nil, status.Errorf(codes.Internal, "transition to APPROVED failed: %v", err)
		}
		// Mint a per-step capability token so the caller can drive InvokeTool.
		// Every step is ALLOW in the APPROVED outcome.
		scopes := make([]stepScope, 0, len(plan.Steps))
		for _, step := range plan.Steps {
			scopes = append(scopes, stepScope{Seq: step.Seq, ToolID: step.ToolId})
		}
		if capTokens, err = mintStepTokens(s.deps.Capability, s.toolReg(), req.SandboxSpiffeId, req.TaskId, scopes); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to mint capability tokens: %v", err)
		}
	case planv1.ValidationOutcome_NEEDS_HUMAN, planv1.ValidationOutcome_NEEDS_STEP_UP:
		if err := taskStore.Transition(ctx, tenantID, req.TaskId, db.TaskStateValidating, db.TaskStateAwaitingApproval); err != nil {
			return nil, status.Errorf(codes.Internal, "transition to AWAITING_APPROVAL failed: %v", err)
		}

		// approvalsvc.Build owns the n-of-m threshold / separation-of-duty /
		// approver-set construction and the CreateApprovalRequest call (CP4/C6).
		// Pass a genuinely nil AccessPolicy when Policy is unset — a typed-nil
		// *policy.Engine wrapped in the interface would not compare == nil inside
		// Build and would panic on FGAEnabled().
		var pol approvalsvc.AccessPolicy
		if s.deps.Policy != nil {
			pol = s.deps.Policy
		}
		if err := approvalsvc.Build(ctx, approvalsvc.BuildInput{
			Store:         taskStore,
			Policy:        pol,
			Config:        s.cfg(),
			Audit:         s.deps.Audit,
			Logger:        s.deps.Logger,
			TraceID:       traceID,
			TenantID:      tenantID,
			TaskID:        req.TaskId,
			TenantUUID:    tenantUUID,
			TaskUUID:      taskUUID,
			OwnerUserID:   task.OwnerUserID,
			ActorSpiffeID: req.SandboxSpiffeId,
			Outcome:       outcome,
			PlanID:        plan.PlanId,
			NSteps:        len(plan.Steps),
		}); err != nil {
			return nil, err
		}
	}

	auditDecision := auditv1.PolicyDecision_ALLOW
	if outcome == planv1.ValidationOutcome_DENIED {
		auditDecision = auditv1.PolicyDecision_DENY
	} else if outcome != planv1.ValidationOutcome_APPROVED {
		auditDecision = auditv1.PolicyDecision_APPROVAL_REQUIRED
	}
	s.emitPlanAudit(ctx, traceID, tenantID, req, auditDecision)

	planEventType := "PLAN_PROPOSED"
	if outcome == planv1.ValidationOutcome_NEEDS_HUMAN || outcome == planv1.ValidationOutcome_NEEDS_STEP_UP {
		planEventType = "APPROVAL_NEEDED"
	}
	publishTaskEvent(ctx, s.deps, tenantID, req.TaskId, planEventType, map[string]any{
		"plan_id": plan.PlanId,
		"outcome": outcome.String(),
		"n_steps": len(plan.Steps),
	})

	return &planv1.PlanValidationResult{
		PlanId:             plan.PlanId,
		Outcome:            outcome,
		StepDecisions:      stepDecisions,
		Violations:         planDec.Violations,
		CapabilityTokenIds: capTokens,
	}, nil
}

// checkAgentSkills loads the agent identified by agentID and returns a non-empty
// violation string if any step in the plan references a tool not in the agent's
// Skills list.  Returns "" when all steps are within the skill boundary.
//
// Tool ids are compared directly against agent.Skills — both use the same
// namespace (e.g. "doc.read", "web.fetch").  The preAuthApprover in the gateway
// uses agent.Skills as its allowlist in exactly the same way, so no mapping is
// needed.  aikonos: sentinel / non-tool step ids (zero-length toolId or prefixed
// "aikonos:") are exempt — they are internal control tokens, not real tools.
func (s *SandboxService) checkAgentSkills(ctx context.Context, tenantID, agentID string, plan *planv1.Plan) string {
	if s.deps.Agents == nil {
		// Agents store is nil but the task is bound to an agent — fail closed.
		// A misconfigured deploy with no Agents store must not silently approve
		// plans for agent-bound tasks; the boundary is unverifiable.
		s.deps.Logger.Warn("checkAgentSkills: Agents store is nil for agent-bound task",
			zap.String("agent_id", agentID))
		return fmt.Sprintf("agent %q: skill boundary cannot be verified (Agents store not configured)", agentID)
	}
	agent, err := s.deps.Agents.Get(ctx, tenantID, agentID)
	if err != nil {
		// Agent not found or store error: fail closed — deny the plan.
		s.deps.Logger.Warn("checkAgentSkills: agent lookup failed",
			zap.String("agent_id", agentID), zap.Error(err))
		return fmt.Sprintf("agent %q not found or unavailable: skill boundary cannot be verified", agentID)
	}

	allowed := make(map[string]bool, len(agent.Skills))
	for _, sk := range agent.Skills {
		allowed[sk] = true
	}
	// mcp: tools are never in agent.Skills — the webui agent editor filters mcp
	// ids out of the skills picker. Attaching an MCP server IS the grant (it
	// writes the permitted_agent FGA tuple), so mcp_servers membership is the
	// correct plan-time boundary; InvokeTool re-checks can_access per call.
	attachedConns := make(map[string]bool, len(agent.McpServers))
	for _, c := range agent.McpServers {
		attachedConns[c] = true
	}

	for _, step := range plan.Steps {
		tid := step.ToolId
		if tid == "" || strings.HasPrefix(tid, "aikonos:") {
			continue // internal sentinel — not a real tool invocation
		}
		if strings.HasPrefix(tid, "mcp:") {
			// A malformed mcp id, or one whose connector the agent has not
			// attached, is outside the boundary.
			connID, _, ok := toolregistry.ParseMcpToolID(tid)
			if !ok || !attachedConns[connID] {
				return fmt.Sprintf("tool %q is not in agent %q's skill boundary", tid, agent.Name)
			}
			continue
		}
		if !allowed[tid] {
			return fmt.Sprintf("tool %q is not in agent %q's skill boundary", tid, agent.Name)
		}
	}
	return ""
}

// userSkillDecision checks whether the task's owner has can_invoke on the given
// tool in OpenFGA and returns the FGADecision string for ToolQuery.
//
// Only called for personal (non-agent-bound) tasks when FGA is enabled.
// Agent-bound tasks are already governed by the CP2 server-side agent.Skills
// list (checkAgentSkills above); their owners may be svc- service principals
// with no group membership, so this group-based gate must not apply to them.
//
// Returns "allow" on grant, "deny" on no-grant or error.  On error it fails
// closed — this is a security gate; consistent with the platform principle.
func (s *SandboxService) userSkillDecision(ctx context.Context, ownerUserID, toolID string) string {
	sp := s.skillPolicyFor()
	if sp == nil || !sp.FGAEnabled() {
		return "allow"
	}
	granted, err := sp.CheckFGA(ctx, "user:"+ownerUserID, "can_invoke", "skill:"+toolID)
	if err != nil {
		s.deps.Logger.Warn("userSkillDecision: CheckFGA error — failing closed",
			zap.String("user", ownerUserID),
			zap.String("tool", toolID),
			zap.Error(err))
		return "deny"
	}
	if granted {
		return "allow"
	}
	return "deny"
}

// advanceToValidating walks a task into VALIDATING using only legal transitions.
func (s *SandboxService) advanceToValidating(ctx context.Context, tenantID string, t *db.Task, ts taskStore) error {
	switch t.State {
	case db.TaskStateValidating:
		return nil
	case db.TaskStateCreated:
		if err := ts.Transition(ctx, tenantID, t.TaskID.String(), db.TaskStateCreated, db.TaskStatePlanning); err != nil {
			return err
		}
		return ts.Transition(ctx, tenantID, t.TaskID.String(), db.TaskStatePlanning, db.TaskStateValidating)
	case db.TaskStatePlanning:
		return ts.Transition(ctx, tenantID, t.TaskID.String(), db.TaskStatePlanning, db.TaskStateValidating)
	default:
		return fmt.Errorf("state %s", t.State)
	}
}

func (s *SandboxService) emitPlanAudit(ctx context.Context, traceID, tenantID string, req *brokerv1.SubmitPlanRequest, decision auditv1.PolicyDecision) {
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:       ids.EventID(),
		TraceId:       traceID,
		TenantId:      tenantID,
		OccurredAt:    timestampNow(),
		ActorSpiffeId: req.SandboxSpiffeId,
		EventType:     "aikonos.broker.plan.validated",
		ResourceRef:   "aikonos:task:" + req.TaskId,
		Decision:      decision,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, "aikonos.broker.plan.validated")
	}
}

func (s *SandboxService) emitNetworkAudit(ctx context.Context, traceID, tenantID, actor, host, eventType string, decision auditv1.PolicyDecision) {
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    tenantID,
		OccurredAt:  timestampNow(),
		ActorUserId: actor,
		EventType:   eventType,
		ResourceRef: "network_host:" + host,
		Decision:    decision,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, eventType)
	}
}

// resolveTenant returns the effective tenant for a south-bound request. South
// callers carry no OIDC tenant; the request field is the primary source, then
// the OIDC identity (north callers), then the broker default. Extracted so
// denial paths (which run before the success-path resolution) can use the same
// fallback chain.
func (s *SandboxService) resolveTenant(ctx context.Context, reqTenant string) string {
	if reqTenant != "" {
		return reqTenant
	}
	if id, ok := auth.IdentityFromContext(ctx); ok && id.TenantID != "" {
		return id.TenantID
	}
	return s.deps.TenantID
}
