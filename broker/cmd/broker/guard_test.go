// broker/cmd/broker/guard_test.go
package main

import (
	"strings"
	"testing"
)

func TestDevModeGuard(t *testing.T) {
	tests := []struct {
		name              string
		devMode           bool
		oidcEnabled       bool
		fgaEnabled        bool
		capabilityDurable bool
		wantFatal         bool
		wantDegraded      []string
	}{
		{
			name:              "devMode true, all unconfigured — no error, all three degraded",
			devMode:           true,
			oidcEnabled:       false,
			fgaEnabled:        false,
			capabilityDurable: false,
			wantFatal:         false,
			wantDegraded:      []string{"OIDC", "OpenFGA", "capability-key"},
		},
		{
			name:              "devMode false, all configured — no error, empty degraded",
			devMode:           false,
			oidcEnabled:       true,
			fgaEnabled:        true,
			capabilityDurable: true,
			wantFatal:         false,
			wantDegraded:      nil,
		},
		{
			name:              "devMode false, FGA only unconfigured — fatal naming OpenFGA",
			devMode:           false,
			oidcEnabled:       true,
			fgaEnabled:        false,
			capabilityDurable: true,
			wantFatal:         true,
			wantDegraded:      []string{"OpenFGA"},
		},
		{
			name:              "devMode false, OIDC+capability unconfigured — fatal naming both",
			devMode:           false,
			oidcEnabled:       false,
			fgaEnabled:        true,
			capabilityDurable: false,
			wantFatal:         true,
			wantDegraded:      []string{"OIDC", "capability-key"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			degraded, fatal := devModeGuard(tc.devMode, tc.oidcEnabled, tc.fgaEnabled, tc.capabilityDurable)

			if tc.wantFatal && fatal == nil {
				t.Fatalf("expected fatal error, got nil")
			}
			if !tc.wantFatal && fatal != nil {
				t.Fatalf("expected no error, got: %v", fatal)
			}

			if len(degraded) != len(tc.wantDegraded) {
				t.Fatalf("degraded length: got %d (%v), want %d (%v)",
					len(degraded), degraded, len(tc.wantDegraded), tc.wantDegraded)
			}
			for i, want := range tc.wantDegraded {
				if degraded[i] != want {
					t.Errorf("degraded[%d]: got %q, want %q", i, degraded[i], want)
				}
			}

			// Fatal error must mention all expected degraded names.
			if fatal != nil {
				msg := fatal.Error()
				for _, name := range tc.wantDegraded {
					if !strings.Contains(msg, name) {
						t.Errorf("fatal error %q does not mention %q", msg, name)
					}
				}
				if !strings.Contains(msg, "AIKONOS_DEV_MODE") {
					t.Errorf("fatal error %q does not mention AIKONOS_DEV_MODE", msg)
				}
			}
		})
	}
}
