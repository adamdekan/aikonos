package broker

// simulate.go — SimulatePolicy RPC: side-effect-free dry-run of the
// (subject, tool, effect-class, host, fga-object) tuple through the live
// evaluators. Tenant-admin gated. No writes, no transitions, no token minting.

import (
	"context"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

func (s *BrokerService) SimulatePolicy(ctx context.Context, req *brokerv1.SimulatePolicyRequest) (*brokerv1.SimulatePolicyResponse, error) {
	tenant, user, err := s.callerIdentity(ctx, "", "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}

	subject := req.SubjectUserId
	eng := s.simPolicyFor()
	var gates []*brokerv1.GateDecision

	// Gate 1 — identity (informational)
	gates = append(gates, &brokerv1.GateDecision{
		Gate:    "identity",
		Outcome: "INFO",
		Reason:  subject,
	})

	// Gate 2 (optional) — FGA access check (informational).
	// Models the InvokeTool-time can_access check, which is separate from the
	// plan-time routing gate — so the simulator neither over- nor under-states
	// the live decision. Result does NOT feed DecideToolCall (Gate 3).
	// CheckFGA takes a prefixed subject ref ("user:<id>"); OPA uses the bare id.
	if req.FgaObject != "" {
		allowed, fgaErr := eng.CheckFGA(ctx, "user:"+subject, "can_access", req.FgaObject)
		if fgaErr != nil {
			return nil, status.Errorf(codes.Internal, "policy evaluation failed")
		}
		fgaOutcome := "ALLOW"
		if !allowed {
			fgaOutcome = "DENY"
		}
		gates = append(gates, &brokerv1.GateDecision{
			Gate:    "access",
			Outcome: fgaOutcome,
			Detail:  req.FgaObject,
		})
	}

	// Gates 3-5 (unified) — CP2: routeStep runs the same 5-layer escalation
	// pipeline SubmitPlan uses (OPA → netacl → disabled_tools → overlay-disable
	// → effect_class_routing), so the simulator now honors the three layers it
	// used to skip despite simulate.go's original parity comment.
	//
	// ToolQuery matches SubmitPlan's plan-time call otherwise: no FGADecision
	// fed here — per-call FGA can_access is enforced at InvokeTool runtime
	// (modelled by the informational "access" gate above), not plan-time
	// routing. ActorSpiffeID/HasDLPAttestation are omitted: matches a plan
	// with no sandbox identity / no DLP attestation, the conservative
	// (stricter) default.
	//
	// C2: reads_sensitive is derived from EffectClass whenever ToolId names a
	// known registry tool (same as enforcement); the explicit ReadsSensitive
	// knob is honored only for a pure-hypothetical (unknown) tool id.
	if s.deps.OverlayLoader != nil {
		s.deps.OverlayLoader.ensureLoaded(ctx, tenant, s.deps.Logger)
	}
	sensitivityKnob := req.ReadsSensitive
	dec, rtrace, err := routeStep(ctx, routeStepInput{
		Engine:             eng,
		Reg:                s.toolReg(),
		Cfg:                s.configFor(),
		ACL:                s.aclFor(),
		TenantID:           tenant,
		UserID:             subject,
		ToolID:             req.ToolId,
		EffectClass:        req.EffectClass,
		NetaclHost:         req.Host,
		OverlayGateEnabled: s.deps.OverlayLoader != nil,
		SensitivityKnob:    &sensitivityKnob,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "policy evaluation failed")
	}

	// Gate 3a — sensitivity (informational): reports whether reads_sensitive
	// was derived server-side from EffectClass or taken from the request's
	// what-if knob (CP2's derive-vs-knob split).
	sensSource := "knob"
	if rtrace.SensitivityDerived {
		sensSource = "derived"
	}
	gates = append(gates, &brokerv1.GateDecision{
		Gate:    "sensitivity",
		Outcome: "INFO",
		Reason:  sensSource,
		Detail:  strconv.FormatBool(rtrace.ReadsSensitive),
	})

	// Gate 3 — OPA/FGA routing decision. Reports the pre-netacl snapshot —
	// this gate has always reflected the OPA/FGA decision alone, never netacl.
	opaDec := rtrace.PostOpaDecision
	arGate := &brokerv1.GateDecision{
		Gate:    "access_routing",
		Outcome: decisionOutcomeString(&opaDec),
		RuleId:  opaDec.PolicyRuleID,
		Reason:  opaDec.Reason,
	}
	if len(opaDec.Reasons) > 0 {
		arGate.Detail = strings.Join(opaDec.Reasons, "; ")
	}
	gates = append(gates, arGate)

	// Gate 4 (optional) — network ACL. Appears whenever the layer actually
	// ran (host set + ACL enabled), regardless of the resulting action.
	if rtrace.NetaclHost != "" {
		gates = append(gates, &brokerv1.GateDecision{
			Gate:    "network",
			Outcome: string(rtrace.NetaclAction),
			Detail:  rtrace.NetaclHost,
		})
	}

	// Gate 5 — capability (informational)
	capScope := ""
	if sc, ok := s.toolReg().RequiredScope(req.ToolId); ok {
		capScope = sc
	}
	gates = append(gates, &brokerv1.GateDecision{
		Gate:    "capability",
		Outcome: "INFO",
		Detail:  capScope,
	})

	// Aggregate outcome: routeStep's final dec already folds in netacl +
	// disabled_tools + overlay-disable + effect_class_routing, in that order.
	outcome := decisionOutcomeString(dec)

	s.emitSimulatedAudit(ctx, tenant, user, subject, req.ToolId, req.EffectClass.String(), outcome)

	return &brokerv1.SimulatePolicyResponse{
		Outcome: outcome,
		Gates:   gates,
	}, nil
}

// decisionOutcomeString maps a Decision to its outcome string label using
// the shared classifyDecision helper.
func decisionOutcomeString(dec *policy.Decision) string {
	return categoryString(classifyDecision(dec))
}

// categoryString converts a stepCategory to its wire-format outcome string.
func categoryString(cat stepCategory) string {
	switch cat {
	case stepDeny:
		return "DENY"
	case stepStepUp:
		return "NEEDS_STEP_UP"
	case stepApproval:
		return "NEEDS_HUMAN"
	default:
		return "ALLOW"
	}
}

// emitSimulatedAudit fires a fire-and-forget ALLOW audit event for the
// simulation. Emitting ALLOW (not DENY) signals "an admin ran a what-if",
// never a real denial. Nil-guarded on deps.Audit.
func (s *BrokerService) emitSimulatedAudit(ctx context.Context, tenant, actor, subject, toolID, effectClass, outcome string) {
	if s.deps.Audit == nil {
		return
	}
	traceID := trace.SpanContextFromContext(ctx).TraceID().String()
	ctxStruct, _ := structpb.NewStruct(map[string]any{
		"subject":      subject,
		"tool_id":      toolID,
		"effect_class": effectClass,
		"outcome":      outcome,
	})
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: actor,
		EventType:   "aikonos.broker.policy.simulated",
		ResourceRef: "aikonos:tool:" + toolID,
		Decision:    auditv1.PolicyDecision_ALLOW,
		Context:     ctxStruct,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, "aikonos.broker.policy.simulated")
	}
}
