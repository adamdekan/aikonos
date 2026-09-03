package effectclass_test

import (
	"strings"
	"testing"

	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"

	"github.com/adamdekan/aikonos/broker/internal/effectclass"
)

// TestParity verifies every non-UNSPECIFIED EffectClass has exactly one registry
// entry, and every registry entry maps to a real proto enum value.
// Fails on proto↔registry drift.
func TestParity(t *testing.T) {
	entries := effectclass.All()

	// Build set of registered classes.
	registeredByClass := make(map[planv1.EffectClass]effectclass.Entry)
	for _, e := range entries {
		if _, dup := registeredByClass[e.Class]; dup {
			t.Errorf("duplicate registry entry for class %v", e.Class)
		}
		registeredByClass[e.Class] = e
	}

	// Every non-UNSPECIFIED proto value must have a registry entry.
	for num, name := range planv1.EffectClass_name {
		if num == 0 {
			continue // EFFECT_CLASS_UNSPECIFIED is intentionally absent
		}
		ec := planv1.EffectClass(num)
		if _, ok := registeredByClass[ec]; !ok {
			t.Errorf("proto value %s (%d) has no registry entry", name, num)
		}
	}

	// Every registry entry must map to a real proto value.
	for _, e := range entries {
		if _, ok := planv1.EffectClass_name[int32(e.Class)]; !ok {
			t.Errorf("registry entry class %d is not a valid EffectClass proto value", e.Class)
		}
		if e.Class == planv1.EffectClass_EFFECT_CLASS_UNSPECIFIED {
			t.Errorf("registry must not contain UNSPECIFIED entry")
		}
	}
}

// TestRegoString verifies RegoString matches strings.ToLower(ec.String()) for
// all defined classes, and returns "" for UNSPECIFIED.
func TestRegoString(t *testing.T) {
	for num := range planv1.EffectClass_name {
		ec := planv1.EffectClass(num)
		got := effectclass.RegoString(ec)
		if num == 0 {
			if got != "" {
				t.Errorf("RegoString(UNSPECIFIED) = %q, want %q", got, "")
			}
			continue
		}
		want := strings.ToLower(ec.String())
		if got != want {
			t.Errorf("RegoString(%v) = %q, want %q", ec, got, want)
		}
	}
}

// TestRoutingTable verifies explicit per-class routing, including write_internal = all-false.
func TestRoutingTable(t *testing.T) {
	cases := []struct {
		class planv1.EffectClass
		want  effectclass.Routing
	}{
		// why: no durable external side-effect — safe to auto-approve without human in the loop.
		{planv1.EffectClass_READ_ONLY, effectclass.Routing{Auto: true, RequireApproval: false, RequireStepUp: false}},
		{planv1.EffectClass_WRITE_LOCAL, effectclass.Routing{Auto: true, RequireApproval: false, RequireStepUp: false}},
		// why: all-false is a quirk preserved verbatim from tool_invocation.rego — write_internal is
		// neither auto-approved nor routed to human approval; tracked as a follow-up to resolve.
		{planv1.EffectClass_WRITE_INTERNAL, effectclass.Routing{Auto: false, RequireApproval: false, RequireStepUp: false}},
		// why: external side-effect visible outside the system — a human must explicitly approve.
		{planv1.EffectClass_WRITE_EXTERNAL, effectclass.Routing{Auto: false, RequireApproval: true, RequireStepUp: false}},
		{planv1.EffectClass_NETWORK_EGRESS, effectclass.Routing{Auto: false, RequireApproval: true, RequireStepUp: false}},
		// why: irreversible or security-sensitive actions — require approval AND step-up auth on top.
		{planv1.EffectClass_CREDENTIAL_ACCESS, effectclass.Routing{Auto: false, RequireApproval: true, RequireStepUp: true}},
		{planv1.EffectClass_DESTRUCTIVE, effectclass.Routing{Auto: false, RequireApproval: true, RequireStepUp: true}},
		{planv1.EffectClass_INFRASTRUCTURE, effectclass.Routing{Auto: false, RequireApproval: true, RequireStepUp: true}},
	}

	for _, tc := range cases {
		got := effectclass.RoutingForClass(tc.class)
		if got != tc.want {
			t.Errorf("RoutingForClass(%v) = %+v, want %+v", tc.class, got, tc.want)
		}
	}
}

// TestRoutingForRego mirrors TestRoutingTable via the string-keyed accessor.
func TestRoutingForRego(t *testing.T) {
	cases := []struct {
		rego string
		want effectclass.Routing
	}{
		{"read_only", effectclass.Routing{Auto: true}},
		{"write_local", effectclass.Routing{Auto: true}},
		{"write_internal", effectclass.Routing{}},
		{"write_external", effectclass.Routing{RequireApproval: true}},
		{"network_egress", effectclass.Routing{RequireApproval: true}},
		{"credential_access", effectclass.Routing{RequireApproval: true, RequireStepUp: true}},
		{"destructive", effectclass.Routing{RequireApproval: true, RequireStepUp: true}},
		{"infrastructure", effectclass.Routing{RequireApproval: true, RequireStepUp: true}},
		{"unknown_string", effectclass.Routing{}}, // default = zero value
	}

	for _, tc := range cases {
		got := effectclass.RoutingForRego(tc.rego)
		if got != tc.want {
			t.Errorf("RoutingForRego(%q) = %+v, want %+v", tc.rego, got, tc.want)
		}
	}
}

// TestRuntime verifies Runtime returns the correct runtime for each class.
// Step-up classes (credential_access, destructive, infrastructure) use "kata";
// all others use "gvisor".  UNSPECIFIED and unknown values default to "gvisor".
func TestRuntime(t *testing.T) {
	// why: kata provides stronger isolation for step-up classes that handle
	// credentials or destructive/infra operations.
	kataClasses := map[planv1.EffectClass]bool{
		planv1.EffectClass_CREDENTIAL_ACCESS: true,
		planv1.EffectClass_DESTRUCTIVE:       true,
		planv1.EffectClass_INFRASTRUCTURE:    true,
	}
	for num := range planv1.EffectClass_name {
		ec := planv1.EffectClass(num)
		got := effectclass.Runtime(ec)
		want := "gvisor"
		if kataClasses[ec] {
			want = "kata"
		}
		if got != want {
			t.Errorf("Runtime(%v) = %q, want %q", ec, got, want)
		}
	}
}

// TestRuntimeForRego verifies RuntimeForRego returns the correct runtime for known strings.
// Step-up classes use "kata"; others use "gvisor"; unknown strings default to "gvisor".
func TestRuntimeForRego(t *testing.T) {
	cases := []struct {
		rego string
		want string
	}{
		{"read_only", "gvisor"},
		{"write_local", "gvisor"},
		{"write_internal", "gvisor"},
		{"write_external", "gvisor"},
		{"network_egress", "gvisor"},
		// why: kata for the three step-up classes that need stronger isolation.
		{"credential_access", "kata"},
		{"destructive", "kata"},
		{"infrastructure", "kata"},
		// unknown string → default gvisor
		{"totally_unknown", "gvisor"},
	}
	for _, tc := range cases {
		if got := effectclass.RuntimeForRego(tc.rego); got != tc.want {
			t.Errorf("RuntimeForRego(%q) = %q, want %q", tc.rego, got, tc.want)
		}
	}
}

// TestAllOrder verifies All() returns entries sorted by ascending enum value.
func TestAllOrder(t *testing.T) {
	entries := effectclass.All()
	for i := 1; i < len(entries); i++ {
		if entries[i].Class <= entries[i-1].Class {
			t.Errorf("All() not sorted at index %d: %v <= %v", i, entries[i].Class, entries[i-1].Class)
		}
	}
}
