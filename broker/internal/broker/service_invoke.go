package broker

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/capability"
	"github.com/adamdekan/aikonos/broker/internal/config"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/toolproxy"
	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// emitToolDenied fires a DENY audit event for an InvokeTool denial. It is
// fire-and-forget and nil-guards deps.Audit — it never changes the return value
// or adds latency to the RPC. extras is merged into the context struct after the
// standard fields (reason, scope, error) so callers can attach site-specific
// context (e.g. agent, object) without duplicating the emit block.
func (s *SandboxService) emitToolDenied(ctx context.Context, req *brokerv1.InvokeToolRequest, scope, reason string, errDetail error, extras map[string]any) {
	if s.deps.Audit == nil {
		return
	}
	ctxFields := map[string]any{"reason": reason}
	if scope != "" {
		ctxFields["scope"] = scope
	}
	if errDetail != nil {
		ctxFields["error"] = errDetail.Error()
	}
	for k, v := range extras {
		ctxFields[k] = v
	}
	ctxStruct, _ := structpb.NewStruct(ctxFields)
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TenantId:    s.resolveTenant(ctx, req.TenantId),
		OccurredAt:  timestampNow(),
		ActorUserId: req.UserId,
		EventType:   "aikonos.broker.tool.denied",
		ResourceRef: "aikonos:tool:" + req.ToolId,
		Decision:    auditv1.PolicyDecision_DENY,
		Context:     ctxStruct,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, "aikonos.broker.tool.denied")
	}
}

// emitBudgetExceeded fires a aikonos.broker.task.budget_exceeded audit event
// when a successful tool call's IncrementCost crosses the task's cost budget.
// Fire-and-forget — the tool call already succeeded and must not be failed by
// an accounting side-effect.
func (s *SandboxService) emitBudgetExceeded(ctx context.Context, tenantID string, req *brokerv1.InvokeToolRequest, consumed, budget int64, incErr error) {
	if s.deps.Audit == nil {
		return
	}
	ctxStruct, _ := structpb.NewStruct(map[string]any{
		"tool_id":  req.ToolId,
		"consumed": consumed,
		"budget":   budget,
		"error":    incErr.Error(),
	})
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TenantId:    tenantID,
		OccurredAt:  timestampNow(),
		ActorUserId: req.UserId,
		EventType:   "aikonos.broker.task.budget_exceeded",
		ResourceRef: "aikonos:task:" + req.TaskId,
		Decision:    auditv1.PolicyDecision_DENY,
		Context:     ctxStruct,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, "aikonos.broker.task.budget_exceeded")
	}
}

// emitResultFlagged fires a aikonos.tool.result_flagged audit event when a
// tool result carries a non-empty injection_flags annotation. Fire-and-forget, annotate-only: the call already
// succeeded and this never changes the response.
func (s *SandboxService) emitResultFlagged(ctx context.Context, req *brokerv1.InvokeToolRequest, tenantID string, flags []any) {
	if s.deps.Audit == nil {
		return
	}
	ctxStruct, _ := structpb.NewStruct(map[string]any{"injection_flags": flags})
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TenantId:    tenantID,
		OccurredAt:  timestampNow(),
		ActorUserId: req.UserId,
		EventType:   "aikonos.tool.result_flagged",
		ResourceRef: "aikonos:tool:" + req.ToolId,
		Decision:    auditv1.PolicyDecision_ALLOW,
		Context:     ctxStruct,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, "aikonos.tool.result_flagged")
	}
}

// memoryGroupPreGate re-checks group membership per call for a memory tool
// invoked with scope=group. Returns nil — no gate —
// for any other tool and for the user/agent scopes, which need no group
// authority. The mcp: branch below is the precedent: a grant revoked after the
// capability token was minted must deny the next call.
//
// Membership is checked for the task row's owner, not req.UserId: the request
// field is caller-supplied, and a group bundle is shared writable state. Fails
// closed — a missing group_id, an unloadable task row, an empty owner, a nil
// policy engine, a check error, or a false decision all deny.
func (s *SandboxService) memoryGroupPreGate(ctx context.Context, req *brokerv1.InvokeToolRequest, scope, ownerUserID string, taskLoadFailed bool) error {
	if req.ToolId != "memory.read" && req.ToolId != "memory.write" {
		return nil
	}
	fields := req.Args.GetFields()
	if fields["scope"].GetStringValue() != "group" {
		return nil
	}
	groupID := strings.TrimSpace(fields["group_id"].GetStringValue())
	if groupID == "" {
		s.emitToolDenied(ctx, req, scope, "memory_group_id_missing", nil, nil)
		return status.Error(codes.PermissionDenied, "group_id required for a group-scoped memory call")
	}
	// No owner, no gate: an unloadable row (or one with no owner, or no task store
	// at all) leaves nothing to check membership for. Denied explicitly rather
	// than passing a bare "user:" subject down and relying on OpenFGA to reject
	// it — same reasoning as the mcp: branch's taskLoadFailed guard.
	owner := strings.TrimSpace(ownerUserID)
	if taskLoadFailed || owner == "" {
		s.emitToolDenied(ctx, req, scope, "memory_group_owner_unknown", nil, map[string]any{"group_id": groupID})
		return status.Error(codes.PermissionDenied, "task owner unknown, cannot verify group membership")
	}
	pol := s.skillPolicyFor()
	if pol == nil {
		s.emitToolDenied(ctx, req, scope, "memory_group_check_error", errors.New("policy engine not configured"), nil)
		return status.Error(codes.PermissionDenied, "group membership check unavailable")
	}
	object := "group:" + groupID
	allowed, cerr := pol.CheckFGA(ctx, "user:"+owner, "member", object)
	if cerr != nil {
		s.deps.Logger.Warn("InvokeTool: memory group membership check failed",
			zap.String("tool_id", req.ToolId),
			zap.String("object", object),
			zap.Error(cerr),
		)
		s.emitToolDenied(ctx, req, scope, "memory_group_check_error", cerr, map[string]any{"object": object})
		return status.Errorf(codes.PermissionDenied, "group membership check failed: %v", cerr)
	}
	if !allowed {
		s.deps.Logger.Warn("InvokeTool: memory group access denied by fga",
			zap.String("tool_id", req.ToolId),
			zap.String("object", object),
		)
		s.emitToolDenied(ctx, req, scope, "memory_group_not_member", nil, map[string]any{"object": object})
		return status.Errorf(codes.PermissionDenied, "task owner is not a member of %s", object)
	}
	return nil
}

// InvokeTool executes a tool via the Tool Proxy after verifying the Biscuit
// capability token grants the required scope; fails closed — an unknown tool,
// disabled tool, invalid token, a missing agent_id on an mcp: tool, or an FGA
// denial all return PermissionDenied before any side-effect is attempted.
func (s *SandboxService) InvokeTool(ctx context.Context, req *brokerv1.InvokeToolRequest) (*brokerv1.InvokeToolResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.tool.invoke")
	defer span.End()

	span.SetAttributes(attribute.String("tool.id", req.ToolId))
	s.deps.Logger.Info("InvokeTool",
		zap.String("task_id", req.TaskId),
		zap.String("tool_id", req.ToolId),
		zap.String("sandbox", req.SandboxSpiffeId),
	)

	// The tool must be registered; an unknown tool is denied (deny-by-default).
	scope, ok := s.toolReg().RequiredScope(req.ToolId)
	if !ok {
		s.emitToolDenied(ctx, req, "", "unknown_tool", nil, nil)
		return nil, status.Errorf(codes.PermissionDenied, "unknown tool %q", req.ToolId)
	}

	// Runtime disable check: tenant admin may disable tools via the
	// disabled_tools config key. Runs before the capability gate so a disabled
	// tool is denied regardless of token validity (deny-only; cannot grant).
	if disabled := config.ParseList(s.cfg().GetString(ctx, req.TenantId, "disabled_tools")); toolInList(disabled, req.ToolId) {
		s.emitToolDenied(ctx, req, scope, "tool_disabled", nil, nil)
		return nil, status.Errorf(codes.PermissionDenied, "tool %q is disabled", req.ToolId)
	}

	// Overlay disable check: tenant admin may disable a tool via the skill
	// overlay (CP3). Runs after disabled_tools so both paths are additive.
	// Only fires when a loader is wired and the tenant overlay is cached.
	if s.deps.OverlayLoader != nil {
		tenantForOverlay := s.resolveTenant(ctx, req.TenantId)
		s.deps.OverlayLoader.ensureLoaded(ctx, tenantForOverlay, s.deps.Logger)
		if enabled, known := s.toolReg().IsEnabled(tenantForOverlay, req.ToolId); known && !enabled {
			s.emitToolDenied(ctx, req, scope, "skill_disabled", nil, nil)
			return nil, status.Errorf(codes.PermissionDenied, "tool %q is disabled", req.ToolId)
		}
	}

	// South-bound callers carry no OIDC tenant (the SPIFFE identity has none),
	// so the caller echoes the task's tenant on the request. Fall back to the
	// peer identity then the broker default for older callers. Resolved here
	// (rather than just before the proxy call) so the cost-budget pre-gate
	// below can look up the task under the right tenant.
	tenantID := s.resolveTenant(ctx, req.TenantId)

	// A2 org capability master: deny org-wide when the tool's authoritative
	// effect class is in the tenant's disabled set. Runs before the FGA/capability
	// gates — a master switch above per-user delegation. Fails closed on a
	// settings read error; no-op when the repo is unwired or the set is empty.
	if s.deps.OrgSettings != nil {
		settings, serr := s.deps.OrgSettings.Get(ctx, tenantID)
		if serr != nil {
			s.emitToolDenied(ctx, req, scope, "capability_check_failed", serr, nil)
			return nil, status.Errorf(codes.Internal, "org capability check: %v", serr)
		}
		if len(settings.DisabledEffectClasses) > 0 {
			if ec, ecOK := s.toolReg().EffectClassForTenant(tenantID, req.ToolId); ecOK && settings.EffectClassDisabled(ec.String()) {
				s.emitToolDenied(ctx, req, scope, "capability_disabled", nil, map[string]any{
					"effect_class": ec.String(),
				})
				return nil, status.Errorf(codes.PermissionDenied, "tool %q effect class %s is disabled for this organization", req.ToolId, ec.String())
			}
		}
	}

	// Cost-budget pre-gate: a task whose cost_consumed already meets or
	// exceeds cost_budget is denied before any Biscuit/proxy work runs.
	// Fails open (skipped) when no task store is wired — matching the
	// nil-safe pattern of the other optional gates in this method.
	// preCallConsumed/taskCostBudget are captured here for reuse by the
	// post-call budget_exceeded emit below, so both audit events describe the
	// same task's numbers without a second Get round-trip.
	var preCallConsumed, taskCostBudget int64
	// boundAgentID is the task's own agent_id binding (empty when the task has
	// none, or wasn't loadable) — captured here for the mcp: FGA re-check below
	// so a caller-supplied req.AgentId can be cross-checked against it instead
	// of trusted verbatim.
	var boundAgentID string
	// taskOwnerUserID is the task row's owner — the identity the plan-time skill
	// gate authorized. The memory group pre-gate below checks membership for it
	// rather than req.UserId, which the caller controls.
	var taskOwnerUserID string
	// taskLoadFailed distinguishes a transport/DB error from a genuine
	// not-found: a not-found means there's simply nothing to bind against
	// (proceed, same as an unwired task store), but a transport error must not
	// silently skip the mcp: mismatch check below — that would let a caller
	// widen its own accounting by spoofing req.AgentId whenever the task store
	// hiccups, reopening the window fix 4 closes. The memory group pre-gate reads
	// it for the same reason: no loaded row means no owner to check membership for.
	var taskLoadFailed bool
	// taskState is the task's state as of the pre-gate load, used below to drive
	// the APPROVED → EXECUTING transition once the call is actually dispatched.
	var taskState db.TaskState
	if tasks := s.deps.Tasks; tasks != nil {
		if task, err := tasks.Get(ctx, tenantID, req.TaskId); err != nil {
			if !errors.Is(err, db.ErrNotFound) {
				taskLoadFailed = true
			}
			s.deps.Logger.Warn("InvokeTool: budget pre-gate task load failed",
				zap.String("task_id", req.TaskId), zap.Error(err))
		} else if task != nil {
			// A production Get always errors ErrNotFound instead of returning a
			// nil task; the nil guard only protects against a minimal test double.
			preCallConsumed, taskCostBudget = task.CostConsumed, task.CostBudget
			taskState = task.State
			taskOwnerUserID = task.OwnerUserID
			if task.AgentID != nil {
				boundAgentID = task.AgentID.String()
			}
			if task.CostConsumed >= task.CostBudget {
				s.emitToolDenied(ctx, req, scope, "budget_exceeded", nil, map[string]any{
					"consumed": task.CostConsumed,
					"budget":   task.CostBudget,
				})
				return nil, status.Errorf(codes.ResourceExhausted, "task %s cost budget exceeded: consumed=%d budget=%d", req.TaskId, task.CostConsumed, task.CostBudget)
			}
		}
	}

	// Capability gate: the step's Biscuit token must be signed by the broker
	// root and must grant the scope this tool requires. This is enforced before
	// any routing — a forged or over-broad token never reaches a tool.
	if s.deps.Capability != nil {
		if req.CapabilityToken == "" {
			return nil, status.Error(codes.PermissionDenied, "capability token required")
		}
		raw, err := base64.StdEncoding.DecodeString(req.CapabilityToken)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "malformed capability token")
		}
		if err := s.deps.Capability.Authorize(raw, scope); err != nil {
			// A datalog runtime-limit error means evaluation never reached a
			// decision, so the call still fails closed but is reported as a
			// transient fault: emitting tool.denied here would write a policy
			// decision that was never computed into the WORM audit chain, and
			// Unavailable (not PermissionDenied) tells the caller to retry.
			if errors.Is(err, capability.ErrAuthorizeTimeout) {
				s.deps.Logger.Error("InvokeTool: capability evaluation timed out",
					zap.String("tool_id", req.ToolId),
					zap.String("scope", scope),
					zap.Error(err),
				)
				return nil, status.Errorf(codes.Unavailable, "capability check for %q could not complete; retry", scope)
			}
			s.deps.Logger.Warn("InvokeTool: capability denied",
				zap.String("tool_id", req.ToolId),
				zap.String("scope", scope),
				zap.Error(err),
			)
			s.emitToolDenied(ctx, req, scope, "capability_denied", err, nil)
			return nil, status.Errorf(codes.PermissionDenied, "capability does not grant %q", scope)
		}
	}

	// Per-call RBAC for MCP tools: InvokeTool re-checks whether the calling
	// agent is still permitted to access the connector. A revoked FGA grant
	// denies the next call without requiring a new capability token.
	// Fails closed: empty agent_id, a nil Policy engine, a Check error, a
	// transport/DB error loading the task, or a caller-supplied agent_id that
	// doesn't match the task's own bound agent (when the task is loadable) all
	// deny.
	effectiveAgentID := req.AgentId
	if strings.HasPrefix(req.ToolId, "mcp:") {
		connID, _, ok := toolregistry.ParseMcpToolID(req.ToolId)
		if !ok {
			return nil, status.Errorf(codes.InvalidArgument, "malformed mcp tool_id %q", req.ToolId)
		}
		agentID := strings.TrimSpace(req.AgentId)
		if agentID == "" {
			return nil, status.Errorf(codes.PermissionDenied, "agent_id required for mcp tool calls")
		}
		// A transport/DB error loading the task means boundAgentID couldn't be
		// established, so the mismatch check below would silently trust
		// agentID verbatim — fail closed instead of reopening that gap.
		if taskLoadFailed {
			s.emitToolDenied(ctx, req, scope, "mcp_task_load_error", nil, nil)
			return nil, status.Errorf(codes.Internal, "task load failed, cannot verify agent binding")
		}
		// A caller could otherwise pass an agent_id different from the one
		// actually bound to the task to widen its FGA/rate-limit/baseline
		// accounting beyond what its own task grants it.
		if boundAgentID != "" && !strings.EqualFold(agentID, boundAgentID) {
			s.emitToolDenied(ctx, req, scope, "mcp_agent_mismatch", nil, map[string]any{
				"req_agent_id":   agentID,
				"bound_agent_id": boundAgentID,
			})
			return nil, status.Errorf(codes.PermissionDenied, "agent_id %q does not match task's bound agent", agentID)
		}
		if boundAgentID != "" {
			agentID = boundAgentID // prefer the task-derived id
		}
		effectiveAgentID = agentID
		agentRef := agentID
		if !strings.HasPrefix(agentRef, "agent:") {
			agentRef = "agent:" + agentRef
		}
		object := "mcp_connector:" + connID
		if s.deps.Policy == nil {
			s.emitToolDenied(ctx, req, "", "mcp_check_error", errors.New("policy engine not configured"), nil)
			return nil, status.Errorf(codes.PermissionDenied, "mcp access check unavailable")
		}
		allowed, cerr := s.deps.Policy.CheckFGA(ctx, agentRef, "can_access", object)
		if cerr != nil {
			s.deps.Logger.Warn("InvokeTool: mcp fga check failed",
				zap.String("tool_id", req.ToolId),
				zap.String("agent", agentRef),
				zap.String("object", object),
				zap.Error(cerr),
			)
			s.emitToolDenied(ctx, req, "", "mcp_check_error", cerr, nil)
			return nil, status.Errorf(codes.PermissionDenied, "mcp access check failed: %v", cerr)
		}
		if !allowed {
			s.deps.Logger.Warn("InvokeTool: mcp access denied by fga",
				zap.String("tool_id", req.ToolId),
				zap.String("agent", agentRef),
				zap.String("object", object),
			)
			s.emitToolDenied(ctx, req, "", "mcp_access_denied", nil, map[string]any{
				"agent":  agentRef,
				"object": object,
			})
			return nil, status.Errorf(codes.PermissionDenied, "agent %q does not have access to mcp_connector:%s", agentRef, connID)
		}
	}

	if err := s.memoryGroupPreGate(ctx, req, scope, taskOwnerUserID, taskLoadFailed); err != nil {
		return nil, err
	}

	// Rate-limit check: RPM enforcement before dispatching the tool.
	// Fails closed: if the limiter returns ResourceExhausted, deny immediately.
	if s.deps.RateLimiter != nil {
		if rlErr := s.deps.RateLimiter.CheckToolInvocation(ctx, tenantID, effectiveAgentID); rlErr != nil {
			s.emitRateLimitDenied(ctx, req, tenantID, "rpm")
			return nil, rlErr
		}
	}

	// Authorized: route to the Tool Proxy for the controlled side-effect.
	if s.deps.ToolProxy == nil {
		return &brokerv1.InvokeToolResponse{
			Success: false,
			Error:   "tool proxy not configured",
		}, nil
	}
	var args map[string]any
	if req.Args != nil {
		args = req.Args.AsMap()
	}

	// Every gate has passed and the side-effect is about to run, so this is the
	// point the task becomes EXECUTING. InvokeTool is the only execution path in
	// the broker, which makes it the only correct producer of this transition —
	// without it a task stops dead at APPROVED, the caller's terminal EmitStatus
	// is rejected (APPROVED has no edge to COMPLETED/FAILED), and ADR-010's
	// EXECUTING → CANCELLED is unreachable. Conditional on APPROVED so a task
	// with several tool calls transitions once and the rest are no-ops; a
	// bookkeeping failure warns rather than failing an authorized call.
	if tasks := s.deps.Tasks; tasks != nil && taskState == db.TaskStateApproved {
		if tErr := tasks.Transition(ctx, tenantID, req.TaskId, db.TaskStateApproved, db.TaskStateExecuting); tErr != nil {
			s.deps.Logger.Warn("InvokeTool: APPROVED → EXECUTING transition failed",
				zap.String("task_id", req.TaskId), zap.Error(tErr))
		}
	}

	invokeStart := time.Now()
	res, err := s.deps.ToolProxy.Invoke(ctx, toolproxy.Request{
		ToolID:   req.ToolId,
		Args:     args,
		TaskID:   req.TaskId,
		TenantID: tenantID,
		UserID:   req.UserId,
		// Task-row-sourced, never req.AgentId: memory.*'s scope=agent resolves a
		// bundle segment from this, so a caller-supplied id would be a
		// cross-agent memory read or write.
		AgentID: boundAgentID,
	})
	if s.deps.TaskMetrics != nil {
		s.deps.TaskMetrics.RecordToolInvokeDuration(ctx, time.Since(invokeStart).Seconds(), tenantID, req.ToolId)
	}
	if err != nil {
		// Proxy-internal failure (e.g. registry/handler drift) — not a tool error.
		s.deps.Logger.Error("InvokeTool: proxy error", zap.String("tool_id", req.ToolId), zap.Error(err))
		return nil, status.Errorf(codes.Internal, "tool proxy error")
	}
	out, mErr := toolproxy.ResultStruct(res.Output)
	if mErr != nil {
		// The tool already ran; its result just can't be marshalled — either a
		// string carries invalid UTF-8, or a handler returned a Go type structpb
		// won't walk (a typed slice like []map[string]any rather than []any).
		// Log here but do NOT
		// early-return: the audit emit, injection scan, cost increment, and
		// baseline observe below must still run (they read res, not out) so an
		// already-completed call's cost is always recorded. The tool-level
		// failure is returned at the end, replacing the opaque codes.Internal
		// that used to kill the RPC.
		s.deps.Logger.Warn("InvokeTool: tool result failed to encode",
			zap.String("tool_id", req.ToolId), zap.Error(mErr))
	}

	// Emit a rich tool-invocation audit event (the observability stream shows
	// the actual tool, scope, cost, and outcome — not just the generic RPC).
	auditCtx := map[string]any{
		"tool_id":    req.ToolId,
		"scope":      scope,
		"cost_units": int(res.CostUnits),
		"outcome":    map[bool]string{true: "COMPLETED", false: "FAILED"}[res.Success],
	}
	if res.Error != "" {
		auditCtx["error"] = res.Error
	}
	auditDecision := auditv1.PolicyDecision_ALLOW
	if !res.Success {
		auditDecision = auditv1.PolicyDecision_DENY
	}
	if s.deps.Audit != nil {
		auditStruct, _ := structpb.NewStruct(auditCtx)
		if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
			EventId:     ids.EventID(),
			TenantId:    tenantID,
			OccurredAt:  timestampNow(),
			ActorUserId: req.UserId,
			EventType:   "aikonos.broker.tool.invoked",
			ResourceRef: "aikonos:tool:" + req.ToolId,
			Decision:    auditDecision,
			Context:     auditStruct,
		}); err != nil {
			audit.RecordEmitFailure(ctx, s.deps.Logger, err, "aikonos.broker.tool.invoked")
		}
	}

	// Extraction-result injection scan (checkpoint 10): a successful call
	// whose result carries a non-empty injection_flags annotation (set by the
	// *.extract handlers' deterministic pattern scan, toolproxy/result_scan.go)
	// gets a dedicated audit event alongside tool.invoked above. Annotate-only
	// — never blocks; res.Success is untouched.
	if res.Success {
		if flags, ok := res.Output["injection_flags"].([]any); ok && len(flags) > 0 {
			s.emitResultFlagged(ctx, req, tenantID, flags)
		}
	}

	// Baseline observation (CP-B3): agent-bound successful calls feed the
	// automated behavioral baseline learner. Personal tasks (no agent_id) have
	// no stable agent key and are out of scope. Fire-and-forget — never
	// affects the return.
	if s.deps.Baseline != nil && res.Success && strings.TrimSpace(effectiveAgentID) != "" {
		s.deps.Baseline.Observe(ctx, tenantID, effectiveAgentID, req.ToolId, res.CostUnits)
	}

	// Always-record: a successful call's cost is persisted even when it
	// crosses the budget — the call already ran, so an accounting failure of
	// any kind must never fail an already-completed tool call. The pre-gate
	// above denies the *next* call once cost_consumed reflects the crossing.
	if res.Success && res.CostUnits > 0 {
		if tasks := s.deps.Tasks; tasks != nil {
			if incErr := tasks.IncrementCost(ctx, tenantID, req.TaskId, res.CostUnits); incErr != nil {
				if errors.Is(incErr, db.ErrBudgetExceeded) {
					s.emitBudgetExceeded(ctx, tenantID, req, preCallConsumed+res.CostUnits, taskCostBudget, incErr)
				} else {
					s.deps.Logger.Warn("InvokeTool: increment cost failed",
						zap.String("task_id", req.TaskId), zap.Error(incErr))
				}
			}
		}
	}

	if mErr != nil {
		// Side effects above have run; surface the encode failure to the model.
		// Deliberately does not name a cause: invalid UTF-8 in a string is one
		// way to get here, a handler returning a typed slice or map that structpb
		// cannot walk is another, and asserting the wrong one sends whoever reads
		// this looking in the wrong place.
		return &brokerv1.InvokeToolResponse{
			Success:           false,
			Error:             "tool result could not be encoded",
			CostUnitsConsumed: res.CostUnits,
		}, nil
	}
	return &brokerv1.InvokeToolResponse{
		Success:           res.Success,
		Result:            out,
		Error:             res.Error,
		CostUnitsConsumed: res.CostUnits,
	}, nil
}

func (s *SandboxService) EmitStatus(ctx context.Context, req *brokerv1.EmitStatusRequest) (*brokerv1.EmitStatusResponse, error) {
	s.deps.Logger.Info("SandboxStatus",
		zap.String("task_id", req.TaskId),
		zap.String("status", req.Status.String()),
	)

	toState, ok := protoStatusToDBState(req.Status)
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "unmappable task status %q", req.Status)
	}

	// South-bound calls carry no OIDC tenant; the sandbox echoes the task's
	// tenant so the per-tenant RLS lookup resolves. Fall back to the broker's
	// default tenant for older callers that don't set it.
	tenant := req.TenantId
	if tenant == "" {
		tenant = s.deps.TenantID
	}

	tasks := s.deps.Tasks
	t, err := tasks.Get(ctx, tenant, req.TaskId)
	if err != nil {
		s.deps.Logger.Warn("EmitStatus: task not found",
			zap.String("task_id", req.TaskId), zap.String("tenant_id", tenant))
		if errors.Is(err, db.ErrNotFound) {
			return nil, status.Errorf(codes.NotFound, "task %s not found", req.TaskId)
		}
		return nil, status.Errorf(codes.Internal, "failed to get task")
	}

	if err := tasks.Transition(ctx, tenant, req.TaskId, t.State, toState); err != nil {
		s.deps.Logger.Warn("EmitStatus: transition rejected", zap.Error(err))
		return nil, status.Errorf(codes.FailedPrecondition, "cannot transition task %s from %s to %s: %v", req.TaskId, t.State, toState, err)
	}
	publishTaskEvent(ctx, s.deps, tenant, req.TaskId, "STATUS_CHANGED", map[string]any{
		"status":      req.Status.String(),
		"description": req.Description,
	})
	return &brokerv1.EmitStatusResponse{}, nil
}

func (s *SandboxService) RequestSubagent(ctx context.Context, req *brokerv1.RequestSubagentRequest) (*brokerv1.SubagentHandle, error) {
	// TODO Phase 4: spawn sub-agent with depth check
	return nil, status.Error(codes.Unimplemented, "sub-agents not yet implemented (Phase 4)")
}

// emitRateLimitDenied fires a rate-limit enforcement audit event. Fire-and-forget.
func (s *SandboxService) emitRateLimitDenied(ctx context.Context, req *brokerv1.InvokeToolRequest, tenantID, limitType string) {
	if s.deps.Audit == nil {
		return
	}
	ctxStruct, _ := structpb.NewStruct(map[string]any{
		"limit_type": limitType,
		"agent_id":   req.AgentId,
		"tool_id":    req.ToolId,
	})
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TenantId:    tenantID,
		OccurredAt:  timestampNow(),
		ActorUserId: req.UserId,
		EventType:   "aikonos.broker.rate_limit.enforced",
		ResourceRef: "aikonos:tool:" + req.ToolId,
		Decision:    auditv1.PolicyDecision_DENY,
		Context:     ctxStruct,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, "aikonos.broker.rate_limit.enforced")
	}
}
