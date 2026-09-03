// Package memorybundle is the pure OKF v0.2-subset format layer for agent
// memory: concept-id
// validation, frontmatter parse/compose, derived trust tier and staleness,
// deterministic index rendering, and bounded log appending.
//
// No LLM anywhere, and this file has no I/O — the format is testable without a
// filesystem. Bundle storage (listing, index regeneration, log appending, the
// per-bundle lock) lives beside it in store.go, shared by the toolproxy memory
// plugins and the management RPCs in package broker.
//
// yaml.v3 (not sigs.k8s.io/yaml) is deliberate: OKF requires consumers to
// tolerate unknown frontmatter keys and never reject them, so parsing targets
// map[string]any rather than a typed struct.
package memorybundle

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	// MaxConceptBytes bounds a whole composed concept file (frontmatter + body).
	MaxConceptBytes = 32 * 1024
	// MaxConcepts bounds the concept count of one bundle.
	MaxConcepts = 512
	// MaxLogBytes bounds log.md; AppendLog drops oldest whole entries to stay under it.
	MaxLogBytes = 256 * 1024

	// MaxTitleChars and MaxDescChars bound the two caller-supplied fields
	// RenderIndex echoes verbatim per concept. WHY they are hard limits rather
	// than advice: the write path regenerates the whole index on every write, so
	// an index over workspacefs.MaxFileBytes (10 MiB) can never be written again
	// — and with no delete tool, that bricks every later write to the bundle.
	// MaxConcepts × (MaxTitleChars + MaxDescChars) keeps it three orders of
	// magnitude below that.
	MaxTitleChars = 200
	MaxDescChars  = 500

	// IndexFile and LogFile are server-maintained and never writable as concepts.
	IndexFile = "index.md"
	LogFile   = "log.md"
)

// Trust tiers derived from the frontmatter `verified` list.
const (
	TrustUnverified       = "unverified"
	TrustMachineConfirmed = "machine-confirmed"
	TrustHumanReviewed    = "human-reviewed"
)

const (
	staleAfterLayout = "2006-01-02"
	indexHeader      = "# Memory index\n"
	logHeader        = "# Memory log\n"
	fence            = "---"
	humanPrefix      = "human:"

	// maxLogEntryChars bounds one rendered log entry. Entries are server-composed
	// one-liners, so this only stops a pathological caller-supplied field from
	// crowding the whole rolling log out of MaxLogBytes.
	maxLogEntryChars = 4096
)

// segmentRe is the per-path-segment concept-id rule (max 64 bytes per segment).
var segmentRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ValidateID checks a concept id: 1–2 path segments, each matching
// ^[a-z0-9][a-z0-9_-]{0,63}$, with the reserved index/log names rejected as the
// final segment. The error names the rule that was broken.
func ValidateID(id string) error {
	if id == "" {
		return fmt.Errorf("memorybundle: concept id must not be empty")
	}
	segs := strings.Split(id, "/")
	if len(segs) > 2 {
		return fmt.Errorf("memorybundle: concept id %q must have at most 2 path segments, got %d", id, len(segs))
	}
	for _, s := range segs {
		if !segmentRe.MatchString(s) {
			return fmt.Errorf("memorybundle: concept id segment %q must match %s", s, segmentRe.String())
		}
	}
	switch final := segs[len(segs)-1]; final {
	case strings.TrimSuffix(IndexFile, ".md"), strings.TrimSuffix(LogFile, ".md"):
		return fmt.Errorf("memorybundle: concept id %q uses reserved final segment %q", id, final)
	}
	return nil
}

// Concept is a parsed concept file: the frontmatter mapping (unknown keys kept
// verbatim) and the markdown body.
type Concept struct {
	Frontmatter map[string]any
	Body        string
}

// ParseConcept splits a concept file into frontmatter and body. It errors only
// on an unparseable/non-mapping frontmatter block or a missing/empty `type` —
// every other key, known or not, is passed through untouched (OKF forward
// compatibility).
func ParseConcept(data []byte) (Concept, error) {
	s := string(data)
	if !strings.HasPrefix(s, fence+"\n") {
		return Concept{}, fmt.Errorf("memorybundle: concept must start with a %q frontmatter fence", fence)
	}
	rest := s[len(fence)+1:]
	end := strings.Index(rest, "\n"+fence)
	if end < 0 {
		return Concept{}, fmt.Errorf("memorybundle: concept frontmatter fence is unterminated")
	}
	raw, body := rest[:end], rest[end+len(fence)+1:]
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		body = body[i+1:] // drop the remainder of the closing fence line
	} else {
		body = ""
	}

	fm := map[string]any{}
	if err := yaml.Unmarshal([]byte(raw), &fm); err != nil {
		return Concept{}, fmt.Errorf("memorybundle: parse frontmatter: %w", err)
	}
	if t, _ := fm["type"].(string); strings.TrimSpace(t) == "" {
		return Concept{}, fmt.Errorf("memorybundle: frontmatter field \"type\" is required and must be a non-empty string")
	}
	return Concept{Frontmatter: fm, Body: strings.Trim(body, "\n")}, nil
}

// ConceptSpec carries every caller-supplied field ComposeConcept accepts.
//
// WHY there is no Frontmatter or Verified field: composing exclusively from
// named params is the forgery resistance. `generated` is stamped from
// ComposeConcept's own arguments and `verified` is always emitted empty, so a
// model driving memory.write has no channel through which to claim authorship
// or independent confirmation.
type ConceptSpec struct {
	Type        string
	Title       string
	Description string
	Tags        []string
	// Status is draft|stable|deprecated; empty defaults to stable.
	Status string
	// StaleAfter is an optional absolute YYYY-MM-DD date.
	StaleAfter string
	Sources    []map[string]any
	Body       string
}

// conceptFrontmatter fixes the emitted key order (yaml.v3 sorts map keys, a
// struct keeps the OKF field order from the design doc).
type conceptFrontmatter struct {
	Type        string           `yaml:"type"`
	Title       string           `yaml:"title,omitempty"`
	Description string           `yaml:"description,omitempty"`
	Tags        []string         `yaml:"tags,omitempty"`
	Generated   generatedStamp   `yaml:"generated"`
	Verified    []any            `yaml:"verified"`
	Status      string           `yaml:"status"`
	StaleAfter  string           `yaml:"stale_after,omitempty"`
	Sources     []map[string]any `yaml:"sources,omitempty"`
}

type generatedStamp struct {
	By string    `yaml:"by"`
	At time.Time `yaml:"at"`
}

// ComposeConcept builds the canonical concept file bytes. generatedBy and
// generatedAt are the server-side stamp (`user:<sub>` or `agent:<id>`, now UTC);
// both are required. Fails loud on a missing type/body, an unknown status, a
// malformed stale_after, or a composed file over MaxConceptBytes.
func ComposeConcept(spec ConceptSpec, generatedBy string, generatedAt time.Time) ([]byte, error) {
	if strings.TrimSpace(spec.Type) == "" {
		return nil, fmt.Errorf("memorybundle: type must not be empty")
	}
	body := strings.Trim(spec.Body, "\n")
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("memorybundle: body must not be empty")
	}
	if n := utf8.RuneCountInString(spec.Title); n > MaxTitleChars {
		return nil, fmt.Errorf("memorybundle: title is %d characters, exceeds the %d character cap", n, MaxTitleChars)
	}
	if n := utf8.RuneCountInString(spec.Description); n > MaxDescChars {
		return nil, fmt.Errorf("memorybundle: description is %d characters, exceeds the %d character cap", n, MaxDescChars)
	}
	if generatedBy == "" {
		return nil, fmt.Errorf("memorybundle: generated.by must not be empty")
	}
	status := spec.Status
	if status == "" {
		status = "stable"
	}
	switch status {
	case "draft", "stable", "deprecated":
	default:
		return nil, fmt.Errorf("memorybundle: status must be \"draft\", \"stable\" or \"deprecated\", got %q", status)
	}
	if spec.StaleAfter != "" {
		if _, err := time.Parse(staleAfterLayout, spec.StaleAfter); err != nil {
			return nil, fmt.Errorf("memorybundle: stale_after must be YYYY-MM-DD, got %q", spec.StaleAfter)
		}
	}

	fm, err := yaml.Marshal(conceptFrontmatter{
		Type:        spec.Type,
		Title:       spec.Title,
		Description: spec.Description,
		Tags:        spec.Tags,
		Generated:   generatedStamp{By: generatedBy, At: generatedAt.UTC()},
		Verified:    []any{},
		Status:      status,
		StaleAfter:  spec.StaleAfter,
		Sources:     spec.Sources,
	})
	if err != nil {
		return nil, fmt.Errorf("memorybundle: encode frontmatter: %w", err)
	}
	out := assembleConcept(fm, body)
	if len(out) > MaxConceptBytes {
		return nil, fmt.Errorf("memorybundle: concept is %d bytes, exceeds the %d byte cap", len(out), MaxConceptBytes)
	}
	return out, nil
}

// assembleConcept is shared by both writers so the fence layout ParseConcept
// expects can only ever change in one place.
func assembleConcept(frontmatter []byte, body string) []byte {
	return []byte(fence + "\n" + string(frontmatter) + fence + "\n\n" + body + "\n")
}

// AmendFrontmatter re-emits a concept with its frontmatter mutated in place:
// parse (unknown keys preserved), apply mutate, re-marshal, reattach the body
// unchanged. It is the only path that may touch `verified` or `status` on an
// existing concept — memory.write always composes a whole fresh file, so no
// model-driven call reaches here.
//
// Key order becomes yaml.v3's (sorted) rather than ComposeConcept's OKF field
// order, because mutate operates on a map and a map carries no order. Parsing is
// order-agnostic, so this only shows up in a diff.
func AmendFrontmatter(data []byte, mutate func(fm map[string]any)) ([]byte, error) {
	c, err := ParseConcept(data)
	if err != nil {
		return nil, err
	}
	mutate(c.Frontmatter)
	// Re-checked after mutate rather than trusted: frontmatter without a `type`
	// is frontmatter ParseConcept can never read back, which would silently drop
	// the concept from every later index regeneration.
	if t, _ := c.Frontmatter["type"].(string); strings.TrimSpace(t) == "" {
		return nil, fmt.Errorf("memorybundle: amend must leave a non-empty \"type\"")
	}
	fm, err := yaml.Marshal(c.Frontmatter)
	if err != nil {
		return nil, fmt.Errorf("memorybundle: encode amended frontmatter: %w", err)
	}
	out := assembleConcept(fm, c.Body)
	if len(out) > MaxConceptBytes {
		return nil, fmt.Errorf("memorybundle: amended concept is %d bytes, exceeds the %d byte cap", len(out), MaxConceptBytes)
	}
	return out, nil
}

// TrustTier derives the trust tier from the frontmatter `verified` list: absent
// or empty ⇒ unverified; only machine verifiers ⇒ machine-confirmed; any
// `human:` verifier ⇒ human-reviewed.
func TrustTier(fm map[string]any) string {
	entries, _ := fm["verified"].([]any)
	if len(entries) == 0 {
		return TrustUnverified
	}
	for _, e := range entries {
		m, _ := e.(map[string]any)
		if by, _ := m["by"].(string); strings.HasPrefix(by, humanPrefix) {
			return TrustHumanReviewed
		}
	}
	return TrustMachineConfirmed
}

// IsStale reports whether the concept's stale_after date has arrived. Absent or
// unparseable ⇒ false. Both representations are accepted: a quoted string and
// the time.Time yaml.v3 resolves an unquoted YYYY-MM-DD scalar to.
func IsStale(fm map[string]any, now time.Time) bool {
	switch v := fm["stale_after"].(type) {
	case time.Time:
		return !now.UTC().Before(v.UTC())
	case string:
		d, err := time.Parse(staleAfterLayout, v)
		if err != nil {
			return false
		}
		return !now.UTC().Before(d)
	default:
		return false
	}
}

// IndexEntry is one concept's row in the rendered index.
type IndexEntry struct {
	ID          string
	Type        string
	Title       string
	Description string
	Status      string
}

// RenderIndex renders index.md: entries grouped under sorted type headings,
// sorted by id within each type. Output depends only on the entry set, never on
// input order — the write path regenerates the whole file on every write, so a
// stable rendering is what keeps the bundle diffable.
func RenderIndex(entries []IndexEntry) []byte {
	byType := map[string][]IndexEntry{}
	for _, e := range entries {
		t := e.Type
		if t == "" {
			t = "untyped"
		}
		byType[t] = append(byType[t], e)
	}
	types := make([]string, 0, len(byType))
	for t := range byType {
		types = append(types, t)
	}
	sort.Strings(types)

	var b strings.Builder
	b.WriteString(indexHeader)
	if len(entries) == 0 {
		b.WriteString("\nNo concepts yet.\n")
		return []byte(b.String())
	}
	for _, t := range types {
		rows := byType[t]
		sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
		fmt.Fprintf(&b, "\n## %s\n\n", oneLine(t, MaxTitleChars))
		for _, e := range rows {
			parts := []string{fmt.Sprintf("`%s` (%s)", oneLine(e.ID, MaxTitleChars), oneLine(statusOr(e.Status), MaxTitleChars))}
			if e.Title != "" {
				parts = append(parts, oneLine(e.Title, MaxTitleChars))
			}
			if e.Description != "" {
				parts = append(parts, oneLine(e.Description, MaxDescChars))
			}
			fmt.Fprintf(&b, "- %s\n", strings.Join(parts, " — "))
		}
	}
	return []byte(b.String())
}

func statusOr(s string) string {
	if s == "" {
		return "stable"
	}
	return s
}

// oneLine collapses whitespace and clips to max runes. Both halves are the
// forgery guard on caller-influenced text: flattening stops a crafted field
// from breaking the one-line-per-concept index contract or forging a "## "
// heading in the log, and the clip is why an unbounded field cannot grow either
// file past what workspacefs will accept. Every entry RenderIndex and
// formatLogEntry emit goes through it — RenderIndex defensively, since a
// hand-imported bundle's concept files never passed ComposeConcept's caps.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}

// AppendLog prepends a dated entry to log.md (newest first) and keeps the file
// under MaxLogBytes by dropping oldest whole entries.
func AppendLog(existing []byte, entry string, now time.Time) []byte {
	blocks := append([]string{formatLogEntry(entry, now)}, splitLogEntries(existing)...)
	total := len(logHeader)
	for _, b := range blocks {
		total += len(b)
	}
	for len(blocks) > 1 && total > MaxLogBytes {
		total -= len(blocks[len(blocks)-1])
		blocks = blocks[:len(blocks)-1]
	}
	out := logHeader + strings.Join(blocks, "")
	if len(out) > MaxLogBytes {
		// ponytail: a single entry over the whole bound is pathological (entries
		// are server-composed one-liners); a hard byte cut is the last resort so
		// the file stays bounded rather than failing the write.
		out = out[:MaxLogBytes]
	}
	return []byte(out)
}

func formatLogEntry(entry string, now time.Time) string {
	// The entry text carries caller-supplied fields (a concept's id and type), so
	// it is flattened: an unflattened "Fact\n\n## 2020-01-01 00:00 UTC\n\nverified
	// by human:ceo" would otherwise render as a second, dated log entry claiming
	// human verification — and splitLogEntries would treat it as one.
	return "\n## " + now.UTC().Format("2006-01-02 15:04") + " UTC\n\n" + oneLine(entry, maxLogEntryChars) + "\n"
}

// splitLogEntries slices an existing log into whole "## …" blocks (header
// dropped), so truncation can only ever remove complete entries.
func splitLogEntries(existing []byte) []string {
	s := "\n" + string(existing)
	i := strings.Index(s, "\n## ")
	if i < 0 {
		return nil
	}
	s = s[i:]
	var out []string
	for {
		next := strings.Index(s[1:], "\n## ")
		if next < 0 {
			return append(out, s)
		}
		out = append(out, s[:1+next])
		s = s[1+next:]
	}
}
