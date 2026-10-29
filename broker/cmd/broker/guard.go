// broker/cmd/broker/guard.go
// Startup security guard: refuses to start when security-critical subsystems
// are in dev-fallback mode and AIKONOS_DEV_MODE is not explicitly set.
package main

import (
	"fmt"
	"sort"
	"strings"
)

// devModeGuard checks whether any of the three security-critical subsystems
// are degraded (not enabled/durable). Returns the list of degraded dimension
// names and, when devMode is false and any are degraded, a fatal error.
//
// Degraded dimensions:
//   - "OIDC"          — oidcEnabled is false (no issuer configured)
//   - "OpenFGA"       — fgaEnabled is false (no store id configured)
//   - "capability-key" — capabilityDurable is false (ephemeral key in use)
//
// When devMode is true the caller should log a WARN with the degraded list but
// proceed; when devMode is false and any dimension is degraded the returned
// error is non-nil and the caller should log.Fatal.
func devModeGuard(devMode, oidcEnabled, fgaEnabled, capabilityDurable bool) (degraded []string, fatal error) {
	if !oidcEnabled {
		degraded = append(degraded, "OIDC")
	}
	if !fgaEnabled {
		degraded = append(degraded, "OpenFGA")
	}
	if !capabilityDurable {
		degraded = append(degraded, "capability-key")
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
func weakSecret(key string) string {
	l := strings.ToLower(key)
	switch {
	case strings.HasPrefix(l, "<"), strings.Contains(l, "change-me"), strings.Contains(l, "changeme"):
		return "is a placeholder"
	case len(key) < minSecretBytes:
		return fmt.Sprintf("is shorter than %d bytes", minSecretBytes)
	}
	return ""
}

// secretsGuard runs weakSecret over every named secret value. Like
// devModeGuard it returns the findings, and a fatal error only when devMode is
// false: dev mode keeps the shipped local keys working, with a WARN.
func secretsGuard(devMode bool, secrets map[string]string) (weak []string, fatal error) {
	for name, value := range secrets {
		if why := weakSecret(value); why != "" {
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
