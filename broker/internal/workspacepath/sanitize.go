// Package workspacepath holds the single (tenant, user) → path-segment
// sanitize rule shared by every store that maps identity claims onto the
// on-disk workspace tree: broker/internal/workspacefs (file explorer),
// broker/internal/toolproxy (doc.write), and broker/internal/connectorstore
// (mcp_connections.json). Before this package existed the three stores each
// carried their own copy; connectorstore's had drifted to a reject-only rule,
// so the same (tenant, user) pair could resolve to different directories
// depending on which store touched it. SanitizeSeg is now that one rule.
package workspacepath

import "strings"

// SanitizeSeg maps an arbitrary tenant or user identifier onto a safe single
// path segment: any byte outside [a-zA-Z0-9_-] becomes '_', and an empty
// input becomes "_". It never errors — every input has a defined output.
func SanitizeSeg(seg string) string {
	if seg == "" {
		return "_"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, seg)
}
