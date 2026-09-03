// broker/internal/audit/signing_key.go
// SigningKeySource abstracts where the audit HMAC signing key comes from, so
// the emitter and VerifyChainVersioned depend on an interface — never a
// concrete Vault client — and stay unit-testable with a fake. The production
// implementation (Vault KV-v2, versioned, cmd/broker) lives outside this
// package; it is the one place that already depends on both audit and
// broker/internal/secrets.
package audit

import (
	"context"
	"fmt"
)

// SigningKeySource resolves the audit HMAC-SHA256 signing key.
type SigningKeySource interface {
	// Current returns the key to sign new events with, and the
	// signing_key_version to record on them. version 0 means the
	// AIKONOS_AUDIT_SIGNING_KEY env/static fallback (Vault unavailable or
	// unconfigured); an empty key means events are chained but unsigned.
	Current() (key string, version int32)

	// ForVersion resolves the key for a previously-recorded
	// signing_key_version, so VerifyChain can validate events signed before a
	// rotation. Implementations must resolve version 0 without any I/O.
	ForVersion(ctx context.Context, version int32) (key string, err error)
}

// staticKeySource is the fallback SigningKeySource used when no Vault-backed
// source is wired (Config.SigningKeySource left nil) — every event signs
// under version 0 with the single configured key (possibly empty/unsigned).
type staticKeySource struct{ key string }

func (s staticKeySource) Current() (string, int32) { return s.key, 0 }

func (s staticKeySource) ForVersion(_ context.Context, version int32) (string, error) {
	if version != 0 {
		return "", fmt.Errorf("audit: no key source for signing_key_version %d (static fallback only resolves version 0)", version)
	}
	return s.key, nil
}
