// broker/cmd/broker/guard.go
// Startup security guard: refuses to start when security-critical subsystems
// are in dev-fallback mode and AIKONOS_DEV_MODE is not explicitly set.
package main

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// devModeGuard checks whether any of the security-critical subsystems
// are degraded (not enabled/durable). Returns the list of degraded dimension
// names and, when devMode is false and any are degraded, a fatal error.
//
// Degraded dimensions:
//   - "OIDC"          — oidcEnabled is false (no issuer configured)
//   - "OpenFGA"       — fgaEnabled is false (no store id configured)
//   - "capability-key" — capabilityDurable is false (ephemeral key in use)
//   - "audit-store"   — auditDurable is false (no MinIO sink: logging-only audit)
//   - "audit-signing" — auditSigned is false (events chained but unsigned)
//
// When devMode is true the caller should log a WARN with the degraded list but
// proceed; when devMode is false and any dimension is degraded the returned
// error is non-nil and the caller should log.Fatal.
func devModeGuard(devMode, oidcEnabled, fgaEnabled, capabilityDurable, auditDurable, auditSigned bool) (degraded []string, fatal error) {
	if !oidcEnabled {
		degraded = append(degraded, "OIDC")
	}
	if !fgaEnabled {
		degraded = append(degraded, "OpenFGA")
	}
	if !capabilityDurable {
		degraded = append(degraded, "capability-key")
	}
	if !auditDurable {
		degraded = append(degraded, "audit-store")
	}
	if !auditSigned {
		degraded = append(degraded, "audit-signing")
	}

	if !devMode && len(degraded) > 0 {
		fatal = fmt.Errorf(
			"refusing to start: %v unconfigured and AIKONOS_DEV_MODE!=true; "+
				"set the missing config or AIKONOS_DEV_MODE=true to run degraded (dev only)",
			degraded,
		)
	}
	return degraded, fatal
}

// minSecretBytes is the shortest HMAC key or pepper accepted outside dev mode.
const minSecretBytes = 32

// weakSecret reports why a key is unfit outside dev mode (F199, F227) — a
// shipped placeholder (the example env files' "<...-CHANGE-ME>" and
// "...-change-me" values are public) or too short — or "" when it is fine.
//
// The "<" prefix test runs only on operator text (binary=false): a random
// binary key (Vault-generated or base64-decoded) starts with 0x3C one time in
// 256. The change-me substrings stay on for binary keys — the local example's
// session key decodes to "...-change-me" — since random bytes hit them about
// once in 2^56.
func weakSecret(key string, binary bool) string {
	l := strings.ToLower(key)
	switch {
	case !binary && strings.HasPrefix(l, "<"), strings.Contains(l, "change-me"), strings.Contains(l, "changeme"):
		return "is a placeholder"
	case len(key) < minSecretBytes:
		return fmt.Sprintf("is shorter than %d bytes", minSecretBytes)
	}
	return ""
}

// secretsGuard runs weakSecret over every named secret: text holds
// operator-supplied strings used as keys as-is, keys holds binary key bytes.
// Like devModeGuard it returns the findings, and a fatal error only when
// devMode is false: dev mode keeps the shipped local keys working, with a WARN.
func secretsGuard(devMode bool, text map[string]string, keys map[string][]byte) (weak []string, fatal error) {
	for name, value := range text {
		if why := weakSecret(value, false); why != "" {
			weak = append(weak, name+" "+why)
		}
	}
	for name, value := range keys {
		if why := weakSecret(string(value), true); why != "" {
			weak = append(weak, name+" "+why)
		}
	}
	sort.Strings(weak)
	if !devMode && len(weak) > 0 {
		fatal = fmt.Errorf(
			"refusing to start: %s; set strong random values (e.g. openssl rand -base64 32) "+
				"or AIKONOS_DEV_MODE=true to run with them (dev only)",
			strings.Join(weak, "; "),
		)
	}
	return weak, fatal
}

// transportSecrets returns, for secretsGuard, the shared secrets the broker
// presents to other services: the office-worker token when a worker is
// configured, and the credential compose puts in the NATS URL
// (nats://user:password@… or nats://token@…). An unparseable URL adds nothing:
// notify.New then fails closed on it.
func transportSecrets(officeWorkerURL, officeToken, natsURL string) map[string]string {
	out := map[string]string{}
	if officeWorkerURL != "" {
		out["office worker token"] = officeToken
	}
	if u, err := url.Parse(natsURL); err == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok {
			out["nats password"] = pw
		} else {
			out["nats token"] = u.User.Username()
		}
	}
	return out
}
