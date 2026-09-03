package scaffold_test

import (
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adamdekan/aikonos/broker/cmd/aikonos/scaffold"
	"github.com/adamdekan/aikonos/broker/internal/skillmanifest"
)

// --- sanitizeName ---

func TestSanitizeName_valid(t *testing.T) {
	cases := []string{"my.tool-1", "web.fetch", "a", "tool_name", "0abc"}
	for _, c := range cases {
		if _, err := scaffold.SanitizeName(c); err != nil {
			t.Errorf("SanitizeName(%q) unexpected error: %v", c, err)
		}
	}
}

func TestSanitizeName_rejectsEmpty(t *testing.T) {
	if _, err := scaffold.SanitizeName(""); err == nil {
		t.Error("expected error for empty name")
	}
}

func TestSanitizeName_rejectsPathTraversal(t *testing.T) {
	cases := []string{"../x", "a/../b", "a/b"}
	for _, c := range cases {
		if _, err := scaffold.SanitizeName(c); err == nil {
			t.Errorf("SanitizeName(%q): expected error for path-traversal/separator", c)
		}
	}
}

func TestSanitizeName_rejectsUppercase(t *testing.T) {
	if _, err := scaffold.SanitizeName("Foo"); err == nil {
		t.Error("expected error for uppercase 'Foo'")
	}
}

func TestSanitizeName_rejectsLeadingDot(t *testing.T) {
	if _, err := scaffold.SanitizeName(".hidden"); err == nil {
		t.Error("expected error for leading-dot name")
	}
}

func TestSanitizeName_rejectsDoubleDot(t *testing.T) {
	if _, err := scaffold.SanitizeName(".."); err == nil {
		t.Error("expected error for '..'")
	}
}

// --- NewTool ---

func TestNewTool_createsValidManifest(t *testing.T) {
	root := t.TempDir()
	id := "my.tool-1"

	path, err := scaffold.NewTool(root, id)
	if err != nil {
		t.Fatalf("NewTool: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not created at %s: %v", path, err)
	}

	// Closed-loop: must round-trip through skillmanifest.Load.
	manifests, err := skillmanifest.Load(filepath.Join(root, "skills"))
	if err != nil {
		t.Fatalf("skillmanifest.Load: %v", err)
	}
	if len(manifests) != 1 {
		t.Fatalf("expected 1 manifest, got %d", len(manifests))
	}
	m := manifests[0]
	if m.ToolID() != id {
		t.Errorf("ToolID = %q, want %q", m.ToolID(), id)
	}
	if m.Scope() != id+":read" {
		t.Errorf("Scope = %q, want %q", m.Scope(), id+":read")
	}
	if m.EffectClass() != "read_only" {
		t.Errorf("EffectClass = %q, want read_only", m.EffectClass())
	}
}

func TestNewTool_noClobber(t *testing.T) {
	root := t.TempDir()
	id := "my-tool"

	path, err := scaffold.NewTool(root, id)
	if err != nil {
		t.Fatalf("first NewTool: %v", err)
	}

	// Record original content.
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read first file: %v", err)
	}

	// Second call must error.
	if _, err := scaffold.NewTool(root, id); err == nil {
		t.Fatal("expected error on second NewTool (no-clobber), got nil")
	}

	// File must be unchanged.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read file: %v", err)
	}
	if string(original) != string(after) {
		t.Error("file was modified by second NewTool call")
	}
}

// --- NewPolicy ---

func TestNewPolicy_createsFile(t *testing.T) {
	root := t.TempDir()
	name := "my_gate"

	path, err := scaffold.NewPolicy(root, name)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("policy file not created at %s: %v", path, err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read policy file: %v", err)
	}
	body := string(content)

	if !strings.Contains(body, "package aikonos."+name) {
		t.Errorf("missing 'package aikonos.%s' in policy file", name)
	}
	if !strings.Contains(body, "default allow := false") {
		t.Error("missing 'default allow := false'")
	}
	if !strings.Contains(body, "default require_approval := false") {
		t.Error("missing 'default require_approval := false'")
	}
	if !strings.Contains(body, "default require_step_up := false") {
		t.Error("missing 'default require_step_up := false'")
	}
}

func TestNewPolicy_noClobber(t *testing.T) {
	root := t.TempDir()
	name := "my_gate"

	if _, err := scaffold.NewPolicy(root, name); err != nil {
		t.Fatalf("first NewPolicy: %v", err)
	}
	if _, err := scaffold.NewPolicy(root, name); err == nil {
		t.Fatal("expected error on second NewPolicy (no-clobber), got nil")
	}
}

// --- NewConnector ---

func TestNewConnector_createsValidStub(t *testing.T) {
	root := t.TempDir()
	name := "dropbox"

	path, err := scaffold.NewConnector(root, name)
	if err != nil {
		t.Fatalf("NewConnector: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not created at %s: %v", path, err)
	}

	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}

	// Must parse as valid Go.
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, path, src, 0); err != nil {
		t.Fatalf("generated source does not parse: %v", err)
	}

	// Must contain all 7 Plugin method names.
	body := string(src)
	for _, method := range []string{
		"Provider", "ConnectorID", "DisplayName", "DefaultScopes",
		"Endpoint", "AuthCodeOptions", "RevokeURL",
	} {
		if !strings.Contains(body, method) {
			t.Errorf("generated source missing method %q", method)
		}
	}

	// Must contain an init func with a Register call inside its body (AST-verified).
	fset2 := token.NewFileSet()
	astFile, err := parser.ParseFile(fset2, path, src, 0)
	if err != nil {
		t.Fatalf("re-parse for AST walk: %v", err)
	}
	var initHasRegister bool
	ast.Inspect(astFile, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "init" || fn.Body == nil {
			return true
		}
		// Walk init's body for a CallExpr whose function is "Register".
		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			call, ok := inner.(*ast.CallExpr)
			if !ok {
				return true
			}
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "Register" {
				initHasRegister = true
			}
			return true
		})
		return false // found init; no need to descend further at top level
	})
	if !initHasRegister {
		t.Error("generated source missing Register( call inside init func body")
	}

	// Must be gofmt-canonical (round-trip unchanged).
	formatted, err := format.Source(src)
	if err != nil {
		t.Fatalf("format.Source failed on generated source: %v", err)
	}
	if string(formatted) != string(src) {
		t.Errorf("generated source is not gofmt-canonical:\ngot:\n%s\nwant:\n%s", src, formatted)
	}
}

func TestNewConnector_noClobber(t *testing.T) {
	root := t.TempDir()
	name := "dropbox"

	if _, err := scaffold.NewConnector(root, name); err != nil {
		t.Fatalf("first NewConnector: %v", err)
	}
	if _, err := scaffold.NewConnector(root, name); err == nil {
		t.Fatal("expected error on second NewConnector (no-clobber), got nil")
	}
}

// --- NewTool (extended: also emits main.py) ---

func TestNewTool_emitsMainPy(t *testing.T) {
	root := t.TempDir()
	id := "my.tool-1"

	if _, err := scaffold.NewTool(root, id); err != nil {
		t.Fatalf("NewTool: %v", err)
	}

	pyPath := filepath.Join(root, "skills", id, "main.py")
	if _, err := os.Stat(pyPath); err != nil {
		t.Fatalf("main.py not created at %s: %v", pyPath, err)
	}

	content, err := os.ReadFile(pyPath)
	if err != nil {
		t.Fatalf("read main.py: %v", err)
	}
	body := string(content)

	// Must have the stdin/stdout JSON protocol entry point.
	if !strings.Contains(body, "def main") {
		t.Error("main.py missing 'def main'")
	}
	if !strings.Contains(body, "json.load(sys.stdin)") {
		t.Error("main.py missing json.load(sys.stdin) entry point")
	}
	// Must reference the tool name.
	if !strings.Contains(body, id) {
		t.Errorf("main.py missing tool id %q", id)
	}
}

// --- NewMigration ---

func TestNewMigration_picksMaxPlusOne(t *testing.T) {
	root := t.TempDir()

	// Seed two migrations so next = 003.
	migrDir := filepath.Join(root, "broker", "internal", "db", "migrations")
	if err := os.MkdirAll(migrDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, f := range []string{"001_initial.sql", "002_step_args.sql"} {
		if err := os.WriteFile(filepath.Join(migrDir, f), []byte("-- seed"), 0o644); err != nil {
			t.Fatalf("seed migration %s: %v", f, err)
		}
	}

	path, err := scaffold.NewMigration("my_table", root)
	if err != nil {
		t.Fatalf("NewMigration: %v", err)
	}

	base := filepath.Base(path)
	if !strings.HasPrefix(base, "003_") {
		t.Errorf("expected filename to start with '003_', got %q", base)
	}
	if base != "003_my_table.sql" {
		t.Errorf("expected '003_my_table.sql', got %q", base)
	}
}

func TestNewMigration_emptyDirStartsAt001(t *testing.T) {
	root := t.TempDir()

	path, err := scaffold.NewMigration("initial", root)
	if err != nil {
		t.Fatalf("NewMigration: %v", err)
	}

	base := filepath.Base(path)
	if base != "001_initial.sql" {
		t.Errorf("expected '001_initial.sql', got %q", base)
	}
}

func TestNewMigration_contentHasRLS(t *testing.T) {
	root := t.TempDir()

	path, err := scaffold.NewMigration("my_table", root)
	if err != nil {
		t.Fatalf("NewMigration: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	body := string(content)

	if !strings.Contains(body, "ENABLE ROW LEVEL SECURITY") {
		t.Error("migration missing ENABLE ROW LEVEL SECURITY")
	}
	if !strings.Contains(body, "current_setting('app.current_tenant', true)") {
		t.Error("migration missing tenant_isolation policy using current_setting")
	}
	if !strings.Contains(body, "my_table") {
		t.Error("migration missing table name 'my_table'")
	}
}

func TestNewMigration_noClobber(t *testing.T) {
	// NewMigration advances the number on each call so two sequential calls
	// with the same name produce *different* paths — no silent overwrite.
	// Verify: first call → 001_check.sql, second → 002_check.sql, paths differ.
	root := t.TempDir()

	first, err := scaffold.NewMigration("check", root)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	second, err := scaffold.NewMigration("check", root)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if first == second {
		t.Errorf("two sequential NewMigration calls produced the same path %q — would silently overwrite", first)
	}
	if filepath.Base(first) != "001_check.sql" {
		t.Errorf("expected first file 001_check.sql, got %q", filepath.Base(first))
	}
	if filepath.Base(second) != "002_check.sql" {
		t.Errorf("expected second file 002_check.sql, got %q", filepath.Base(second))
	}
}

// --- NewAdminView ---

func TestNewAdminView_createsVueFile(t *testing.T) {
	root := t.TempDir()
	name := "my-view"

	path, err := scaffold.NewAdminView(name, root)
	if err != nil {
		t.Fatalf("NewAdminView: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("vue file not created at %s: %v", path, err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read vue file: %v", err)
	}
	body := string(content)

	// Must import the three core ui components.
	for _, imp := range []string{"DataTable", "EmptyState", "useToast"} {
		if !strings.Contains(body, imp) {
			t.Errorf("generated .vue missing import %q", imp)
		}
	}
	// Must have a <template> with a view div.
	if !strings.Contains(body, "<template>") {
		t.Error("generated .vue missing <template>")
	}
	if !strings.Contains(body, `class="view"`) {
		t.Error("generated .vue missing view class div")
	}
}

func TestNewAdminView_titleCasedFileName(t *testing.T) {
	root := t.TempDir()

	path, err := scaffold.NewAdminView("my-view", root)
	if err != nil {
		t.Fatalf("NewAdminView: %v", err)
	}

	base := filepath.Base(path)
	if base != "MyView.vue" {
		t.Errorf("expected 'MyView.vue', got %q", base)
	}
}

func TestNewAdminView_noClobber(t *testing.T) {
	root := t.TempDir()

	if _, err := scaffold.NewAdminView("dup", root); err != nil {
		t.Fatalf("first NewAdminView: %v", err)
	}
	if _, err := scaffold.NewAdminView("dup", root); err == nil {
		t.Fatal("expected error on second NewAdminView (no-clobber), got nil")
	}
}
