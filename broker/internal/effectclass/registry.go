// Package effectclass is the single source of truth for effect-class routing,
// runtime selection, and Rego string mappings.
// Engine, sandbox, and OPA generator all derive from this table — nothing is
// duplicated elsewhere.
//
//go:generate go run ./gen
package effectclass

import (
	"sort"
	"strings"

	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// Routing describes how the governance engine should handle a tool invocation
// for a given effect class.  The three flags are independent; an engine should
// consult all three that are true.
type Routing struct {
	Auto            bool // allow without human approval
	RequireApproval bool // gate on human HITL approval
	RequireStepUp   bool // additionally require step-up authentication
}

// Entry is one row in the canonical effect-class registry.
type Entry struct {
	Class   planv1.EffectClass
	Rego    string // lowercase rego identifier (e.g. "read_only")
	Routing Routing
	Runtime string // sandbox runtime class: "gvisor" for most classes; "kata" for the three
	// step-up classes (credential_access, destructive, infrastructure) which need
	// stronger isolation.  Advisory only under the docker-compose deployment — the
	// sandbox manager is disabled there, so no runtime class is applied.
}

// canonical is the authoritative table.  All 8 non-UNSPECIFIED classes must
// appear here; the parity test fails if proto and this table diverge.
//
// Routing reproduces current tool_invocation.rego behaviour exactly — including
// write_internal's "all-false" quirk (neither auto nor approval).  That quirk is
// a pre-existing behaviour preserved intentionally; a follow-up tracks it.
var canonical = []Entry{
	{
		Class:   planv1.EffectClass_READ_ONLY,
		Rego:    "read_only",
		Routing: Routing{Auto: true},
		Runtime: "gvisor",
	},
	{
		Class:   planv1.EffectClass_WRITE_LOCAL,
		Rego:    "write_local",
		Routing: Routing{Auto: true},
		Runtime: "gvisor",
	},
	{
		Class: planv1.EffectClass_WRITE_INTERNAL,
		Rego:  "write_internal",
		// All-false Routing → the engine's fail-closed default denies the step
		// (classifyDecision: !Allow && !NeedsApproval && !NeedsStepUp → stepDeny).
		// write_internal is reserved/unused: no tool maps to it today and any step
		// declaring it is denied until a tool and explicit routing are defined.
		Routing: Routing{},
		Runtime: "gvisor",
	},
	{
		Class:   planv1.EffectClass_WRITE_EXTERNAL,
		Rego:    "write_external",
		Routing: Routing{RequireApproval: true},
		Runtime: "gvisor",
	},
	{
		Class:   planv1.EffectClass_NETWORK_EGRESS,
		Rego:    "network_egress",
		Routing: Routing{RequireApproval: true},
		Runtime: "gvisor",
	},
	{
		Class:   planv1.EffectClass_CREDENTIAL_ACCESS,
		Rego:    "credential_access",
		Routing: Routing{RequireApproval: true, RequireStepUp: true},
		Runtime: "kata",
	},
	{
		Class:   planv1.EffectClass_DESTRUCTIVE,
		Rego:    "destructive",
		Routing: Routing{RequireApproval: true, RequireStepUp: true},
		Runtime: "kata",
	},
	{
		Class:   planv1.EffectClass_INFRASTRUCTURE,
		Rego:    "infrastructure",
		Routing: Routing{RequireApproval: true, RequireStepUp: true},
		Runtime: "kata",
	},
}

// indexByClass is built once at init for O(1) lookups.
var indexByClass map[planv1.EffectClass]Entry
var indexByRego map[string]Entry

func init() {
	indexByClass = make(map[planv1.EffectClass]Entry, len(canonical))
	indexByRego = make(map[string]Entry, len(canonical))
	for _, e := range canonical {
		indexByClass[e.Class] = e
		indexByRego[e.Rego] = e
	}
}

// All returns a stable copy of the canonical table sorted by ascending enum
// value.  The order is stable across calls — the generator (CP3) depends on it.
func All() []Entry {
	out := make([]Entry, len(canonical))
	copy(out, canonical)
	sort.Slice(out, func(i, j int) bool {
		return out[i].Class < out[j].Class
	})
	return out
}

// RegoString returns the lowercase Rego identifier for ec (e.g. READ_ONLY →
// "read_only").  Returns "" for EFFECT_CLASS_UNSPECIFIED.
func RegoString(ec planv1.EffectClass) string {
	if ec == planv1.EffectClass_EFFECT_CLASS_UNSPECIFIED {
		return ""
	}
	if e, ok := indexByClass[ec]; ok {
		return e.Rego
	}
	// Fallback for any future proto value not yet in the table; keeps behaviour
	// consistent with strings.ToLower(ec.String()) until parity test fires.
	return strings.ToLower(ec.String())
}

// Runtime returns the sandbox RuntimeClass for ec.  Defaults to "gvisor" for
// UNSPECIFIED and any unknown value (preserves SelectRuntime's default branch).
func Runtime(ec planv1.EffectClass) string {
	if e, ok := indexByClass[ec]; ok {
		return e.Runtime
	}
	return "gvisor"
}

// RuntimeForRego returns the sandbox RuntimeClass for the lowercase Rego string
// s.  Defaults to "gvisor" for unknown strings (sandbox caller holds the string
// form, not the enum).
func RuntimeForRego(s string) string {
	if e, ok := indexByRego[s]; ok {
		return e.Runtime
	}
	return "gvisor"
}

// RoutingForClass returns the Routing for ec.  Returns the zero Routing for
// UNSPECIFIED or unknown values.
func RoutingForClass(ec planv1.EffectClass) Routing {
	return indexByClass[ec].Routing
}

// RoutingForRego returns the Routing for the lowercase Rego string s.  Returns
// the zero Routing for unknown strings.
func RoutingForRego(s string) Routing {
	return indexByRego[s].Routing
}
