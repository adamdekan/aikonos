package toolregistry

import (
	"os"
	"path/filepath"
	"testing"

	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// TestAuthoritativeEffectClass_BaselineTable verifies every hand-authored
// baseline tool
// returns the correct authoritative effect class.
func TestAuthoritativeEffectClass_BaselineTable(t *testing.T) {
	cases := []struct {
		toolID string
		want   planv1.EffectClass
	}{
		{"siem.query", planv1.EffectClass_READ_ONLY},
		{"siem.search", planv1.EffectClass_READ_ONLY},
		{"slack.post", planv1.EffectClass_WRITE_EXTERNAL},
		{"slack.dm", planv1.EffectClass_WRITE_EXTERNAL},
	}
	for _, tc := range cases {
		got, ok := AuthoritativeEffectClass(tc.toolID)
		if !ok {
			t.Errorf("AuthoritativeEffectClass(%q): ok=false, want true", tc.toolID)
			continue
		}
		if got != tc.want {
			t.Errorf("AuthoritativeEffectClass(%q) = %v, want %v", tc.toolID, got, tc.want)
		}
	}
}

// TestAuthoritativeEffectClass_PluginBackedRequiresLoadFromPlugins is the
// toolregistry-side half of C11's behavior-preservation proof: a plugin-
// backed id (doc.read, docs formerly in staticBaselineEffectClass) resolves
// nothing on a bare NewRegistry() — it is no longer hand-authored — and
// resolves its former baseline value once LoadFromPlugins runs, proving the
// derivation is load-bearing (main.go wires this from
// toolproxy.RegisteredPluginBaselines(); the literal value here is restated
// because toolregistry cannot import toolproxy — see PluginBaseline's doc
// comment).
func TestAuthoritativeEffectClass_PluginBackedRequiresLoadFromPlugins(t *testing.T) {
	reg := NewRegistry()
	if _, ok := reg.AuthoritativeEffectClass("doc.read"); ok {
		t.Fatal("doc.read resolves an effect class before LoadFromPlugins — it must be absent from the static baseline post-C11")
	}
	reg.LoadFromPlugins([]PluginBaseline{
		{ToolID: "doc.read", Scope: "doc:read", EffectClass: planv1.EffectClass_READ_ONLY},
	})
	got, ok := reg.AuthoritativeEffectClass("doc.read")
	if !ok || got != planv1.EffectClass_READ_ONLY {
		t.Fatalf("AuthoritativeEffectClass(doc.read) after LoadFromPlugins = (%v,%v), want (READ_ONLY,true)", got, ok)
	}
}

// TestAuthoritativeEffectClass_MCP returns false (plan-declared fallback).
func TestAuthoritativeEffectClass_MCP(t *testing.T) {
	mcpTools := []string{
		"mcp:conn-abc:some-tool",
		"mcp:uuid-123:search",
	}
	for _, id := range mcpTools {
		_, ok := AuthoritativeEffectClass(id)
		if ok {
			t.Errorf("AuthoritativeEffectClass(%q): ok=true for MCP tool, want false", id)
		}
	}
}

// TestAuthoritativeEffectClass_Unknown returns false for unknown tools.
func TestAuthoritativeEffectClass_Unknown(t *testing.T) {
	_, ok := AuthoritativeEffectClass("totally.unknown.tool")
	if ok {
		t.Errorf("AuthoritativeEffectClass(unknown): ok=true, want false")
	}
}

// TestLoadManifests_EffectClassConflict verifies that a manifest declaring a
// different effect_class for a baseline tool causes LoadManifests to fail closed.
func TestLoadManifests_EffectClassConflict(t *testing.T) {
	dir := t.TempDir()
	toolDir := filepath.Join(dir, "slack.post")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// slack.post is write_external in the baseline; manifest claims read_only.
	manifest := `apiVersion: aikonos.com/v1
kind: Skill
metadata:
  name: slack.post
spec:
  capability_scope: slack:write
  effect_class: read_only
`
	if err := os.WriteFile(filepath.Join(toolDir, "skill.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	// Clear any prior overlay state.
	if err := LoadManifests(t.TempDir()); err != nil {
		t.Fatalf("setup clear overlay: %v", err)
	}

	err := LoadManifests(dir)
	if err == nil {
		t.Fatal("expected error for effect_class conflict on baseline tool, got nil")
	}
}

// TestLoadManifests_EffectClassMatchAllowed verifies that a manifest for a
// baseline tool whose effect_class matches the baseline is accepted.
func TestLoadManifests_EffectClassMatchAllowed(t *testing.T) {
	dir := t.TempDir()
	toolDir := filepath.Join(dir, "doc.write")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// doc.write is write_local; manifest agrees.
	manifest := `apiVersion: aikonos.com/v1
kind: Skill
metadata:
  name: doc.write
spec:
  capability_scope: doc:write
  effect_class: write_local
`
	if err := os.WriteFile(filepath.Join(toolDir, "skill.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := LoadManifests(dir); err != nil {
		t.Fatalf("LoadManifests with matching effect_class should succeed: %v", err)
	}

	// Cleanup.
	_ = LoadManifests("../../../skills")
}
