// broker/internal/toolproxy/result_scan.go
//
// Deterministic pattern scan on *.extract tool results. The four office *.extract tools pull text out of
// untrusted external documents straight into model context — the widest
// prompt-injection surface the office-doc-tools feature adds. This is a
// regex-based, evadable scan: defense-in-depth and an audit signal, NOT a
// guarantee. A determined attacker can reformat text (spacing, casing,
// unicode homoglyphs) to slip past a literal pattern match. Annotate-only —
// a match never blocks or alters the extracted content, it only adds
// injection_flags to the result (see office_client.go / office_xlsx.go /
// office_pdf.go extract handlers) and, at the broker InvokeTool layer,
// triggers a aikonos.tool.result_flagged audit event.
//
// Mirrors netacl/eval.go's config-list style: a plain slice of named regexes,
// evaluated in order, compiled once at package init.
package toolproxy

import "regexp"

// injectionPattern is one named regex class in the default scan set.
type injectionPattern struct {
	name string
	re   *regexp.Regexp
}

// defaultInjectionPatterns is the built-in scan set: instruction tags seen in
// prompt-injection payloads, role-override/imperative phrasing, and
// exfil-shaped URLs (known data-exfil relay hosts, or a URL paired with a
// data-carrying query param). Not exhaustive by design — see the package doc
// comment above.
var defaultInjectionPatterns = []injectionPattern{
	{
		name: "instruction_tag",
		re:   regexp.MustCompile(`(?i)<\s*/?\s*system\s*>|\[\s*/?\s*inst\s*\]|<\|im_(start|end)\|>`),
	},
	{
		name: "role_override",
		re:   regexp.MustCompile(`(?i)(ignore|disregard|forget)\s+(all\s+)?(the\s+)?(above|previous|prior)(\s+(instructions?|prompts?|rules?))?|you\s+are\s+now\s+`),
	},
	{
		name: "exfil_url",
		re:   regexp.MustCompile(`(?i)https?://[^\s]*(webhook\.site|requestbin\.com|pipedream\.net|burpcollaborator\.net|ngrok-free\.app)[^\s]*|https?://[^\s]+\?[^\s]*\bdata=`),
	},
}

// ScanForInjection runs the default pattern set against text and returns the
// distinct matched flag names in defaultInjectionPatterns order, or nil if
// none matched. Pure and deterministic — no I/O, safe to call inline in a
// toolproxy handler.
func ScanForInjection(text string) []string {
	var flags []string
	for _, p := range defaultInjectionPatterns {
		if p.re.MatchString(text) {
			flags = append(flags, p.name)
		}
	}
	return flags
}

// annotateInjectionFlags scans text and, on a match, sets out["injection_flags"]
// — as []any, not []string, since the result crosses structpb.NewStruct
// (ResultStruct in toolproxy.go) on its way back through InvokeTool, and
// structpb only accepts []interface{} for list values. Shared by every
// *.extract handler wiring the scan (office_client.go/office_xlsx.go/
// office_pdf.go) so the []any conversion lives in one place.
func annotateInjectionFlags(out map[string]any, text string) {
	flags := ScanForInjection(text)
	if len(flags) == 0 {
		return
	}
	anyFlags := make([]any, len(flags))
	for i, f := range flags {
		anyFlags[i] = f
	}
	out["injection_flags"] = anyFlags
}
