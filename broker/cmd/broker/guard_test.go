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
		auditDurable      bool
		auditSigned       bool
		wantFatal         bool
		wantDegraded      []string
	}{
		{
			name:              "devMode true, all unconfigured — no error, all degraded",
			devMode:           true,
			oidcEnabled:       false,
			fgaEnabled:        false,
			capabilityDurable: false,
			wantFatal:         false,
			wantDegraded:      []string{"OIDC", "OpenFGA", "capability-key", "audit-store", "audit-signing"},
		},
		{
			name:              "devMode false, all configured — no error, empty degraded",
			devMode:           false,
			oidcEnabled:       true,
			fgaEnabled:        true,
			capabilityDurable: true,
			auditDurable:      true,
			auditSigned:       true,
			wantFatal:         false,
			wantDegraded:      nil,
		},
		{
			name:              "devMode false, FGA only unconfigured — fatal naming OpenFGA",
			devMode:           false,
			oidcEnabled:       true,
			fgaEnabled:        false,
			capabilityDurable: true,
			auditDurable:      true,
			auditSigned:       true,
			wantFatal:         true,
			wantDegraded:      []string{"OpenFGA"},
		},
		{
			name:              "devMode false, OIDC+capability unconfigured — fatal naming both",
			devMode:           false,
			oidcEnabled:       false,
			fgaEnabled:        true,
			capabilityDurable: false,
			auditDurable:      true,
			auditSigned:       true,
			wantFatal:         true,
			wantDegraded:      []string{"OIDC", "capability-key"},
		},
		{
			name:              "devMode false, audit logging-only — fatal naming audit-store",
			devMode:           false,
			oidcEnabled:       true,
			fgaEnabled:        true,
			capabilityDurable: true,
			auditDurable:      false,
			auditSigned:       true,
			wantFatal:         true,
			wantDegraded:      []string{"audit-store"},
		},
		{
			name:              "devMode false, audit unsigned — fatal naming audit-signing",
			devMode:           false,
			oidcEnabled:       true,
			fgaEnabled:        true,
			capabilityDurable: true,
			auditDurable:      true,
			auditSigned:       false,
			wantFatal:         true,
			wantDegraded:      []string{"audit-signing"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			degraded, fatal := devModeGuard(tc.devMode, tc.oidcEnabled, tc.fgaEnabled, tc.capabilityDurable, tc.auditDurable, tc.auditSigned)

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

// F199/F227: placeholder and short secrets are fatal outside dev mode and a
// WARN-only finding inside it.
func TestSecretsGuard(t *testing.T) {
	strong := strings.Repeat("k", minSecretBytes)
	for name, value := range map[string]string{
		"empty":          "",
		"short":          "short-key",
		"onprem example": "<AUDIT_SIGNING_KEY-CHANGE-ME>",
		"local example":  "dev-audit-hmac-key-change-me-padded-to-length",
		"changeme":       strings.Repeat("x", 40) + "changeme",
	} {
		weak, fatal := secretsGuard(false, map[string]string{"audit signing key": value, "api key pepper": strong})
		if fatal == nil || len(weak) != 1 {
			t.Errorf("%s: weak=%v fatal=%v, want one finding and a fatal error", name, weak, fatal)
		}
		if weak, fatal := secretsGuard(true, map[string]string{"audit signing key": value}); fatal != nil || len(weak) != 1 {
			t.Errorf("%s in dev mode: weak=%v fatal=%v, want a finding and no fatal error", name, weak, fatal)
		}
	}
	if weak, fatal := secretsGuard(false, map[string]string{"audit signing key": strong}); fatal != nil || weak != nil {
		t.Errorf("strong key: weak=%v fatal=%v", weak, fatal)
	}
}

// The office-worker token and the NATS credential are shared secrets too: the
// shipped example values must be fatal outside dev mode, like the HMAC keys.
func TestTransportSecrets(t *testing.T) {
	strong := strings.Repeat("k", minSecretBytes)
	got := transportSecrets("http://office-worker:8081", "<OFFICE_WORKER_TOKEN-CHANGE-ME>",
		"nats://broker:dev-nats-broker-change-me@nats:4222")
	if got["office worker token"] != "<OFFICE_WORKER_TOKEN-CHANGE-ME>" || got["nats password"] != "dev-nats-broker-change-me" {
		t.Fatalf("transportSecrets = %v", got)
	}
	if weak, fatal := secretsGuard(false, got); fatal == nil || len(weak) != 2 {
		t.Errorf("shipped placeholders: weak=%v fatal=%v, want two findings and a fatal error", weak, fatal)
	}
	// A bare token rides as the URL username.
	if got := transportSecrets("", "", "nats://"+strong+"@nats:4222"); got["nats token"] != strong || len(got) != 1 {
		t.Errorf("token URL: %v", got)
	}
	// No worker, no credential in the URL (or no URL): nothing to check.
	for _, u := range []string{"nats://nats:4222", ""} {
		if got := transportSecrets("", "", u); len(got) != 0 {
			t.Errorf("transportSecrets(%q) = %v, want none", u, got)
		}
	}
	// A configured worker without a token is checked (and so fatal), not skipped.
	if got := transportSecrets("http://office-worker:8081", "", ""); len(got) != 1 {
		t.Errorf("worker without token: %v", got)
	}
}
