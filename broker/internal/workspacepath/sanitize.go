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

// legacySeg is the pre-F175 many-to-one rule: each byte outside [a-zA-Z0-9_-]
// becomes '_'. Kept for the suffix and for Dir's one-time move.
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

// Dir is root/SanitizeSeg(tenant)/SanitizeSeg(user). A directory still under
// the pre-F175 name (legacySeg of both) is moved there on first use, so
// existing workspaces stay readable. Where two identifiers shared that old
// directory, the first to arrive after the upgrade takes it.
// ponytail: one stat per call; cache migrated keys if this ever shows up.
func Dir(root, tenant, user string) string {
	dir := filepath.Join(root, SanitizeSeg(tenant), SanitizeSeg(user))
	if root == "" || tenant == "" || user == "" {
		return dir
	}
	old := filepath.Join(root, legacySeg(tenant), legacySeg(user))
	if old == dir {
		return dir
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if fi, err := os.Stat(old); err == nil && fi.IsDir() {
			// Best-effort: a racing caller may have moved it already.
			_ = os.MkdirAll(filepath.Dir(dir), 0o750)
			_ = os.Rename(old, dir)
		}
	}
	return dir
}
