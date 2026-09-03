package toolregistry

import (
	"testing"

	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// newBuiltinTestRegistry seeds a fresh Registry the way main.go seeds toolReg
// post-C11: NewRegistry() + LoadFromPlugins.
// toolregistry cannot import toolproxy (would cycle — see PluginBaseline's
// doc comment), so the literal scope/effect-class values toolproxy's own
// web.fetch/doc.write plugins declare are restated here for tests that need a
// plugin-backed id to behave like a loaded baseline tool.
func newBuiltinTestRegistry() *Registry {
	reg := NewRegistry()
	reg.LoadFromPlugins([]PluginBaseline{
		{ToolID: "web.fetch", Scope: "web:read", EffectClass: planv1.EffectClass_READ_ONLY},
		{ToolID: "doc.write", Scope: "doc:write", EffectClass: planv1.EffectClass_WRITE_LOCAL},
	})
	return reg
}

// ── DeriveScope ───────────────────────────────────────────────────────────────

func TestDeriveScope_BuiltIn(t *testing.T) {
	reg := newBuiltinTestRegistry()
	scope, err := reg.DeriveScope("web.fetch")
	if err != nil {
		t.Fatalf("DeriveScope(web.fetch): unexpected error %v", err)
	}
	if scope != "web:read" {
		t.Errorf("DeriveScope(web.fetch) = %q, want %q", scope, "web:read")
	}
}

func TestDeriveScope_MCP(t *testing.T) {
	reg := NewRegistry()
	scope, err := reg.DeriveScope("mcp:conn-abc:some-tool")
	if err != nil {
		t.Fatalf("DeriveScope(mcp:conn-abc:some-tool): unexpected error %v", err)
	}
	if scope != "mcp:conn-abc" {
		t.Errorf("DeriveScope(mcp:conn-abc:some-tool) = %q, want %q", scope, "mcp:conn-abc")
	}
}

func TestDeriveScope_Unknown(t *testing.T) {
	reg := NewRegistry()
	_, err := reg.DeriveScope("totally.unknown.tool")
	if err == nil {
		t.Error("DeriveScope(unknown): expected error, got nil")
	}
}

// ── ResolveOverlayClass ───────────────────────────────────────────────────────

func TestResolveOverlayClass_UnknownRego_Rejected(t *testing.T) {
	reg := NewRegistry()
	_, err := reg.ResolveOverlayClass("web.fetch", "builtin", "not_a_real_class")
	if err == nil {
		t.Error("ResolveOverlayClass with unknown rego: expected error, got nil")
	}
}

func TestResolveOverlayClass_MCP_FloorApplied(t *testing.T) {
	reg := NewRegistry()
	// Declare read_only for an MCP tool; floor is write_external.
	got, err := reg.ResolveOverlayClass("mcp:conn-abc:some-tool", "mcp", "read_only")
	if err != nil {
		t.Fatalf("ResolveOverlayClass(mcp, read_only): unexpected error %v", err)
	}
	if got != planv1.EffectClass_WRITE_EXTERNAL {
		t.Errorf("ResolveOverlayClass(mcp, read_only) = %v, want WRITE_EXTERNAL", got)
	}
}

func TestResolveOverlayClass_MCP_AboveFloor_Kept(t *testing.T) {
	reg := NewRegistry()
	// Declare destructive for an MCP tool; above the floor, keep declared.
	got, err := reg.ResolveOverlayClass("mcp:conn-abc:some-tool", "mcp", "destructive")
	if err != nil {
		t.Fatalf("ResolveOverlayClass(mcp, destructive): unexpected error %v", err)
	}
	if got != planv1.EffectClass_DESTRUCTIVE {
		t.Errorf("ResolveOverlayClass(mcp, destructive) = %v, want DESTRUCTIVE", got)
	}
}

func TestResolveOverlayClass_BuiltIn_DowngradeRejected(t *testing.T) {
	reg := NewRegistry()
	// web.fetch baseline is read_only (severity 1); declaring read_only matches — fine.
	// slack.post baseline is write_external (severity 2); declaring read_only (severity 1) is a
	// downgrade — must be rejected.
	_, err := reg.ResolveOverlayClass("slack.post", "builtin", "read_only")
	if err == nil {
		t.Error("ResolveOverlayClass(builtin, downgrade): expected error, got nil")
	}
}

func TestResolveOverlayClass_BuiltIn_MatchAllowed(t *testing.T) {
	reg := newBuiltinTestRegistry()
	// doc.write baseline is write_local; declaring write_local is not a downgrade.
	got, err := reg.ResolveOverlayClass("doc.write", "builtin", "write_local")
	if err != nil {
		t.Fatalf("ResolveOverlayClass(builtin, match): unexpected error %v", err)
	}
	if got != planv1.EffectClass_WRITE_LOCAL {
		t.Errorf("ResolveOverlayClass(builtin, match) = %v, want WRITE_LOCAL", got)
	}
}

func TestResolveOverlayClass_BuiltIn_StricterAccepted(t *testing.T) {
	reg := newBuiltinTestRegistry()
	// web.fetch baseline is read_only; declaring write_external is stricter — stored value = write_external.
	got, err := reg.ResolveOverlayClass("web.fetch", "builtin", "write_external")
	if err != nil {
		t.Fatalf("ResolveOverlayClass(builtin, upgrade): unexpected error %v", err)
	}
	if got != planv1.EffectClass_WRITE_EXTERNAL {
		t.Errorf("ResolveOverlayClass(builtin, upgrade) = %v, want WRITE_EXTERNAL", got)
	}
}

// ── IsEnabled / SetTenantOverlay ─────────────────────────────────────────────

func TestIsEnabled_NoOverlay_Unknown(t *testing.T) {
	reg := NewRegistry()
	enabled, known := reg.IsEnabled("tenant-a", "web.fetch")
	if known {
		t.Errorf("IsEnabled with no overlay: known=true, want false; enabled=%v", enabled)
	}
}

func TestIsEnabled_AfterSet_KnownEnabled(t *testing.T) {
	reg := NewRegistry()
	reg.SetTenantOverlay("tenant-a", []OverlayRow{
		{ToolID: "web.fetch", Enabled: true, EffectClass: planv1.EffectClass_READ_ONLY, Scope: "web:read", ExecutorKind: "builtin"},
	})
	enabled, known := reg.IsEnabled("tenant-a", "web.fetch")
	if !known {
		t.Error("IsEnabled after set: known=false, want true")
	}
	if !enabled {
		t.Error("IsEnabled after set: enabled=false, want true")
	}
}

func TestIsEnabled_AfterSet_KnownDisabled(t *testing.T) {
	reg := NewRegistry()
	reg.SetTenantOverlay("tenant-a", []OverlayRow{
		{ToolID: "web.fetch", Enabled: false, EffectClass: planv1.EffectClass_READ_ONLY, Scope: "web:read", ExecutorKind: "builtin"},
	})
	enabled, known := reg.IsEnabled("tenant-a", "web.fetch")
	if !known {
		t.Error("IsEnabled (disabled row): known=false, want true")
	}
	if enabled {
		t.Error("IsEnabled (disabled row): enabled=true, want false")
	}
}

func TestIsEnabled_WrongTenant_Unknown(t *testing.T) {
	reg := NewRegistry()
	reg.SetTenantOverlay("tenant-a", []OverlayRow{
		{ToolID: "web.fetch", Enabled: true, EffectClass: planv1.EffectClass_READ_ONLY, Scope: "web:read", ExecutorKind: "builtin"},
	})
	// Different tenant has no overlay.
	_, known := reg.IsEnabled("tenant-b", "web.fetch")
	if known {
		t.Error("IsEnabled for different tenant: known=true, want false")
	}
}

// ── SetTenantOverlay atomic swap ─────────────────────────────────────────────

func TestSetTenantOverlay_AtomicReplace(t *testing.T) {
	reg := NewRegistry()
	reg.SetTenantOverlay("tenant-a", []OverlayRow{
		{ToolID: "web.fetch", Enabled: true, EffectClass: planv1.EffectClass_READ_ONLY, Scope: "web:read", ExecutorKind: "builtin"},
		{ToolID: "doc.write", Enabled: true, EffectClass: planv1.EffectClass_WRITE_LOCAL, Scope: "doc:write", ExecutorKind: "builtin"},
	})

	// Second call fully replaces the first (doc.write disappears).
	reg.SetTenantOverlay("tenant-a", []OverlayRow{
		{ToolID: "slack.post", Enabled: true, EffectClass: planv1.EffectClass_WRITE_EXTERNAL, Scope: "slack:write", ExecutorKind: "builtin"},
	})

	// doc.write must now be unknown for tenant-a (was in the old snapshot).
	_, known := reg.IsEnabled("tenant-a", "doc.write")
	if known {
		t.Error("after atomic replace, old row doc.write should be unknown")
	}
	// slack.post must be present.
	_, known = reg.IsEnabled("tenant-a", "slack.post")
	if !known {
		t.Error("after atomic replace, new row slack.post should be known")
	}
}

func TestSetTenantOverlay_IsolatesTenants(t *testing.T) {
	reg := NewRegistry()
	reg.SetTenantOverlay("tenant-a", []OverlayRow{
		{ToolID: "web.fetch", Enabled: true, EffectClass: planv1.EffectClass_READ_ONLY, Scope: "web:read", ExecutorKind: "builtin"},
	})
	reg.SetTenantOverlay("tenant-b", []OverlayRow{
		{ToolID: "doc.write", Enabled: false, EffectClass: planv1.EffectClass_WRITE_LOCAL, Scope: "doc:write", ExecutorKind: "builtin"},
	})

	// tenant-a must not see tenant-b's doc.write.
	_, knownA := reg.IsEnabled("tenant-a", "doc.write")
	if knownA {
		t.Error("tenant-a should not see tenant-b's doc.write")
	}
	// tenant-b must not see tenant-a's web.fetch.
	_, knownB := reg.IsEnabled("tenant-b", "web.fetch")
	if knownB {
		t.Error("tenant-b should not see tenant-a's web.fetch")
	}
}

// ── EffectClassForTenant ─────────────────────────────────────────────────────

func TestEffectClassForTenant_OverlayClass(t *testing.T) {
	reg := NewRegistry()
	reg.SetTenantOverlay("tenant-a", []OverlayRow{
		// Store write_external on a normally read_only tool.
		{ToolID: "web.fetch", Enabled: true, EffectClass: planv1.EffectClass_WRITE_EXTERNAL, Scope: "web:read", ExecutorKind: "builtin"},
	})
	got, ok := reg.EffectClassForTenant("tenant-a", "web.fetch")
	if !ok {
		t.Error("EffectClassForTenant: ok=false, want true")
	}
	if got != planv1.EffectClass_WRITE_EXTERNAL {
		t.Errorf("EffectClassForTenant = %v, want WRITE_EXTERNAL", got)
	}
}

func TestEffectClassForTenant_DelegatesToGlobal(t *testing.T) {
	reg := NewRegistry()
	// No overlay set — should delegate to AuthoritativeEffectClass.
	got, ok := reg.EffectClassForTenant("tenant-a", "slack.post")
	if !ok {
		t.Error("EffectClassForTenant (no overlay): ok=false, want true (global fallback)")
	}
	if got != planv1.EffectClass_WRITE_EXTERNAL {
		t.Errorf("EffectClassForTenant (no overlay) = %v, want WRITE_EXTERNAL", got)
	}
}

func TestEffectClassForTenant_OverlayBeforeGlobal(t *testing.T) {
	reg := newBuiltinTestRegistry()
	// Before overlay.
	before, _ := reg.EffectClassForTenant("tenant-a", "web.fetch")

	reg.SetTenantOverlay("tenant-a", []OverlayRow{
		{ToolID: "web.fetch", Enabled: true, EffectClass: planv1.EffectClass_DESTRUCTIVE, Scope: "web:read", ExecutorKind: "builtin"},
	})
	after, ok := reg.EffectClassForTenant("tenant-a", "web.fetch")
	if !ok {
		t.Error("EffectClassForTenant after overlay: ok=false")
	}
	// After must differ from the global baseline (read_only → destructive).
	if after == before {
		t.Errorf("EffectClassForTenant should return overlay class %v, still returning global %v", after, before)
	}
	if after != planv1.EffectClass_DESTRUCTIVE {
		t.Errorf("EffectClassForTenant = %v, want DESTRUCTIVE", after)
	}
}

func TestEffectClassForTenant_UnknownMCPNoOverlay_FalseGlobal(t *testing.T) {
	reg := NewRegistry()
	// MCP tools have no global authoritative class and no overlay — ok=false.
	_, ok := reg.EffectClassForTenant("tenant-a", "mcp:conn-abc:some-tool")
	if ok {
		t.Error("EffectClassForTenant(mcp, no overlay): ok=true, want false")
	}
}
