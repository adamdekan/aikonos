package toolregistry

import (
	"fmt"
	"sync/atomic"

	"github.com/adamdekan/aikonos/broker/internal/effectclass"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// OverlayRow is the unit callers pass to SetTenantOverlay. CP3 builds these
// from DB rows; CP2 holds the data structures only (no db import).
type OverlayRow struct {
	ToolID      string
	Enabled     bool
	EffectClass planv1.EffectClass
	Scope       string
	ExecutorKind string
}

// overlayState is the per-tool data stored in a snapshot.
type overlayState struct {
	enabled      bool
	effectClass  planv1.EffectClass
	scope        string
	executorKind string
}

// tenantOverlaySnapshot is the immutable value held behind the atomic pointer.
// Key: tenant → tool_id → state. Never mutated after construction.
type tenantOverlaySnapshot map[string]map[string]overlayState

// tenantOverlay is the atomic pointer stored on Registry.
// Using a named struct so atomic.Pointer[T] has a concrete named type.
type tenantOverlayPtr = atomic.Pointer[tenantOverlaySnapshot]

// SetTenantOverlay atomically swaps the overlay snapshot so that tenant's rows
// are replaced by rows. Other tenants' rows are preserved (copy-on-write).
// It never mutates the live snapshot, so concurrent readers see a consistent view.
func (reg *Registry) SetTenantOverlay(tenant string, rows []OverlayRow) {
	// Load current snapshot (may be nil on first call).
	cur := reg.tenantOverlay.Load()

	// Build a shallow copy of all tenants, then overwrite the target tenant.
	next := make(tenantOverlaySnapshot)
	if cur != nil {
		for t, m := range *cur {
			if t == tenant {
				continue // will be replaced below
			}
			next[t] = m
		}
	}

	toolMap := make(map[string]overlayState, len(rows))
	for _, r := range rows {
		toolMap[r.ToolID] = overlayState{
			enabled:      r.Enabled,
			effectClass:  r.EffectClass,
			scope:        r.Scope,
			executorKind: r.ExecutorKind,
		}
	}
	next[tenant] = toolMap

	reg.tenantOverlay.Store(&next)
}

// IsEnabled reports whether the tool is present in the tenant overlay and
// whether it is enabled. known=false means no overlay row exists for this
// (tenant, toolID) pair — callers treat that as the existing denied path.
func (reg *Registry) IsEnabled(tenant, toolID string) (enabled bool, known bool) {
	snap := reg.tenantOverlay.Load()
	if snap == nil {
		return false, false
	}
	toolMap, ok := (*snap)[tenant]
	if !ok {
		return false, false
	}
	state, ok := toolMap[toolID]
	if !ok {
		return false, false
	}
	return state.enabled, true
}

// EffectClassForTenant returns the authoritative effect class for (tenant,
// toolID). If a tenant overlay row exists, its stored class is returned —
// it was already validated and floored at write time (I4/I5). Otherwise,
// the method delegates to the global AuthoritativeEffectClass (signature
// unchanged, other callers unaffected).
func (reg *Registry) EffectClassForTenant(tenant, toolID string) (planv1.EffectClass, bool) {
	snap := reg.tenantOverlay.Load()
	if snap != nil {
		if toolMap, ok := (*snap)[tenant]; ok {
			if state, ok := toolMap[toolID]; ok {
				return state.effectClass, true
			}
		}
	}
	return reg.AuthoritativeEffectClass(toolID)
}

// DeriveScope derives the capability scope for toolID without accepting one
// from the caller (invariant I3). Built-in → its baseline scope via
// RequiredScope. MCP → "mcp:<connectorID>". Anything else → error (CP3 uses
// this to reject net-new tool_ids with no executor).
func (reg *Registry) DeriveScope(toolID string) (string, error) {
	// Check static baseline first (includes overlay-extended tools from manifests).
	if scope, ok := reg.baseline[toolID]; ok {
		return scope, nil
	}
	// MCP prefix rule.
	if connID, _, ok := ParseMcpToolID(toolID); ok {
		return "mcp:" + connID, nil
	}
	// Manifest overlay can extend with non-baseline, non-mcp tools (e.g. dropbox.read).
	if e, ok := reg.overlay[toolID]; ok {
		return e.scope, nil
	}
	return "", fmt.Errorf("toolregistry: no executor for tool_id %q: only built-in and mcp: ids are allowed", toolID)
}

// ResolveOverlayClass computes the authoritative effect class to store for a
// skill overlay row, applying enum validation and the Stricter floor.
//
// Rules (in order):
//  1. Validate declaredRego is a known effect class — unknown → error, fail-closed.
//  2. executorKind == "mcp": floor is write_external (net-new MCP tools may do
//     anything externally; the operator cannot attest they are read-only). Return
//     Stricter(declared, write_external). This may raise the class but never lowers it.
//  3. Built-in (toolID in staticBaselineEffectClass): the compiled class is ground
//     truth. Storing a class weaker than the baseline would silently downgrade
//     enforcement for existing callers (I4 "may never be weaker"). We detect this
//     as Severity(declared) < Severity(baseline) and return an error so the editor
//     sees an explicit rejection rather than a silent raise. A stricter or equal
//     declaration is accepted; the stored value = Stricter(declared, baseline).
//  4. Non-baseline, non-mcp built-in (manifest-overlay tool): no compiled floor;
//     return declared as-is (already enum-validated).
func (reg *Registry) ResolveOverlayClass(toolID, executorKind, declaredRego string) (planv1.EffectClass, error) {
	// Step 1: enum-validate.
	declared, ok := regoToEffectClass[declaredRego]
	if !ok {
		return planv1.EffectClass_EFFECT_CLASS_UNSPECIFIED,
			fmt.Errorf("toolregistry: unknown effect class %q: must be one of read_only, write_local, write_internal, write_external, network_egress, credential_access, destructive, infrastructure", declaredRego)
	}

	// Step 2: MCP floor.
	if executorKind == "mcp" {
		return effectclass.Stricter(declared, planv1.EffectClass_WRITE_EXTERNAL), nil
	}

	// Step 3: built-in immutability check.
	if baseline, inBaseline := reg.baselineEffectClass[toolID]; inBaseline {
		if effectclass.Severity(declared) < effectclass.Severity(baseline) {
			return planv1.EffectClass_EFFECT_CLASS_UNSPECIFIED,
				fmt.Errorf("toolregistry: cannot lower effect class for built-in %q: baseline is %v, declared %q is weaker", toolID, baseline, declaredRego)
		}
		return effectclass.Stricter(declared, baseline), nil
	}

	// Step 4: manifest-extended (or any other) tool — return validated declared.
	return declared, nil
}
