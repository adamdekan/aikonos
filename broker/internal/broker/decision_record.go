package broker

// decision_record.go — the aikonos.broker.policy.decision audit event. One is
// emitted per policy decision and carries what the decision was made from: the
// policy revision OPA evaluated with, OPA's input and result, which of the
// broker's own layers changed the outcome, and the tool, access and approval
// settings in force. With the archived policy snapshot it is enough to re-run
// the OPA part of the decision (scripts/replay-decision.sh). See
// docs/15-decision-replay.md.

import (
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/approvalsvc"
	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/netacl"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
)

// PolicyDecisionEvent is the event type of a decision record.
const PolicyDecisionEvent = "aikonos.broker.policy.decision"

// Decision record kinds.
const (
	decisionKindToolInvocation = "tool_invocation"
	decisionKindPlanValidation = "plan_validation"
	decisionKindEnvelopeSend   = "envelope_send"
	decisionKindAgentSkills    = "agent_skill_boundary"
)

// decisionRecord is the context of a PolicyDecisionEvent.
type decisionRecord struct {
	Kind     string            `json:"kind"`
	Outcome  decisionOutcome   `json:"outcome"`
	Plan     *planRef          `json:"plan,omitempty"`
	Step     *stepRef          `json:"step,omitempty"`
	OPA      *policy.Evidence  `json:"opa,omitempty"`
	Layers   []layerRecord     `json:"layers,omitempty"`
	Settings map[string]any    `json:"settings,omitempty"`
	FGA      *fgaRecord        `json:"fga,omitempty"`
	Approval *approvalsvc.Gate `json:"approval,omitempty"`
}

// decisionOutcome is what was decided and which part of the pipeline decided it.
type decisionOutcome struct {
	Decision  string   `json:"decision"`   // allow | approval_required | step_up_required | deny
	DecidedBy string   `json:"decided_by"` // e.g. "opa:aikonos/tool_invocation", "netacl", "tenant_config:disabled_tools"
	Reason    string   `json:"reason,omitempty"`
	Reasons   []string `json:"reasons,omitempty"`
}

type planRef struct {
	TaskID string `json:"task_id"`
	PlanID string `json:"plan_id,omitempty"`
}

type stepRef struct {
	TaskID string `json:"task_id"`
	PlanID string `json:"plan_id,omitempty"`
	Seq    int32  `json:"seq"`
	ToolID string `json:"tool_id"`
}

// layerRecord is one broker layer that ran after OPA and changed the outcome.
type layerRecord struct {
	Layer  string         `json:"layer"`
	Effect string         `json:"effect"` // the outcome it imposed
	Detail map[string]any `json:"detail,omitempty"`
}

// fgaRecord is the OpenFGA check behind a decision.
type fgaRecord struct {
	ModelID  string `json:"model_id,omitempty"`
	User     string `json:"user"`
	Relation string `json:"relation"`
	Object   string `json:"object"`
	Allowed  bool   `json:"allowed"`
	Error    string `json:"error,omitempty"`
}

// decisionString names a step category the way records and the console do.
func decisionString(c stepCategory) string {
	switch c {
	case stepDeny:
		return "deny"
	case stepStepUp:
		return "step_up_required"
	case stepApproval:
		return "approval_required"
	default:
		return "allow"
	}
}

func auditDecisionFor(c stepCategory) auditv1.PolicyDecision {
	switch c {
	case stepDeny:
		return auditv1.PolicyDecision_DENY
	case stepAllow:
		return auditv1.PolicyDecision_ALLOW
	default:
		return auditv1.PolicyDecision_APPROVAL_REQUIRED
	}
}

// routeLayers lists the broker layers after OPA that changed a step's outcome,
// in the order they ran, and names the one that decided it: the last of them,
// or OPA (dec.PolicyRuleID) when none did.
func routeLayers(dec *policy.Decision, rt routeTrace) ([]layerRecord, string) {
	var layers []layerRecord
	if rt.NetaclMutated {
		effect := "deny"
		if rt.NetaclAction == netacl.Ask {
			effect = "approval_required"
		}
		layers = append(layers, layerRecord{Layer: "netacl", Effect: effect,
			Detail: map[string]any{"host": rt.NetaclHost, "action": string(rt.NetaclAction)}})
	}
	if rt.DisabledToolsHit {
		layers = append(layers, layerRecord{Layer: "tenant_config:disabled_tools", Effect: "deny"})
	}
	if rt.OverlayDisableHit {
		layers = append(layers, layerRecord{Layer: "skill_overlay", Effect: "deny"})
	}
	if rt.ConfigOverride != nil {
		posture, _ := rt.ConfigOverride["posture"].(string)
		layers = append(layers, layerRecord{Layer: "tenant_config:effect_class_routing", Effect: posture, Detail: rt.ConfigOverride})
	}
	decidedBy := dec.PolicyRuleID
	if dec.Evidence != nil && decidedBy == "opa:tool_invocation" {
		decidedBy = "opa:" + dec.Evidence.Query
	}
	if n := len(layers); n > 0 {
		decidedBy = layers[n-1].Layer
	}
	return layers, decidedBy
}

// emitPolicyDecision emits one decision record. Fire-and-forget like every
// other audit event: a record that cannot be encoded is logged, never fails
// the decision it describes.
func emitPolicyDecision(ctx context.Context, emitter auditEmitter, logger *zap.Logger, ev *auditv1.AuditEvent, rec decisionRecord) {
	if emitter == nil {
		return
	}
	ctxStruct, err := recordStruct(rec)
	if err != nil {
		logger.Error("policy decision record could not be encoded", zap.String("kind", rec.Kind), zap.Error(err))
	} else {
		ev.Context = ctxStruct
	}
	ev.EventId = ids.EventID()
	ev.EventType = PolicyDecisionEvent
	ev.OccurredAt = timestampNow()
	if err := emitter.Emit(ctx, ev); err != nil {
		audit.RecordEmitFailure(ctx, logger, err, PolicyDecisionEvent)
	}
}

// recordStruct converts a record to the event's context Struct by way of JSON,
// so the stored context is exactly the record's JSON form.
func recordStruct(v any) (*structpb.Struct, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		return nil, fmt.Errorf("decision record context: %w", err)
	}
	return s, nil
}
