// broker/internal/toolregistry/toolregistry.go
// Maps a registered tool id to the capability scope a caller must hold to
// invoke it. This is the bridge between delegation scopes (siem:read) and tool
// ids (siem.query) — InvokeTool authorizes the presented Biscuit against the
// scope returned here.
//
// An unregistered tool has no scope and cannot be invoked (deny-by-default):
// plan.proto already requires a step's tool_id to exist in the broker's
// registry. The static table below is the dev registry; a real deployment will
// source this from the skill manifests (skills/<name>/skill.yaml).
//
// Dynamic MCP tools follow a prefix convention: tool_id "mcp:<C>:<tool>"
// maps to scope "mcp:<C>" (connector-scoped, connector-agnostic of the
// specific tool). The static registry is checked first; the prefix rule handles
// all remaining mcp: ids without a per-tool entry.
package toolregistry

import (
	"fmt"
	"strings"

	"github.com/adamdekan/aikonos/broker/internal/skillmanifest"
)

// staticBaseline is the authoritative static tool_id → scope map for tools
// with no toolproxy plugin (siem.*, slack.*) plus the mcp: prefix rule below.
// Every toolproxy-registered tool id derives its scope from its own plugin's
// Scope() instead — see
// Registry.LoadFromPlugins, wired from main.go via
// toolproxy.RegisteredPluginBaselines(). Manifests may annotate or extend this
// map but may never change a baseline scope.
var staticBaseline = map[string]string{
	"siem.query":  "siem:read",
	"siem.search": "siem:read",
	"slack.post":  "slack:write",
	"slack.dm":    "slack:write",
}

type overlayEntry struct {
	scope       string
	effectClass string
}

// Registry holds the tool scope and effect-class maps. Use NewRegistry to
// construct; the zero value is not usable.
type Registry struct {
	baseline            map[string]string
	baselineEffectClass map[string]effectClassEnum
	overlay             map[string]overlayEntry
	// tenantOverlay is the per-tenant skill overlay cache (CP2+). Reads and
	// writes go through SetTenantOverlay / IsEnabled / EffectClassForTenant.
	tenantOverlay tenantOverlayPtr
}

// NewRegistry returns a Registry seeded with the static baseline scope and
// authoritative effect-class tables.
func NewRegistry() *Registry {
	return &Registry{
		baseline:            copyStringMap(staticBaseline),
		baselineEffectClass: copyEffectClassMap(staticBaselineEffectClass),
		overlay:             map[string]overlayEntry{},
	}
}

// PluginBaseline is a neutral tool_id → scope/effect-class entry sourced from
// a toolproxy plugin's own Scope()/EffectClass() methods (C11,
// ). Defined here — not in toolproxy — because
// toolproxy already imports toolregistry (ParseMcpToolID); the reverse import
// would cycle, so toolproxy converts its plugins into this neutral shape via
// RegisteredPluginBaselines() and hands the slice back to LoadFromPlugins.
type PluginBaseline struct {
	ToolID      string
	Scope       string
	EffectClass effectClassEnum
}

// LoadFromPlugins populates reg's baseline scope and effect-class maps from
// plugin-declared entries, making the plugin the single source of truth for
// its own id instead of a hand-synced staticBaseline/staticBaselineEffectClass
// row. Called once at startup (main.go) after both the registry and the
// plugin registry exist. Idempotent.
func (reg *Registry) LoadFromPlugins(entries []PluginBaseline) {
	for _, e := range entries {
		reg.baseline[e.ToolID] = e.Scope
		reg.baselineEffectClass[e.ToolID] = e.EffectClass
	}
}

// LoadManifests loads skill manifests from dir and builds the overlay.
// Tools already in the baseline are accepted only when their declared scope
// matches exactly; a mismatch returns a fail-closed error and leaves the
// overlay unchanged. New tool_ids are added to the overlay. Idempotent.
func (reg *Registry) LoadManifests(dir string) error {
	manifests, err := skillmanifest.Load(dir)
	if err != nil {
		return err
	}

	tmp := make(map[string]overlayEntry, len(manifests))
	for _, m := range manifests {
		id := m.ToolID()
		if baseScope, inBase := reg.baseline[id]; inBase {
			if m.Scope() != baseScope {
				return fmt.Errorf(
					"toolregistry: manifest scope conflict for %q: baseline %q, manifest %q",
					id, baseScope, m.Scope(),
				)
			}
			// Fail closed if the manifest's effect_class disagrees with the
			// authoritative baseline — mirrors the scope-conflict guard above.
			if err := reg.checkEffectClassConflict(id, m.EffectClass()); err != nil {
				return err
			}
			// Scope matches baseline — record effect-class only.
			tmp[id] = overlayEntry{scope: baseScope, effectClass: m.EffectClass()}
		} else {
			tmp[id] = overlayEntry{scope: m.Scope(), effectClass: m.EffectClass()}
		}
	}

	reg.overlay = tmp
	return nil
}

// RequiredScope returns the capability scope a tool requires. ok is false for
// an unregistered tool — callers must treat that as a denial.
//
// Lookup order: baseline → overlay → MCP prefix rule.
func (reg *Registry) RequiredScope(toolID string) (scope string, ok bool) {
	if scope, ok = reg.baseline[toolID]; ok {
		return
	}
	if e, found := reg.overlay[toolID]; found {
		return e.scope, true
	}
	if connID, _, isMcp := ParseMcpToolID(toolID); isMcp {
		return "mcp:" + connID, true
	}
	return "", false
}

// ToolEntry holds the tool_id and its required capability scope.
type ToolEntry struct {
	ToolID string
	Scope  string
}

// ListTools returns all registered tools (baseline + overlay) as a slice.
// The MCP prefix rule is not enumerated — only named entries are returned.
// Order is not guaranteed.
func (reg *Registry) ListTools() []ToolEntry {
	seen := make(map[string]struct{}, len(reg.baseline)+len(reg.overlay))
	out := make([]ToolEntry, 0, len(reg.baseline)+len(reg.overlay))
	for id, scope := range reg.baseline {
		seen[id] = struct{}{}
		out = append(out, ToolEntry{ToolID: id, Scope: scope})
	}
	for id, e := range reg.overlay {
		if _, already := seen[id]; already {
			continue
		}
		out = append(out, ToolEntry{ToolID: id, Scope: e.scope})
	}
	return out
}

// EffectClass returns the manifest-declared effect-class for toolID.
// ok is false when no manifest has been loaded for that tool (baseline-only
// tools without a manifest, MCP tools, or unknown tools all return false).
// This is informational — it does not affect routing.
func (reg *Registry) EffectClass(toolID string) (string, bool) {
	if e, found := reg.overlay[toolID]; found && e.effectClass != "" {
		return e.effectClass, true
	}
	return "", false
}

// IsBuiltin reports whether toolID is in the static baseline. A baseline tool
// has a guaranteed executor (builtin); manifest-overlay tools may extend scope
// but are NOT in the baseline. CP3 uses this to distinguish the two so it can
// reject manifest-only ids (I2).
func (reg *Registry) IsBuiltin(toolID string) bool {
	_, ok := reg.baseline[toolID]
	return ok
}

// ParseMcpToolID parses a tool_id of the form "mcp:<connectorId>:<tool>" into
// its connector id and tool name components. ok is false if the id does not
// match the pattern (wrong prefix, too few segments, or an empty component).
// Pure function — no registry state needed.
func ParseMcpToolID(toolID string) (connectorID, tool string, ok bool) {
	if !strings.HasPrefix(toolID, "mcp:") {
		return "", "", false
	}
	rest := toolID[len("mcp:"):]
	// rest must be "<connectorId>:<tool>": exactly one more colon.
	idx := strings.Index(rest, ":")
	if idx <= 0 {
		return "", "", false
	}
	connectorID = rest[:idx]
	tool = rest[idx+1:]
	if tool == "" {
		return "", "", false
	}
	return connectorID, tool, true
}

// ── package-default instance (escape hatch for callers that can't be threaded) ──

// pkgDefault is the package-level Registry used by the package-level func
// delegates below. It is seeded by NewRegistry at init time so tests and any
// non-broker callers that call the package-level funcs continue to work without
// change.
var pkgDefault = NewRegistry()

// PkgDefault returns the package-default Registry. Used as a nil-safe fallback
// in broker service methods when ToolRegistry is not injected (e.g. older unit
// tests). Production code injects a Registry via Deps.ToolRegistry instead.
func PkgDefault() *Registry { return pkgDefault }

// LoadManifests is a package-level delegate to the package-default Registry.
// Used by toolregistry tests. Production loads manifests into the injected
// Registry (main.go calls toolReg.LoadManifests on the instance), NOT pkgDefault.
func LoadManifests(dir string) error { return pkgDefault.LoadManifests(dir) }

// RequiredScope is a package-level delegate to the package-default Registry.
func RequiredScope(toolID string) (string, bool) { return pkgDefault.RequiredScope(toolID) }

// EffectClass is a package-level delegate to the package-default Registry.
func EffectClass(toolID string) (string, bool) { return pkgDefault.EffectClass(toolID) }

// AuthoritativeEffectClass is a package-level delegate to the package-default
// Registry.
func AuthoritativeEffectClass(toolID string) (effectClassEnum, bool) {
	return pkgDefault.AuthoritativeEffectClass(toolID)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func copyStringMap(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func copyEffectClassMap(src map[string]effectClassEnum) map[string]effectClassEnum {
	dst := make(map[string]effectClassEnum, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
