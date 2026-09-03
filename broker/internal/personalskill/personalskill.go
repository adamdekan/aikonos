// Package personalskill is the storage+format layer for user-authored
// Agent-Skills-format skill folders under Skills/<name>/ in a user's own
// workspace.
//
// This file is the pure format half: SKILL.md frontmatter parsing,
// validation, and the package's caps. No I/O — store.go is where the
// workspacefs.Backend calls (shallow scan, snapshot, install, delete) live,
// mirroring memorybundle's format/store split for the same reason: two
// surfaces (the gateway session build and the future transfer RPCs) need the
// same parse/validate rules without duplicating them.
//
// yaml.v3 is used to decode straight into a typed struct rather than
// memorybundle's map[string]any: personal-skill frontmatter has no OKF
// forward-compatibility requirement, so unknown keys are simply dropped by
// yaml.v3's default (non-KnownFields) struct decode — no extra code needed.
package personalskill

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	// MaxSkillMdBytes bounds one SKILL.md file. A larger file is invalid
	// (excluded from the catalog with a warning), never a scan-wide error.
	MaxSkillMdBytes = 256 * 1024
	// MaxSkillFileBytes bounds a single file read served to the model via
	// GetPersonalSkillFileSouth (read_skill_file Pi tool). A file exceeding
	// this cap returns InvalidArgument instead of NotFound (distinct from the
	// missing-file case).
	MaxSkillFileBytes = 256 * 1024
	// MaxFolderBytes bounds a whole skill folder's total content size, enforced
	// by Snapshot at transfer time.
	MaxFolderBytes = 20 * 1024 * 1024
	// MaxFolderFiles bounds a whole skill folder's file count, enforced by
	// Snapshot at transfer time.
	MaxFolderFiles = 100
	// MaxSkills bounds how many entries Scan returns per user (gRPC list-size
	// ceiling — see spec risk table).
	MaxSkills = 50

	// maxKeywords and maxKeywordChars mirror db.NormalizeKeywords' write-time
	// keyword contract (lowercase/trim/dedup), applied here to user-authored
	// frontmatter rather than an admin-authored bundle field. Truncating
	// (rather than rejecting the whole skill) is deliberate: a keyword-list
	// overage on an otherwise-valid skill must not exclude it from discovery —
	// see personalskill.md's design note on this choice.
	maxKeywords     = 32
	maxKeywordChars = 64
)

// Dir is the personal-skills root, relative to the owner's workspace segment.
const Dir = "Skills"

// SkillMdName is the required file inside every skill folder.
const SkillMdName = "SKILL.md"

// SkillDir returns name's folder path relative to the owner's workspace
// segment ("Skills/<name>").
func SkillDir(name string) string { return Dir + "/" + name }

// SkillMdPath returns name's SKILL.md path relative to the owner's workspace
// segment.
func SkillMdPath(name string) string { return SkillDir(name) + "/" + SkillMdName }

const fence = "---"

// dotsOnlyRe rejects a name made of nothing but dots ("." and ".." included) —
// filepath.Clean leaves both unchanged (they are already their own cleanest
// form), so the traversal guard below cannot catch them on its own.
var dotsOnlyRe = regexp.MustCompile(`^\.+$`)

// ValidSkillName reports whether name is safe to use as a single Skills/
// subdirectory segment: non-empty, no path separator, not dots-only, and
// unchanged by filepath.Clean (catches ".." components, trailing slashes,
// and other traversal-shaped input). Every mutating entry point (Install,
// DeleteSkill, FreeName) rejects a name that fails this before it ever
// touches a Backend call.
func ValidSkillName(name string) bool {
	if name == "" {
		return false
	}
	if strings.ContainsRune(name, '/') || strings.ContainsRune(name, '\\') {
		return false
	}
	if dotsOnlyRe.MatchString(name) {
		return false
	}
	return filepath.Clean(name) == name
}

// Frontmatter is SKILL.md's parsed frontmatter block (known fields only —
// unknown keys are dropped by yaml.v3's struct decode).
type Frontmatter struct {
	Name                   string
	Description            string
	AllowedTools           []string
	Keywords               []string
	DisableModelInvocation bool
}

// rawFrontmatter is the yaml.v3 decode target; field names/tags mirror the
// gateway's parseSkillMd (agent-gateway/src/pi/skill-parser.ts) so the two
// formats read the same SKILL.md the same way.
type rawFrontmatter struct {
	Name                   string   `yaml:"name"`
	Description            string   `yaml:"description"`
	AllowedTools           []string `yaml:"allowed-tools"`
	Keywords               []string `yaml:"keywords"`
	DisableModelInvocation bool     `yaml:"disable-model-invocation"`
}

// splitFrontmatter splits data at its leading frontmatter fence, returning
// the raw (undecoded) frontmatter block and the trailing markdown body — the
// one implementation ParseFrontmatter and SplitBody both build on, so the
// fence-split logic lives in exactly one place. Errors only on a
// missing/unterminated fence — a well-known limitation shared with
// memorybundle's ParseConcept: a frontmatter value that happens to contain
// the literal sequence "\n---" would confuse the split. SKILL.md frontmatter
// is short, scalar-valued metadata, so this has not been worth a
// YAML-aware splitter.
func splitFrontmatter(data []byte) (raw, body string, err error) {
	s := string(data)
	if !strings.HasPrefix(s, fence+"\n") {
		return "", "", fmt.Errorf("personalskill: SKILL.md must start with a %q frontmatter fence", fence)
	}
	rest := s[len(fence)+1:]
	end := strings.Index(rest, "\n"+fence)
	if end < 0 {
		return "", "", fmt.Errorf("personalskill: SKILL.md frontmatter fence is unterminated")
	}
	body = strings.TrimLeft(rest[end+1+len(fence):], "\n")
	return rest[:end], body, nil
}

// ParseFrontmatter splits SKILL.md's leading frontmatter block and decodes
// its known fields. Errors only on a missing/unterminated fence or
// unparseable YAML.
func ParseFrontmatter(data []byte) (Frontmatter, error) {
	raw, _, err := splitFrontmatter(data)
	if err != nil {
		return Frontmatter{}, err
	}

	var fm rawFrontmatter
	if err := yaml.Unmarshal([]byte(raw), &fm); err != nil {
		return Frontmatter{}, fmt.Errorf("personalskill: parse frontmatter: %w", err)
	}
	return Frontmatter{
		Name:                   strings.TrimSpace(fm.Name),
		Description:            strings.TrimSpace(fm.Description),
		AllowedTools:           fm.AllowedTools,
		Keywords:               normalizeKeywords(fm.Keywords),
		DisableModelInvocation: fm.DisableModelInvocation,
	}, nil
}

// SplitBody returns data's trailing markdown after the closing frontmatter
// fence — the same split ParseFrontmatter performs internally, exposed for
// callers (broker's GetPersonalSkillSouth) that need the body verbatim
// without re-implementing the fence split.
func SplitBody(data []byte) (string, error) {
	_, body, err := splitFrontmatter(data)
	return body, err
}

// normalizeKeywords mirrors db.NormalizeKeywords' lowercase/trim/dedup
// (order-preserving, first-seen wins) without importing package db, then caps
// the result at maxKeywords entries of at most maxKeywordChars runes —
// truncating an overlong keyword rather than dropping it, and skipping a
// truncation that collides with an already-kept keyword.
func normalizeKeywords(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, kw := range in {
		norm := strings.ToLower(strings.TrimSpace(kw))
		if norm == "" {
			continue
		}
		if n := utf8.RuneCountInString(norm); n > maxKeywordChars {
			norm = string([]rune(norm)[:maxKeywordChars])
		}
		if seen[norm] {
			continue
		}
		seen[norm] = true
		out = append(out, norm)
		if len(out) == maxKeywords {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// SkillEntry is one discovered personal skill's catalog row (frontmatter
// only — no body). Name is always the directory name: frontmatter `name` is
// display-only and never overrides it (spec: "the directory name is the
// skill's identity everywhere").
type SkillEntry struct {
	Name                   string
	Description            string
	Keywords               []string
	AllowedTools           []string
	DisableModelInvocation bool
	// Valid is false when the skill is excluded from the catalog (missing/
	// unparseable/oversize SKILL.md, or a missing description). Warning is
	// always the human-readable reason in that case; it is also set (with
	// Valid still true) on a frontmatter/directory name mismatch.
	Valid   bool
	Warning string
}

// buildEntry composes one catalog row from a directory name and its
// (possibly failed) frontmatter parse. Never returns an error — an invalid
// skill becomes an invalid entry, so one bad skill can never fail Scan.
func buildEntry(dirName string, fm Frontmatter, parseErr error) SkillEntry {
	if parseErr != nil {
		return SkillEntry{Name: dirName, Warning: "unparseable frontmatter: " + parseErr.Error()}
	}
	if fm.Description == "" {
		return SkillEntry{Name: dirName, Warning: "missing description"}
	}
	e := SkillEntry{
		Name:                   dirName,
		Description:            fm.Description,
		Keywords:               fm.Keywords,
		AllowedTools:           fm.AllowedTools,
		DisableModelInvocation: fm.DisableModelInvocation,
		Valid:                  true,
	}
	if fm.Name != "" && fm.Name != dirName {
		e.Warning = fmt.Sprintf("frontmatter name %q does not match directory name %q", fm.Name, dirName)
	}
	return e
}
