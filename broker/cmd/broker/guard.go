// broker/cmd/broker/guard.go
// Startup security guard: refuses to start when security-critical subsystems
// are in dev-fallback mode and AIKONOS_DEV_MODE is not explicitly set.
package main

import "fmt"

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
