package broker

// route_step.go — routeStep (C1+C2): the single escalation-order pipeline
// shared by SubmitPlan and SimulatePolicy. Five layers, fixed order:
//
//  1. OPA (policy.DecideToolCall)
//  2. netacl (web-egress access-list)
//  3. disabled_tools (tenant config)
//  4. overlay-disable (tenant skill overlay)
//  5. effect_class_routing (tenant config posture, monotonic-stricter only)
//
// routeStep is a pure decision function: it mutates and returns a
// *policy.Decision plus a routeTrace describing which layers fired. It never
// emits audit events, writes to the DB, or otherwise has side effects — the
// caller (SubmitPlan or SimulatePolicy) owns all of that, using the trace to
// reconstruct exactly what it needs (decision_trace notes, tool-denied/
// skill-disabled audits, response gates).
//
// C2: reads_sensitive is derived server-side at the top of the pipeline —
// effectclass.ReadsSensitive(EffectClass) — instead of trusting a client-sent
// boolean. SubmitPlan never supplies SensitivityKnob (nil ⇒ always derive).
// SimulatePolicy's what-if knob is honored only when ToolID does not resolve
// to a known registry effect class (a pure hypothetical); a known tool id
// always derives, matching the enforcement path.

import (
	"context"
	"fmt"

	"github.com/adamdekan/aikonos/broker/internal/config"
	"github.com/adamdekan/aikonos/broker/internal/effectclass"
	"github.com/adamdekan/aikonos/broker/internal/netacl"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// toolCallDecider is the minimal OPA surface routeStep needs. Both
// planPolicyEngine and simPolicyEngine satisfy it structurally.
type toolCallDecider interface {
	DecideToolCall(ctx context.Context, q policy.ToolQuery) (*policy.Decision, error)
}

// routeStepInput bundles every per-call input routeStep needs. Fields left at
// their zero value skip the corresponding optional layer (e.g. NetaclHost=""
// skips netacl; OverlayGateEnabled=false skips overlay-disable).
type routeStepInput struct {
	Engine toolCallDecider
	Reg    *toolregistry.Registry
	Cfg    Config
	ACL    aclDecider

	TenantID          string
	UserID            string
	ToolID            string
	EffectClass       planv1.EffectClass // already resolved by the caller (SubmitPlan's Stricter-merged routingClass; SimulatePolicy's req.EffectClass, unchanged)
	ActorSpiffeID     string
	FGADecision       string
	HasDLPAttestation bool

	// NetaclHost is the destination host to evaluate; "" skips the layer
	// (SubmitPlan only extracts this for web.fetch steps; SimulatePolicy
	// passes req.Host for any tool — that gating difference is the caller's,
	// not routeStep's, to preserve each caller's existing behavior exactly).
	NetaclHost string

	// OverlayGateEnabled mirrors SubmitPlan's existing `deps.OverlayLoader !=
	// nil` guard — the overlay-disable layer is skipped entirely when false.
	OverlayGateEnabled bool

	// SensitivityKnob is nil on the enforcement path (SubmitPlan): always
	// derive. On the simulator's what-if path it carries the client's
	// explicit reads_sensitive override, honored only when ToolID is not a
	// known registry tool.
	SensitivityKnob *bool
}

// routeTrace records which layers fired, so the caller can reconstruct its
// own audits / decision-trace notes / response gates without routeStep
// having any side effects of its own.
type routeTrace struct {
	// SensitivityDerived is true whenever ReadsSensitive was derived from
	// EffectClass (always true for SubmitPlan; true for SimulatePolicy
	// whenever ToolID resolves to a known registry effect class). False only
	// for the simulator's pure-hypothetical knob path.
	SensitivityDerived bool
	ReadsSensitive     bool // the value actually fed to policy.ToolQuery

	// PostOpaDecision is a snapshot of the decision immediately after layer 1
	// (OPA), before netacl or any later layer mutates it. SimulatePolicy uses
	// this to report its "access_routing" gate exactly as before — that gate
	// has always reflected the OPA/FGA decision alone, never netacl.
	PostOpaDecision policy.Decision

	// NetaclHost/NetaclAction are set whenever the netacl layer actually ran
	// (host non-empty and ACL enabled), regardless of the resulting action —
	// SimulatePolicy's "network" gate has always appeared whenever the check
	// ran, even for an ALLOW action. NetaclMutated is true only when the
	// layer actually changed dec (a Deny, or an Ask while still allowed) —
	// SubmitPlan's decision_trace["network"] note and its
	// aikonos.network.fetch.{denied,ask} audit only ever fired on mutation.
	NetaclHost    string
	NetaclAction  netacl.Action
	NetaclMutated bool

	DisabledToolsHit  bool
	OverlayDisableHit bool

	// ConfigOverride mirrors SubmitPlan's existing FU3 decision_trace note
	// (source/class/posture), nil when the config posture did not escalate.
	ConfigOverride map[string]any
}

// errRouteStepNilDecision is returned when the engine reports no error but
// also no decision — a synthetic safety net so both callers fail closed
// (codes.Internal) instead of risking a nil-pointer mutation in the layers
// below. SimulatePolicy already guarded this explicitly before routeStep
// existed; SubmitPlan gains the same guard for free.
type errRouteStepNilDecision struct{}

func (errRouteStepNilDecision) Error() string { return "policy evaluation returned no decision" }

// routeStep runs the unified 5-layer escalation pipeline and returns the
// final decision plus a trace of which layers fired. See the package-level
// doc comment for the exact order and the C1/C2 rationale.
func routeStep(ctx context.Context, in routeStepInput) (*policy.Decision, routeTrace, error) {
	var trace routeTrace

	// C2: derive reads_sensitive from the effect class being routed.
	trace.ReadsSensitive = effectclass.ReadsSensitive(in.EffectClass)
	trace.SensitivityDerived = true
	if in.SensitivityKnob != nil {
		if _, known := in.Reg.EffectClassForTenant(in.TenantID, in.ToolID); !known {
			trace.ReadsSensitive = *in.SensitivityKnob
			trace.SensitivityDerived = false
		}
	}

	// Layer 1 — OPA.
	dec, err := in.Engine.DecideToolCall(ctx, policy.ToolQuery{
		UserID:            in.UserID,
		TenantID:          in.TenantID,
		ToolID:            in.ToolID,
		EffectClass:       in.EffectClass,
		ActorSpiffeID:     in.ActorSpiffeID,
		ReadsSensitive:    trace.ReadsSensitive,
		HasDLPAttestation: in.HasDLPAttestation,
		FGADecision:       in.FGADecision,
	})
	if err != nil {
		return nil, trace, err
	}
	if dec == nil {
		return nil, trace, errRouteStepNilDecision{}
	}
	trace.PostOpaDecision = *dec

	// Layer 2 — netacl (web egress).
	if in.ACL != nil && in.ACL.Enabled() && in.NetaclHost != "" {
		switch in.ACL.Decide(ctx, in.TenantID, in.UserID, in.NetaclHost) {
		case netacl.Deny:
			trace.NetaclHost, trace.NetaclAction, trace.NetaclMutated = in.NetaclHost, netacl.Deny, true
			dec.Allow, dec.Deny, dec.NeedsApproval, dec.NeedsStepUp = false, true, false, false
			dec.Reason = fmt.Sprintf("network host %q is not permitted by the access-list", in.NetaclHost)
		case netacl.Ask:
			trace.NetaclHost, trace.NetaclAction = in.NetaclHost, netacl.Ask
			if dec.Allow {
				trace.NetaclMutated = true
				dec.Allow, dec.NeedsApproval = false, true
				dec.Reason = fmt.Sprintf("network host %q requires approval", in.NetaclHost)
			}
		case netacl.Allow:
			trace.NetaclHost, trace.NetaclAction = in.NetaclHost, netacl.Allow
		}
	}

	// Layer 3 — disabled_tools.
	if disabledList := config.ParseList(in.Cfg.GetString(ctx, in.TenantID, "disabled_tools")); toolInList(disabledList, in.ToolID) {
		dec.Allow, dec.Deny, dec.NeedsApproval, dec.NeedsStepUp = false, true, false, false
		dec.Reason = fmt.Sprintf("tool %q is disabled", in.ToolID)
		trace.DisabledToolsHit = true
	}

	// Layer 4 — overlay disable.
	if in.OverlayGateEnabled {
		if enabled, known := in.Reg.IsEnabled(in.TenantID, in.ToolID); known && !enabled {
			dec.Allow, dec.Deny, dec.NeedsApproval, dec.NeedsStepUp = false, true, false, false
			dec.Reason = fmt.Sprintf("tool %q is disabled", in.ToolID)
			trace.OverlayDisableHit = true
		}
	}

	// Layer 5 — effect_class_routing config posture (monotonic-stricter only).
	if ecRouting := config.ParseEffectClassRouting(in.Cfg.GetString(ctx, in.TenantID, "effect_class_routing")); len(ecRouting) > 0 {
		regoName := effectclass.RegoString(in.EffectClass)
		if posture, ok := ecRouting[regoName]; ok {
			if posture.Severity() > decisionSeverity(dec) {
				applyPosture(dec, posture)
				trace.ConfigOverride = map[string]any{
					"source":  "config",
					"class":   regoName,
					"posture": string(posture),
				}
			}
		}
	}

	return dec, trace, nil
}
