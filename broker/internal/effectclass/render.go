package effectclass

import (
	"fmt"
	"sort"
	"strings"
)

// RenderRoutingBlock returns the opa-fmt-canonical text that belongs between
// the BEGIN and END marker lines in tool_invocation.rego.  Each set assignment
// is followed by a blank line; members are sorted alphabetically.
//
// Both the generator (gen/main.go) and the parity test call this function so
// the two sources of truth can never drift from each other.
func RenderRoutingBlock() string {
	var auto, approval, stepup []string
	for _, e := range All() {
		if e.Routing.Auto {
			auto = append(auto, e.Rego)
		}
		if e.Routing.RequireApproval {
			approval = append(approval, e.Rego)
		}
		if e.Routing.RequireStepUp {
			stepup = append(stepup, e.Rego)
		}
	}
	// All() is sorted by enum value; re-sort by Rego string for a deterministic,
	// alphabetical set literal independent of enum numbering.
	sort.Strings(auto)
	sort.Strings(approval)
	sort.Strings(stepup)

	var b strings.Builder
	b.WriteString(renderSet("auto_classes", auto))
	b.WriteString("\n\n")
	b.WriteString(renderSet("approval_classes", approval))
	b.WriteString("\n\n")
	b.WriteString(renderSet("stepup_classes", stepup))
	b.WriteString("\n\n")
	return b.String()
}

func renderSet(name string, members []string) string {
	quoted := make([]string, len(members))
	for i, m := range members {
		quoted[i] = `"` + m + `"`
	}
	return fmt.Sprintf("%s := {%s}", name, strings.Join(quoted, ", "))
}
