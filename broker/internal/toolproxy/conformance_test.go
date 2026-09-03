// broker/internal/toolproxy/conformance_test.go
//
// Guards against toolproxy/toolregistry drift: every tool a plugin
// self-registers must have an authoritative effect-class entry in the
// toolregistry, or the broker cannot classify it for approval routing.
package toolproxy

import (
	"testing"

	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
)

// TestRegisteredTools_HaveAuthoritativeEffectClass builds a toolregistry
// Registry the way production does (broker/cmd/broker/main.go:
// NewRegistry() + LoadManifests(skillsDir) + LoadFromPlugins(...)) and
// asserts every self-registered toolproxy plugin resolves to an
// authoritative effect class. Post-C11 this
// is true by construction — the registry derives plugin-backed scope/effect-
// class from the plugin itself via LoadFromPlugins, not a hand-kept table.
// The reverse direction (baseline ids without a toolproxy handler, e.g.
// siem.query/slack.post — routed elsewhere) is legitimate and not asserted
// here.
func TestRegisteredTools_HaveAuthoritativeEffectClass(t *testing.T) {
	reg := toolregistry.NewRegistry()
	if err := reg.LoadManifests("../../../skills"); err != nil {
		t.Fatalf("LoadManifests(skills): %v", err)
	}
	reg.LoadFromPlugins(RegisteredPluginBaselines())

	for _, id := range RegisteredToolIDs() {
		if _, ok := reg.AuthoritativeEffectClass(id); !ok {
			t.Errorf(
				"toolproxy plugin %q has no authoritative effect class in toolregistry; "+
					"add a staticBaselineEffectClass entry (authoritative.go) or a skill manifest "+
					"(skills/%s/skill.yaml) declaring effect_class",
				id, id,
			)
		}
	}
}

// TestRegisteredTools_HaveRequiredScope guards against the other half of the
// same drift class: an id present in staticBaselineEffectClass (authoritative.go)
// but missing from staticBaseline (toolregistry.go, tool_id -> scope). RequiredScope
// pins toolproxy/toolregistry drift: a missing scope makes a tool unable to be
// resolved for capability classification and approval routing.
func TestRegisteredTools_HaveRequiredScope(t *testing.T) {
	reg := toolregistry.NewRegistry()
	if err := reg.LoadManifests("../../../skills"); err != nil {
		t.Fatalf("LoadManifests(skills): %v", err)
	}
	reg.LoadFromPlugins(RegisteredPluginBaselines())

	for _, id := range RegisteredToolIDs() {
		if scope, ok := reg.RequiredScope(id); !ok || scope == "" {
			t.Errorf(
				"toolproxy plugin %q has no required scope in toolregistry; "+
					"add a staticBaseline entry (toolregistry.go) mapping it to a capability scope",
				id,
			)
		}
	}
}
