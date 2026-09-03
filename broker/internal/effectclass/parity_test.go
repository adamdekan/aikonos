package effectclass_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/adamdekan/aikonos/broker/internal/effectclass"
)

// TestOPARoutingParity regenerates the effectclass routing block in-memory and
// byte-compares it against the committed region in tool_invocation.rego.
// Fails if the registry changed without running `go generate ./broker/internal/effectclass/...`.
func TestOPARoutingParity(t *testing.T) {
	// Locate the rego file relative to this source file's directory so the test
	// is independent of the caller's working directory.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	regoPath := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "policies", "opa", "tool_invocation.rego")

	raw, err := os.ReadFile(regoPath)
	if err != nil {
		t.Fatalf("read %s: %v", regoPath, err)
	}
	content := string(raw)

	const (
		beginMarker = "# BEGIN generated effectclass routing — do not edit by hand (go generate ./broker/internal/effectclass/...)"
		endMarker   = "# END generated effectclass routing"
	)

	bi := strings.Index(content, beginMarker)
	if bi < 0 {
		t.Fatalf("BEGIN marker not found in %s", regoPath)
	}
	after := bi + len(beginMarker)
	if after < len(content) && content[after] == '\n' {
		after++
	}

	ei := strings.Index(content, endMarker)
	if ei < 0 {
		t.Fatalf("END marker not found in %s", regoPath)
	}

	committed := content[after:ei]
	want := effectclass.RenderRoutingBlock()

	if committed != want {
		t.Errorf("tool_invocation.rego routing region is out of date.\nRun: go generate ./broker/internal/effectclass/...\n\ngot (committed):\n%s\nwant (from registry):\n%s", committed, want)
	}
}
