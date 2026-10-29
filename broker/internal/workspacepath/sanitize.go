// Package workspacepath holds the single (tenant, user) → path-segment
// sanitize rule shared by every store that maps identity claims onto the
// on-disk workspace tree: broker/internal/workspacefs (file explorer),
// broker/internal/toolproxy (doc.write), and broker/internal/connectorstore
// (mcp_connections.json). Before this package existed the three stores each
// carried their own copy; connectorstore's had drifted to a reject-only rule,
// so the same (tenant, user) pair could resolve to different directories
// depending on which store touched it. SanitizeSeg is now that one rule.
package workspacepath

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// SanitizeSeg maps an arbitrary tenant or user identifier onto a safe single
// path segment, one-to-one (F175/G32, L-10). An identifier of [a-zA-Z0-9_-]
// only (UUIDs, group ids) is its own segment; any other is lossily mapped
// (each other byte becomes '_') and suffixed with "." and 16 hex digits of
// its SHA-256, so "a.b@corp" and "a_b@corp" no longer share a directory. A
// passthrough segment never contains '.', so the two forms cannot meet. An
// empty input becomes "_". It never errors — every input has a defined output.
func SanitizeSeg(seg string) string {
	if seg == "" {
		return "_"
	}
	lossy := legacySeg(seg)
	if lossy == seg {
		return seg
	}
	sum := sha256.Sum256([]byte(seg))
	return lossy + "." + hex.EncodeToString(sum[:8])
}

// Hashed reports whether seg has the "<lossy>.<16 hex>" shape SanitizeSeg
// gives a non-passthrough identifier, so it may be another identity's
// directory name. A store that joins a raw identifier into a path (a
// pre-sanitize fallback) must refuse such a segment.
func Hashed(seg string) bool {
	lossy, sum, ok := strings.Cut(seg, ".")
	if !ok || len(sum) != 16 || legacySeg(lossy) != lossy {
		return false
	}
	_, err := hex.DecodeString(sum)
	return err == nil
}

// legacySeg is the pre-F175 many-to-one rule: each byte outside [a-zA-Z0-9_-]
// becomes '_'. Kept for the suffix and for MigrateLegacy.
func legacySeg(seg string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, seg)
}

// Dir is root/SanitizeSeg(tenant)/SanitizeSeg(user). It never moves a
// pre-F175 directory: a caller whose subject collides with another's old
// lossy name must not claim it by arriving first. MigrateLegacy does the
// one-time move at startup, to the one known owner only.
func Dir(root, tenant, user string) string {
	return filepath.Join(root, SanitizeSeg(tenant), SanitizeSeg(user))
}

// migratedMarker marks a tenant directory MigrateLegacy has run on. Neither it
// nor the ".legacy-" quarantine names can be reached through Dir: SanitizeSeg
// never yields a segment that starts with '.'.
const migratedMarker = ".f175-migrated"

// MigrateLegacy is the one-time move of tenant's pre-F175 workspaces (named by
// the lossy legacySeg) to their collision-free names (G32/F175). subjects is
// every user the deployment knows. A legacy directory exactly one known subject
// maps to moves to that subject. One that several known subjects map to was
// shared before the fix: it moves to ".legacy-<name>" in the tenant directory,
// where no identity reaches it. One whose new name already holds data stays
// put. Both are returned (their current paths) for an admin to split by hand.
// One no known subject maps to stays where it is. Run it before serving; a
// marker file makes later runs no-ops, so a subject that appears after the
// upgrade can never take a directory over on a restart. Recovery: an admin
// who wants a rerun (say, for a user missing from both lists) deletes the
// marker and restarts.
func MigrateLegacy(root, tenant string, subjects []string) (ambiguous []string, err error) {
	if root == "" || tenant == "" {
		return nil, nil
	}
	tdir := filepath.Join(root, SanitizeSeg(tenant))
	marker := filepath.Join(tdir, migratedMarker)
	if _, err := os.Stat(marker); err == nil {
		return nil, nil
	}
	owners := map[string]map[string]bool{}
	for _, s := range subjects {
		// group-<gid> names a group memory bundle: a pre-F175 subject of that
		// shape (it can no longer sign in) must never be handed one.
		if s == "" || strings.HasPrefix(s, "group-") {
			continue
		}
		l := legacySeg(s)
		if owners[l] == nil {
			owners[l] = map[string]bool{}
		}
		owners[l][s] = true
	}
	for l, set := range owners {
		from := filepath.Join(root, legacySeg(tenant), l)
		if fi, err := os.Stat(from); err != nil || !fi.IsDir() {
			continue
		}
		to := filepath.Join(tdir, ".legacy-"+l)
		if len(set) == 1 {
			for s := range set {
				to = Dir(root, tenant, s)
			}
			if to == from {
				continue
			}
		}
		if _, err := os.Stat(to); err == nil {
			// Both names hold data (a run interrupted, or a write raced the
			// move): merging could mix owners, so an admin decides.
			ambiguous = append(ambiguous, from)
			continue
		}
		if err := os.MkdirAll(tdir, 0o750); err != nil {
			return ambiguous, err
		}
		if err := os.Rename(from, to); err != nil {
			return ambiguous, err
		}
		if len(set) > 1 {
			ambiguous = append(ambiguous, to)
		}
	}
	if err := os.MkdirAll(tdir, 0o750); err != nil {
		return ambiguous, err
	}
	return ambiguous, os.WriteFile(marker, nil, 0o600)
}
