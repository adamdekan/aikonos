// broker/internal/toolproxy/plugin.go
//
// Handler self-registration, mirroring connector/plugin.go: each handler file
// registers its plugin(s) via init() instead of New hardcoding the tool-id
// table. Unlike the connector plugins (pure metadata), a ToolPlugin also
// builds the Handler itself since handlers need constructor-time deps pulled
// out of Config (workspace root, connector secrets, etc).
package toolproxy

import (
	"sort"

	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"

	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
)

// ToolPlugin describes one statically-registered tool handler. The plugin is
// the single source of truth for its own id's capability scope and effect
// class — toolregistry.staticBaseline /
// staticBaselineEffectClass no longer carry a hand-authored entry for any
// registered plugin; RegisteredPluginBaselines derives them at startup.
type ToolPlugin interface {
	ToolID() string
	// Available reports whether cfg carries the deps this handler needs.
	Available(cfg Config) bool
	// Build constructs the handler. Called only when Available is true.
	Build(cfg Config) Handler
	// Scope returns the capability scope a caller must hold to invoke ToolID.
	Scope() string
	// EffectClass returns the broker-authoritative effect class for ToolID.
	EffectClass() planv1.EffectClass
}

var plugins = map[string]ToolPlugin{}

// RegisterPlugin adds p to the package-level registry. Panics on a duplicate
// ToolID because registration happens at init time and a collision is a
// programming error.
func RegisterPlugin(p ToolPlugin) {
	if _, exists := plugins[p.ToolID()]; exists {
		panic("toolproxy: duplicate plugin registration for tool " + p.ToolID())
	}
	plugins[p.ToolID()] = p
}

// RegisteredToolIDs returns every self-registered tool id, sorted for
// deterministic output. Used by New (handler wiring) and by conformance
// tests asserting registry ground truth.
func RegisteredToolIDs() []string {
	out := make([]string, 0, len(plugins))
	for id := range plugins {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// RegisteredPluginBaselines returns every self-registered plugin's
// {ToolID, Scope, EffectClass} as neutral toolregistry entries, sorted by
// ToolID for deterministic output. main.go feeds this to
// toolregistry.Registry.LoadFromPlugins so the registry derives plugin-backed
// scope/effect-class instead of carrying a second hand-synced table (C11).
func RegisteredPluginBaselines() []toolregistry.PluginBaseline {
	ids := RegisteredToolIDs()
	out := make([]toolregistry.PluginBaseline, 0, len(ids))
	for _, id := range ids {
		p := plugins[id]
		out = append(out, toolregistry.PluginBaseline{
			ToolID:      id,
			Scope:       p.Scope(),
			EffectClass: p.EffectClass(),
		})
	}
	return out
}
