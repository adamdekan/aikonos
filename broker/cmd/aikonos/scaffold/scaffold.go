// Package scaffold generates starter artifacts for Aikonos extension seams.
// Generation-only: writes files; no runtime registration or side effects.
package scaffold

import (
	"errors"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

// SanitizeName validates and normalises an artifact name: must be non-empty,
// all-lowercase, composed only of [a-z0-9._-], no leading dot, no ".." component,
// and no path separators (/ or \).
func SanitizeName(s string) (string, error) {
	if s == "" {
		return "", errors.New("scaffold: name must not be empty")
	}
	if strings.ContainsAny(s, "/\\") {
		return "", fmt.Errorf("scaffold: name %q must not contain path separators", s)
	}
	if s == ".." || strings.HasPrefix(s, "../") || strings.Contains(s, "/../") {
		return "", fmt.Errorf("scaffold: name %q must not contain path traversal", s)
	}
	if strings.HasPrefix(s, ".") {
		return "", fmt.Errorf("scaffold: name %q must not start with a dot", s)
	}
	for _, r := range s {
		if !isAllowed(r) {
			return "", fmt.Errorf("scaffold: name %q contains invalid character %q (allowed: [a-z0-9._-])", s, r)
		}
	}
	return s, nil
}

func isAllowed(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= '0' && r <= '9') ||
		r == '.' || r == '_' || r == '-'
}

// writeNoClobber creates parent directories then writes content to path.
// Returns an error if path already exists (never overwrites).
func writeNoClobber(path, content string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("scaffold: %s already exists (no-clobber)", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("scaffold: mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("scaffold: write %s: %w", path, err)
	}
	return nil
}

// NewTool generates skills/<id>/skill.yaml and skills/<id>/main.py under root.
// The manifest is valid per skillmanifest.Load.
// Returns the path of the skill.yaml written.
func NewTool(root, id string) (string, error) {
	name, err := SanitizeName(id)
	if err != nil {
		return "", err
	}

	yamlPath := filepath.Join(root, "skills", name, "skill.yaml")

	yamlContent := fmt.Sprintf(`apiVersion: aikonos.com/v1
kind: Skill
metadata:
  name: %s
  version: 0.1.0
  description: "Generated skill scaffold for %s."
spec:
  capability_scope: "%s:read"
  effect_class: read_only
  tools:
    - name: %s_run
      description: "Sample tool for %s."
      effect_class: read_only
      input_schema:
        type: object
        properties:
          input:
            type: string
            description: "Input value."
        required: []
        additionalProperties: false
`, name, name, name, strings.ReplaceAll(name, ".", "_"), name)

	if err := writeNoClobber(yamlPath, yamlContent); err != nil {
		return "", err
	}

	pyPath := filepath.Join(root, "skills", name, "main.py")
	toolFn := strings.ReplaceAll(name, ".", "_")
	toolFn = strings.ReplaceAll(toolFn, "-", "_")

	pyContent := fmt.Sprintf(`"""
skills/%s/main.py
Aikonos skill: %s

Generated scaffold. Implement %s_run and add it to the dispatch table below.

Entry protocol (stdin/stdout JSON):
  Input:  {"tool": "<tool_name>", "args": {...}}
  Output: {"result": {...}} | {"error": "..."}
"""
import json
import sys
from typing import Any


def %s_run(input: str = "") -> dict[str, Any]:
    """TODO: implement %s_run."""
    # Replace with real implementation.
    return {"output": input}


# ── Skill entry point ─────────────────────────────────────────────────────────

def main() -> None:
    try:
        request = json.load(sys.stdin)
    except json.JSONDecodeError as e:
        print(json.dumps({"error": f"Invalid JSON input: {e}"}))
        sys.exit(1)

    tool = request.get("tool")
    args = request.get("args", {})

    try:
        if tool == "%s_run":
            result = %s_run(**args)
            print(json.dumps({"result": result}))
        else:
            print(json.dumps({"error": f"Unknown tool: {tool!r}"}))
            sys.exit(1)
    except (ValueError, RuntimeError) as e:
        print(json.dumps({"error": str(e)}))
        sys.exit(1)


if __name__ == "__main__":
    main()
`, name, name, toolFn, toolFn, toolFn, toolFn, toolFn)

	if err := writeNoClobber(pyPath, pyContent); err != nil {
		return "", err
	}

	return yamlPath, nil
}

// titleCase converts a sanitized name like "my-connector" to "MyConnector"
// by uppercasing the first rune and every rune that follows a non-alphanumeric.
func titleCase(s string) string {
	var b strings.Builder
	upper := true
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			upper = true
			continue
		}
		if upper {
			b.WriteRune(unicode.ToUpper(r))
			upper = false
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// NewPolicy generates policies/opa/<name>.rego under root.
// The file contains the pluggable gate skeleton per #13.
// Returns the path written.
func NewPolicy(root, name string) (string, error) {
	safe, err := SanitizeName(name)
	if err != nil {
		return "", err
	}

	path := filepath.Join(root, "policies", "opa", safe+".rego")

	content := fmt.Sprintf(`# Pluggable OPA gate for %s — generated by aikonos new policy.
# Register with: RegisterToolGate("%s", "aikonos/%s")
# See broker/internal/policy/engine.go for the gate contract.
package aikonos.%s

default allow := false

default require_approval := false

default require_step_up := false

# deny_reasons collects human-readable explanations when allow is false.
# Uncomment and extend to populate structured denial messages:
#
# deny_reasons[msg] {
#     not input.subject.tenant_id
#     msg := "missing tenant_id"
# }
`, safe, safe, safe, safe)

	if err := writeNoClobber(path, content); err != nil {
		return "", err
	}
	return path, nil
}

// NewMigration generates the next-numbered migration SQL file under
// broker/internal/db/migrations/ within root. The number is computed from the
// highest existing NNN prefix in that directory plus one, zero-padded to 3
// digits. An empty or absent migrations directory starts at 001.
// Returns the path written.
func NewMigration(name, root string) (string, error) {
	safe, err := SanitizeName(name)
	if err != nil {
		return "", err
	}

	migrDir := filepath.Join(root, "broker", "internal", "db", "migrations")

	next, err := nextMigrationNumber(migrDir)
	if err != nil {
		return "", err
	}

	filename := fmt.Sprintf("%03d_%s.sql", next, safe)
	path := filepath.Join(migrDir, filename)

	// Use the sanitized name as the table name placeholder so the template
	// refers to a plausible table by default.
	content := fmt.Sprintf(`-- %s
-- TODO: describe what this migration adds.
--
-- RLS is mandatory for every new table so tenant isolation is enforced at the
-- DB layer. Replace <table> with the actual table name and add column
-- definitions inside the CREATE TABLE body.

CREATE TABLE IF NOT EXISTS %s (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  UUID        NOT NULL,
    -- TODO: add columns here
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- TODO: add indexes as needed.

ALTER TABLE %s ENABLE ROW LEVEL SECURITY;
CREATE POLICY %s_tenant_isolation ON %s
    USING (tenant_id::text = current_setting('app.current_tenant', true));

INSERT INTO schema_migrations (version, description)
VALUES ('%03d', '%s')
ON CONFLICT (version) DO NOTHING;
`, filename, safe, safe, safe, safe, next, safe)

	if err := writeNoClobber(path, content); err != nil {
		return "", err
	}
	return path, nil
}

// nextMigrationNumber scans dir for files whose names begin with NNN_ and
// returns max(NNN)+1. Returns 1 when the dir is absent or contains no numbered
// files.
func nextMigrationNumber(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 1, nil
		}
		return 0, fmt.Errorf("scaffold: read migrations dir %s: %w", dir, err)
	}

	max := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n, ok := parseMigrationNumber(e.Name())
		if ok && n > max {
			max = n
		}
	}
	return max + 1, nil
}

// parseMigrationNumber extracts the leading integer from a filename like
// "007_foo.sql". Returns (n, true) on success.
func parseMigrationNumber(name string) (int, bool) {
	idx := strings.IndexByte(name, '_')
	if idx < 1 {
		return 0, false
	}
	n, err := strconv.Atoi(name[:idx])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// NewAdminView generates webui/web/src/views/admin/<Name>.vue under root.
// The skeleton mirrors the Network.vue structure (DataTable + EmptyState +
// useToast) and is ready to wire into router.js + Sidebar.vue.
// Returns the path written and prints next-steps wiring instructions to stdout.
func NewAdminView(name, root string) (string, error) {
	safe, err := SanitizeName(name)
	if err != nil {
		return "", err
	}

	title := titleCase(safe)
	path := filepath.Join(root, "webui", "web", "src", "views", "admin", title+".vue")

	// Derive a kebab-case route name and a human label from the sanitized name.
	label := strings.ReplaceAll(safe, "-", " ")
	label = strings.ReplaceAll(label, "_", " ")
	label = strings.ToUpper(label[:1]) + label[1:]

	content := fmt.Sprintf(`<script setup>
import { ref, onMounted } from "vue";
import DataTable from "../../components/ui/DataTable.vue";
import EmptyState from "../../components/ui/EmptyState.vue";
import { useToast } from "../../components/ui/useToast.js";

const { push: toast } = useToast();

const rows      = ref([]);
const forbidden = ref(false);
const loading   = ref(true);
const error     = ref("");

// TODO: replace with real API calls from ../../api/admin.js
async function load() {
  loading.value   = true;
  error.value     = "";
  forbidden.value = false;
  try {
    // const data = await list%s();
    // if (data.forbidden) { forbidden.value = true; return; }
    // rows.value = data.items ?? [];
  } catch (e) {
    error.value = e.message;
  } finally {
    loading.value = false;
  }
}

onMounted(load);

// TODO: define columns matching the data shape.
const TABLE_COLS = [
  { key: "id",         label: "ID" },
  { key: "created_at", label: "Created" },
];
</script>

<template>
  <div class="view">
    <div class="view-header">
      <h1>%s</h1>
    </div>

    <EmptyState
      v-if="forbidden"
      data-testid="forbidden"
      message="You are not a tenant admin."
    />

    <template v-else>
      <div v-if="error" data-testid="error-banner" class="banner-err">{{ error }}</div>

      <DataTable
        :columns="TABLE_COLS"
        :rows="rows"
        :loading="loading"
        empty-text="No %s records found."
      >
        <template #row="{ row }">
          <td>{{ row.id }}</td>
          <td>{{ row.created_at }}</td>
        </template>
      </DataTable>
    </template>
  </div>
</template>

<style scoped>
.view { padding: 24px 32px; max-width: 900px; }
.view-header { display: flex; align-items: center; gap: 10px; margin-bottom: 24px; }
.view-header h1 { margin: 0; font-size: 20px; font-weight: 600; }

.banner-err {
  background: var(--fill-danger); border: 1px solid var(--danger);
  border-radius: var(--radius-sm); padding: 10px 14px;
  color: var(--danger); font-size: 13px; margin-bottom: 16px;
}
</style>
`, title, label, safe)

	if err := writeNoClobber(path, content); err != nil {
		return "", err
	}

	// Print wiring instructions rather than auto-editing source files.
	fmt.Printf(`created %s

Next steps — wire into router.js and Sidebar.vue:

1. webui/web/src/router.js — add inside the admin children array:
   { path: "%s", name: "%s", component: () => import("./views/admin/%s.vue") }

2. webui/web/src/components/Sidebar.vue — add to the admin nav items array:
   { name: "%s", label: "%s", route: "/admin/%s" }

`, path, safe, safe, title, safe, label, safe)

	return path, nil
}

// NewConnector generates broker/internal/connector/<name>_plugin.go under root.
// The file is package connector, implements all 7 connector.Plugin methods with
// placeholder bodies and TODO markers, and registers via init().
// The generated source is passed through go/format before writing — if the
// template is broken, format.Source returns an error rather than writing
// uncompilable output.
// Returns the path written.
func NewConnector(root, name string) (string, error) {
	safe, err := SanitizeName(name)
	if err != nil {
		return "", err
	}

	typeName := titleCase(safe) + "Plugin"
	path := filepath.Join(root, "broker", "internal", "connector", safe+"_plugin.go")

	raw := fmt.Sprintf(`package connector

import (
	"golang.org/x/oauth2"
)

// %s implements connector.Plugin for the %s provider.
// TODO: fill in real OAuth endpoints, scopes, and credential wiring.
type %s struct{}

func (%s) Provider() Provider {
	// TODO: replace with a Provider constant (see ProviderGoogleDrive / ProviderOneDrive)
	return Provider("%s")
}

func (%s) ConnectorID() string { return "%s" }

func (%s) DisplayName() string {
	// TODO: replace with the human-readable provider name.
	return "%s"
}

func (%s) DefaultScopes() []string {
	// TODO: list the OAuth scopes required by this connector.
	return nil
}

func (%s) Endpoint(_ EndpointOpts) oauth2.Endpoint {
	// TODO: return the provider's OAuth2 authorization + token endpoints.
	return oauth2.Endpoint{}
}

func (%s) AuthCodeOptions() []oauth2.AuthCodeOption {
	// TODO: add provider-specific AuthCodeOption values (e.g. offline access).
	return nil
}

func (%s) RevokeURL() string {
	// TODO: return the token-revocation URL, or "" if the provider has none.
	return ""
}

func init() {
	Register(%s{})
}
`,
		typeName, safe,
		typeName,
		typeName, safe,
		typeName, safe,
		typeName, safe,
		typeName,
		typeName,
		typeName,
		typeName,
		typeName)

	src, err := format.Source([]byte(raw))
	if err != nil {
		return "", fmt.Errorf("scaffold: connector template produced invalid Go: %w", err)
	}

	if err := writeNoClobber(path, string(src)); err != nil {
		return "", err
	}
	return path, nil
}
