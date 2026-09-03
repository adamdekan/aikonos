// Package ids generates identifiers for broker domain objects.
package ids

import "github.com/google/uuid"

// EventID returns a time-ordered UUIDv7 string for an audit event_id, per
// proto/audit.proto ("UUIDv7 — sortable by time"). Time-ordering makes the
// MinIO audit objects list chronologically and lets the audit chain-head resume
// pick the newest event reliably. Falls back to v4 only if the RNG fails.
func EventID() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}
