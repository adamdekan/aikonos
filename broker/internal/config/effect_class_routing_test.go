package config_test

// FU3 tests: ParseEffectClassRouting + Posture.Severity.

import (
	"testing"

	"github.com/adamdekan/aikonos/broker/internal/config"
)

// TestParseEffectClassRouting_WellFormed verifies that a valid config string is
// parsed into the expected map.
func TestParseEffectClassRouting_WellFormed(t *testing.T) {
	m := config.ParseEffectClassRouting("write_local:approval,read_only:step_up")
	if got, want := m["write_local"], config.PostureApproval; got != want {
		t.Errorf("write_local = %q, want %q", got, want)
	}
	if got, want := m["read_only"], config.PostureStepUp; got != want {
		t.Errorf("read_only = %q, want %q", got, want)
	}
	if len(m) != 2 {
		t.Errorf("map len = %d, want 2", len(m))
	}
}

// TestParseEffectClassRouting_Empty returns an empty map for an empty string.
func TestParseEffectClassRouting_Empty(t *testing.T) {
	m := config.ParseEffectClassRouting("")
	if len(m) != 0 {
		t.Errorf("want empty map, got %v", m)
	}
}

// TestParseEffectClassRouting_MalformedSkipped verifies that malformed pairs are
// silently skipped and valid pairs in the same string still parse.
func TestParseEffectClassRouting_MalformedSkipped(t *testing.T) {
	// "nocolon" is malformed; "write_external:deny" is valid
	m := config.ParseEffectClassRouting("nocolon,write_external:deny,:missing_class,write_local:")
	if _, bad := m["nocolon"]; bad {
		t.Error("malformed 'nocolon' must not appear in map")
	}
	if got, want := m["write_external"], config.PostureDeny; got != want {
		t.Errorf("write_external = %q, want %q", got, want)
	}
	if len(m) != 1 {
		t.Errorf("map len = %d, want 1 (only write_external:deny valid)", len(m))
	}
}

// TestParseEffectClassRouting_UnknownClassSkipped verifies unknown class names
// are skipped without error.
func TestParseEffectClassRouting_UnknownClassSkipped(t *testing.T) {
	m := config.ParseEffectClassRouting("totally_fake:deny,read_only:deny")
	if _, bad := m["totally_fake"]; bad {
		t.Error("unknown class must be skipped")
	}
	if got, want := m["read_only"], config.PostureDeny; got != want {
		t.Errorf("read_only = %q, want %q", got, want)
	}
}

// TestParseEffectClassRouting_UnknownPostureSkipped verifies unknown posture
// values are skipped without error.
func TestParseEffectClassRouting_UnknownPostureSkipped(t *testing.T) {
	m := config.ParseEffectClassRouting("read_only:relax,write_local:approval")
	if _, bad := m["read_only"]; bad {
		t.Error("unknown posture 'relax' must be skipped")
	}
	if got, want := m["write_local"], config.PostureApproval; got != want {
		t.Errorf("write_local = %q, want %q", got, want)
	}
}

// TestParseEffectClassRouting_DupLastWins verifies that the last pair for a
// class takes precedence when a class appears more than once.
func TestParseEffectClassRouting_DupLastWins(t *testing.T) {
	m := config.ParseEffectClassRouting("write_local:approval,write_local:deny")
	if got, want := m["write_local"], config.PostureDeny; got != want {
		t.Errorf("dup: write_local = %q, want last-wins %q", got, want)
	}
}

// TestParseEffectClassRouting_AllValidPostures verifies all three posture
// constants parse correctly.
func TestParseEffectClassRouting_AllValidPostures(t *testing.T) {
	m := config.ParseEffectClassRouting("read_only:approval,write_local:step_up,write_external:deny")
	if m["read_only"] != config.PostureApproval {
		t.Errorf("read_only posture = %q, want approval", m["read_only"])
	}
	if m["write_local"] != config.PostureStepUp {
		t.Errorf("write_local posture = %q, want step_up", m["write_local"])
	}
	if m["write_external"] != config.PostureDeny {
		t.Errorf("write_external posture = %q, want deny", m["write_external"])
	}
}

// TestPostureSeverity verifies the bucket values that the apply-time comparison
// uses to determine monotonic-stricter.
func TestPostureSeverity(t *testing.T) {
	cases := []struct {
		p    config.Posture
		want int
	}{
		{config.PostureApproval, 2},
		{config.PostureStepUp, 3},
		{config.PostureDeny, 4},
		{"unknown", 0},
	}
	for _, tc := range cases {
		if got := tc.p.Severity(); got != tc.want {
			t.Errorf("Posture(%q).Severity() = %d, want %d", tc.p, got, tc.want)
		}
	}
}
