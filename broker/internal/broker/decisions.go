package broker

// decisions.go — GetDecisionTrace RPC: assembles the four-gate per-step trace
// for a task from the enriched plan_steps record. Tenant-admin gated.

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// simPolicyFor returns the simPolicyEngine to use: the test-injected fake if
// set, otherwise the concrete Policy from deps.Policy.
func (s *BrokerService) simPolicyFor() simPolicyEngine {
	if s.simPolicy != nil {
		return s.simPolicy
	}
	return s.deps.Policy
}

// aclFor returns the aclDecider to use (deps.ACL; settable in tests via Deps.ACL).
func (s *BrokerService) aclFor() aclDecider {
	return s.deps.ACL
}

func (s *BrokerService) GetDecisionTrace(ctx context.Context, req *brokerv1.GetDecisionTraceRequest) (*brokerv1.GetDecisionTraceResponse, error) {
	tenant, user, err := s.callerIdentity(ctx, "", "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}

	traces, err := s.deps.Tasks.GetPlanStepTraces(ctx, tenant, req.TaskId)
	if err != nil {
		s.deps.Logger.Error("GetDecisionTrace: GetPlanStepTraces failed",
			zap.String("task_id", req.TaskId),
			zap.Error(err),
		)
		return nil, status.Errorf(codes.Internal, "failed to read decision trace")
	}

	steps := make([]*brokerv1.StepTrace, 0, len(traces))
	for _, tr := range traces {
		steps = append(steps, &brokerv1.StepTrace{
			Seq:           tr.Seq,
			ToolId:        tr.ToolID,
			EffectClass:   tr.EffectClass,
			Decision:      tr.PolicyDecision,
			Justification: tr.Justification,
			Gates:         assembleStepGates(s.toolReg(), tr),
		})
	}

	s.emitDecisionExplainedAudit(ctx, tenant, user, req.TaskId, len(steps))

	return &brokerv1.GetDecisionTraceResponse{
		TaskId: req.TaskId,
		Steps:  steps,
	}, nil
}

// assembleStepGates builds the ordered four-gate slice for one plan step.
// Gates are: identity (always INFO), access_routing (OPA decision),
// network (conditional on DecisionTrace["network"]), capability (always INFO).
// Type-asserts on map values are defensive: wrong type → zero value, no panic.
func assembleStepGates(reg *toolregistry.Registry, tr db.PlanStepTrace) []*brokerv1.GateDecision {
	dt := tr.DecisionTrace
	if dt == nil {
		dt = map[string]any{}
	}
	var gates []*brokerv1.GateDecision

	// Gate 1 — identity (informational; owner not loaded to avoid DB round-trip)
	gates = append(gates, &brokerv1.GateDecision{
		Gate:    "identity",
		Outcome: "INFO",
		Reason:  "authenticated caller",
	})

	// Gate 2 — access_routing: the aggregate OPA/policy verdict for this step
	arGate := &brokerv1.GateDecision{
		Gate:    "access_routing",
		Outcome: tr.PolicyDecision,
		RuleId:  tr.PolicyRuleID,
		Reason:  tr.PolicyReason,
	}
	if detail := joinTraceReasons(dt["reasons"]); detail != "" {
		arGate.Detail = detail
	}
	gates = append(gates, arGate)

	// Gate 3 — network: only when DecisionTrace["network"] is a map
	if netRaw, ok := dt["network"]; ok {
		if netMap, ok := netRaw.(map[string]any); ok {
			action, _ := netMap["action"].(string)
			reason, _ := netMap["reason"].(string)
			host, _ := netMap["host"].(string)
			gates = append(gates, &brokerv1.GateDecision{
				Gate:    "network",
				Outcome: action,
				Reason:  reason,
				Detail:  host,
			})
		}
	}

	// Gate 4 — capability (informational: scope the token must carry)
	capScope, _ := dt["capability_scope"].(string)
	if capScope == "" {
		if sc, ok := reg.RequiredScope(tr.ToolID); ok {
			capScope = sc
		}
	}
	gates = append(gates, &brokerv1.GateDecision{
		Gate:    "capability",
		Outcome: "INFO",
		Detail:  capScope,
	})

	return gates
}

// joinTraceReasons joins the "reasons" value from a DecisionTrace to a
// "; "-delimited string. Accepts []any (JSON-decoded) or []string (test).
func joinTraceReasons(raw any) string {
	switch v := raw.(type) {
	case []any:
		parts := make([]string, 0, len(v))
		for _, r := range v {
			if s, ok := r.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "; ")
	case []string:
		return strings.Join(v, "; ")
	}
	return ""
}

// emitDecisionExplainedAudit fires a fire-and-forget ALLOW audit event.
// Nil-guarded on deps.Audit.
func (s *BrokerService) emitDecisionExplainedAudit(ctx context.Context, tenant, actor, taskID string, stepCount int) {
	if s.deps.Audit == nil {
		return
	}
	traceID := trace.SpanContextFromContext(ctx).TraceID().String()
	ctxStruct, _ := structpb.NewStruct(map[string]any{
		"task_id":    taskID,
		"step_count": float64(stepCount),
	})
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: actor,
		EventType:   "aikonos.broker.decision.explained",
		ResourceRef: "aikonos:task:" + taskID,
		Decision:    auditv1.PolicyDecision_ALLOW,
		Context:     ctxStruct,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, "aikonos.broker.decision.explained")
	}
}
