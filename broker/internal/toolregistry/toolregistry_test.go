package toolregistry

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadManifests_Parity(t *testing.T) {
	if err := LoadManifests("../../../skills"); err != nil {
		t.Fatalf("LoadManifests(skills): %v", err)
	}
	cases := []struct {
		tool      string
		wantScope string
		wantEC    string
	}{
		{"web.fetch", "web:read", "read_only"},
		{"doc.write", "doc:write", "write_local"},
		{"email.draft", "email:write", "write_external"},
	}
	for _, tc := range cases {
		scope, ok := RequiredScope(tc.tool)
		if !ok || scope != tc.wantScope {
			t.Errorf("RequiredScope(%q) = (%q,%v), want (%q,true)", tc.tool, scope, ok, tc.wantScope)
		}
		ec, ecOK := EffectClass(tc.tool)
		if !ecOK || ec != tc.wantEC {
			t.Errorf("EffectClass(%q) = (%q,%v), want (%q,true)", tc.tool, ec, ecOK, tc.wantEC)
		}
	}
}

func TestLoadManifests_Conflict(t *testing.T) {
	// siem.query has no toolproxy plugin, so it
	// stays in staticBaseline unconditionally — a stable fixture for this
	// hardcoded-baseline conflict check, unlike a plugin-backed id such as web.fetch
	// whose baseline entry now only exists once LoadFromPlugins runs (main.go).
	dir := t.TempDir()
	toolDir := filepath.Join(dir, "siem.query")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `apiVersion: aikonos.com/v1
kind: Skill
metadata:
  name: siem.query
spec:
  capability_scope: admin:write
  effect_class: read_only
`
	if err := os.WriteFile(filepath.Join(toolDir, "skill.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	// Clear the overlay to a known-empty state. Any prior test that called LoadManifests(skills)
	// would have populated the overlay with siem.query; leaving it populated would make the
	// EffectClass assertion below a no-op.
	if err := LoadManifests(t.TempDir()); err != nil {
		t.Fatalf("setup clear overlay: %v", err)
	}

	err := LoadManifests(dir)
	if err == nil {
		t.Fatal("expected error for scope conflict, got nil")
	}

	// Baseline scope must be intact (comes from the hardcoded registry, not the overlay).
	scope, ok := RequiredScope("siem.query")
	if !ok || scope != "siem:read" {
		t.Errorf("after conflict error, RequiredScope(siem.query) = (%q,%v), want (siem:read,true)", scope, ok)
	}

	// why: LoadManifests returns before swapping the overlay on conflict; if that guard
	// regressed to a partial swap, the overlay entry for siem.query would appear here.
	_, ecOK := EffectClass("siem.query")
	if ecOK {
		t.Error("after conflict error, EffectClass(siem.query) must be absent (overlay must not have been swapped)")
	}
}

func TestLoadManifests_NewTool(t *testing.T) {
	dir := t.TempDir()
	toolDir := filepath.Join(dir, "dropbox.read")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `apiVersion: aikonos.com/v1
kind: Skill
metadata:
  name: dropbox.read
spec:
  capability_scope: dropbox:read
  effect_class: read_only
`
	if err := os.WriteFile(filepath.Join(toolDir, "skill.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := LoadManifests(dir); err != nil {
		t.Fatalf("LoadManifests: %v", err)
	}

	scope, ok := RequiredScope("dropbox.read")
	if !ok || scope != "dropbox:read" {
		t.Errorf("RequiredScope(dropbox.read) = (%q,%v), want (dropbox:read,true)", scope, ok)
	}
	ec, ecOK := EffectClass("dropbox.read")
	if !ecOK || ec != "read_only" {
		t.Errorf("EffectClass(dropbox.read) = (%q,%v), want (read_only,true)", ec, ecOK)
	}

	// Cleanup: reset overlay so subsequent tests are unaffected.
	_ = LoadManifests("../../../skills")
}

func TestLoadManifests_DenyByDefault(t *testing.T) {
	if err := LoadManifests("../../../skills"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	_, ok := RequiredScope("totally.unknown.tool")
	if ok {
		t.Error("unknown tool must return ok=false after LoadManifests")
	}
}

func TestLoadManifests_MCPPrefixAfterLoad(t *testing.T) {
	if err := LoadManifests("../../../skills"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	scope, ok := RequiredScope("mcp:conn-abc:my-tool")
	if !ok || scope != "mcp:conn-abc" {
		t.Errorf("RequiredScope(mcp:conn-abc:my-tool) = (%q,%v), want (mcp:conn-abc,true)", scope, ok)
	}
}

func TestRequiredScope(t *testing.T) {
	cases := []struct {
		tool      string
		wantScope string
		wantOK    bool
	}{
		{"siem.query", "siem:read", true},
		{"slack.post", "slack:write", true},
		{"doc.write", "doc:write", true},
		{"totally.unregistered", "", false},
		{"", "", false},
		// MCP dynamic prefix rule.
		{"mcp:conn-abc:my-tool", "mcp:conn-abc", true},
		{"mcp:uuid-123:search", "mcp:uuid-123", true},
		// Malformed mcp: ids must not match.
		{"mcp:", "", false},
		{"mcp:only-connector", "", false},
		{"mcp:conn:", "", false},
	}
	for _, tc := range cases {
		got, ok := RequiredScope(tc.tool)
		if got != tc.wantScope || ok != tc.wantOK {
			t.Errorf("RequiredScope(%q) = (%q,%v), want (%q,%v)", tc.tool, got, ok, tc.wantScope, tc.wantOK)
		}
	}
}

func TestParseMcpToolID(t *testing.T) {
	cases := []struct {
		toolID   string
		wantConn string
		wantTool string
		wantOK   bool
	}{
		// Happy path: first colon after mcp: separates connector from tool.
		{"mcp:conn-abc:my-tool", "conn-abc", "my-tool", true},
		{"mcp:uuid-123:search", "uuid-123", "search", true},
		// Tool name may itself contain colons (connector ID is always before the first split).
		{"mcp:conn-abc:a:b", "conn-abc", "a:b", true},
		// Non-mcp prefix: no match.
		{"doc.read", "", "", false},
		// mcp: with nothing after.
		{"mcp:", "", "", false},
		// mcp: with connector but no tool separator.
		{"mcp:no-tool-part", "", "", false},
		// mcp: with connector and empty tool.
		{"mcp:c:", "", "", false},
	}
	for _, tc := range cases {
		conn, tool, ok := ParseMcpToolID(tc.toolID)
		if ok != tc.wantOK {
			t.Errorf("parseMcpToolID(%q).ok = %v, want %v", tc.toolID, ok, tc.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if conn != tc.wantConn || tool != tc.wantTool {
			t.Errorf("parseMcpToolID(%q) = (%q,%q), want (%q,%q)", tc.toolID, conn, tool, tc.wantConn, tc.wantTool)
		}
	}
}
