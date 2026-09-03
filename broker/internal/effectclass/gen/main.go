// Command gen splices the generated effectclass routing sets into
// policies/opa/tool_invocation.rego between the BEGIN/END marker comments, then
// runs opa fmt -w to keep the file canonically formatted.
//
// Run via: go generate ./broker/internal/effectclass/...
package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/adamdekan/aikonos/broker/internal/effectclass"
)

const (
	beginMarker = "# BEGIN generated effectclass routing — do not edit by hand (go generate ./broker/internal/effectclass/...)"
	endMarker   = "# END generated effectclass routing"
)

func main() {
	// Default path relative to the package dir where go generate runs
	// (broker/internal/effectclass → policies/opa/tool_invocation.rego).
	regoPath := filepath.Join("..", "..", "..", "policies", "opa", "tool_invocation.rego")
	if len(os.Args) > 1 {
		regoPath = os.Args[1]
	}

	abs, err := filepath.Abs(regoPath)
	if err != nil {
		log.Fatalf("gen: resolve path: %v", err)
	}

	src, err := os.ReadFile(abs)
	if err != nil {
		log.Fatalf("gen: read %s: %v", abs, err)
	}

	updated, err := spliceBlock(string(src), effectclass.RenderRoutingBlock())
	if err != nil {
		log.Fatalf("gen: splice: %v", err)
	}

	if err := os.WriteFile(abs, []byte(updated), 0o644); err != nil {
		log.Fatalf("gen: write %s: %v", abs, err)
	}

	cmd := exec.Command("opa", "fmt", "-w", abs)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("gen: opa fmt: %v", err)
	}

	fmt.Fprintf(os.Stderr, "gen: wrote %s\n", abs)
}

// spliceBlock replaces everything between the BEGIN and END marker lines with
// block.  Returns an error (instead of silently appending) if either marker is
// absent — the markers must be added to the file before the generator is run.
func spliceBlock(src, block string) (string, error) {
	bi := strings.Index(src, beginMarker)
	if bi < 0 {
		return "", fmt.Errorf("BEGIN marker not found; add it to tool_invocation.rego before running go generate")
	}
	after := bi + len(beginMarker)
	if after < len(src) && src[after] == '\n' {
		after++
	}

	ei := strings.Index(src, endMarker)
	if ei < 0 {
		return "", fmt.Errorf("END marker not found; add it to tool_invocation.rego before running go generate")
	}

	return src[:after] + block + src[ei:], nil
}
