package toolregistry

import (
	"fmt"

	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// effectClassEnum is the concrete type for effect-class values stored in the
// Registry. Defined here so toolregistry.go can reference it without an import
// cycle; it is exactly planv1.EffectClass.
type effectClassEnum = planv1.EffectClass

// staticBaselineEffectClass is the authoritative tool_id → effect class map
// for tools with no toolproxy plugin (siem.*, slack.*). Every toolproxy-
// registered tool id derives its effect class from its own plugin's
// EffectClass() instead — see
// Registry.LoadFromPlugins, wired from main.go via
// toolproxy.RegisteredPluginBaselines(). Do not invent classes for entries
// added here; this table is the verified truth from FU2.
//
// Routing uses Stricter(plan-declared, authoritative), so a planner can never
// route weaker than this table.  MCP and unknown tools return ok=false and fall
// back to plan-declared (the broker has no independent ground truth for MCP
// tool semantics).
var staticBaselineEffectClass = map[string]planv1.EffectClass{
	"siem.query":  planv1.EffectClass_READ_ONLY,
	"siem.search": planv1.EffectClass_READ_ONLY,
	"slack.post":  planv1.EffectClass_WRITE_EXTERNAL,
	"slack.dm":    planv1.EffectClass_WRITE_EXTERNAL,
}

// regoToEffectClass maps Rego string names to their planv1.EffectClass enum
// values for manifest overlay validation.
var regoToEffectClass = map[string]planv1.EffectClass{
	"read_only":         planv1.EffectClass_READ_ONLY,
	"write_local":       planv1.EffectClass_WRITE_LOCAL,
	"write_internal":    planv1.EffectClass_WRITE_INTERNAL,
	"write_external":    planv1.EffectClass_WRITE_EXTERNAL,
	"network_egress":    planv1.EffectClass_NETWORK_EGRESS,
	"credential_access": planv1.EffectClass_CREDENTIAL_ACCESS,
	"destructive":       planv1.EffectClass_DESTRUCTIVE,
	"infrastructure":    planv1.EffectClass_INFRASTRUCTURE,
}

// EffectClassFromRego converts a Rego string name (e.g. "write_local") to its
// planv1.EffectClass value. ok=false when the string is not a known class.
// Used by the CP3 overlay loader to convert stored string values back to enums.
func EffectClassFromRego(rego string) (planv1.EffectClass, bool) {
	ec, ok := regoToEffectClass[rego]
	return ec, ok
}

// AuthoritativeEffectClass returns the broker-authoritative effect class for
// toolID.  Lookup order: baselineEffectClass → overlay (manifest effect_class).
// Returns ok=false for MCP tools (mcp:<conn>:<tool>) and unknown tools — callers
// must fall back to plan-declared for those.
func (reg *Registry) AuthoritativeEffectClass(toolID string) (planv1.EffectClass, bool) {
	if ec, ok := reg.baselineEffectClass[toolID]; ok {
		return ec, true
	}
	// Overlay can extend the authoritative map for non-baseline tools. Manifests
	// cannot register mcp:-prefixed ids (LoadManifests sources only skill.yaml
	// tool names), so the overlay never shadows the MCP fallback below.
	if e, found := reg.overlay[toolID]; found && e.effectClass != "" {
		if ec, ok := regoToEffectClass[e.effectClass]; ok {
			return ec, true
		}
	}
	// MCP tools and unknowns: no broker-side ground truth.
	return planv1.EffectClass_EFFECT_CLASS_UNSPECIFIED, false
}

// checkEffectClassConflict returns an error when a manifest declares an
// effect_class for a baseline tool that disagrees with the authoritative
// baseline.  Mirrors the existing scope-conflict guard in LoadManifests.
func (reg *Registry) checkEffectClassConflict(toolID, manifestEC string) error {
	baseEC, inBase := reg.baselineEffectClass[toolID]
	if !inBase || manifestEC == "" {
		return nil
	}
	manifestEnum, known := regoToEffectClass[manifestEC]
	if !known {
		// Unknown string in the manifest — not a conflict with the baseline, but
		// the manifest itself is malformed.  Return an error so LoadManifests
		// fails closed.
		return fmt.Errorf(
			"toolregistry: manifest effect_class %q for %q is not a known effect class",
			manifestEC, toolID,
		)
	}
	if manifestEnum != baseEC {
		return fmt.Errorf(
			"toolregistry: manifest effect_class conflict for %q: baseline %v, manifest %q",
			toolID, baseEC, manifestEC,
		)
	}
	return nil
}
