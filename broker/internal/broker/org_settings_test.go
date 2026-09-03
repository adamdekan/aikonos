package broker

import (
	"context"
	"testing"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/db"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

// orgSettingsPatch must build a patch containing ONLY the fields the caller set.
// This is the load-bearing contract behind partial merge: an admin page that
// touches one setting must never zero out another page's field.
func TestOrgSettingsPatch_OnlyPresentFields(t *testing.T) {
	// No fields set → empty patch, no keys.
	patch, changed := orgSettingsPatch(&brokerv1.OrgSettings{})
	if len(patch) != 0 {
		t.Fatalf("expected empty patch for no-op update, got %v", patch)
	}
	if len(changed) != 0 {
		t.Fatalf("expected no changed keys, got %v", changed)
	}

	// Only instruction_preamble set → exactly that key present, value preserved,
	// including the empty string (an explicit clear, not an omission).
	for _, val := range []string{"Never include PII.", ""} {
		p := &brokerv1.OrgSettings{InstructionPreamble: &val}
		patch, changed := orgSettingsPatch(p)
		if len(patch) != 1 {
			t.Fatalf("expected exactly one patched key for val=%q, got %v", val, patch)
		}
		got, ok := patch["instruction_preamble"]
		if !ok {
			t.Fatalf("instruction_preamble missing from patch for val=%q", val)
		}
		if got != val {
			t.Fatalf("patch value = %q, want %q", got, val)
		}
		if len(changed) != 1 || changed[0] != "instruction_preamble" {
			t.Fatalf("changed keys = %v, want [instruction_preamble]", changed)
		}
	}
}

// A8: the unattended flag patches only when set, and its default (unset) is
// allowed=true — the fail-open governance default.
func TestOrgSettingsPatch_UnattendedFlag(t *testing.T) {
	tru := true
	fls := false

	for _, tc := range []struct {
		name string
		in   *brokerv1.OrgSettings
		want bool
	}{
		{"explicit true", &brokerv1.OrgSettings{UnattendedAllowed: &tru}, true},
		{"explicit false", &brokerv1.OrgSettings{UnattendedAllowed: &fls}, false},
	} {
		patch, changed := orgSettingsPatch(tc.in)
		if len(patch) != 1 {
			t.Fatalf("%s: expected one patched key, got %v", tc.name, patch)
		}
		got, ok := patch["unattended_allowed"]
		if !ok {
			t.Fatalf("%s: unattended_allowed missing from patch", tc.name)
		}
		if got != tc.want {
			t.Fatalf("%s: patch value = %v, want %v", tc.name, got, tc.want)
		}
		if len(changed) != 1 || changed[0] != "unattended_allowed" {
			t.Fatalf("%s: changed = %v", tc.name, changed)
		}
	}

	// Default resolution: nil pointer → allowed (true).
	if !(db.OrgSettings{}).UnattendedAllowedOrDefault() {
		t.Fatal("unset UnattendedAllowed must default to true (allowed)")
	}
	if (db.OrgSettings{UnattendedAllowed: &fls}).UnattendedAllowedOrDefault() {
		t.Fatal("explicit false must resolve to false")
	}
}

// A6: the workflow-sharing default is allowed, and the guard denies only on an
// explicit false. requireWorkflowSharingAllowed with a nil repo is a no-op
// (feature not configured → no restriction).
func TestWorkflowSharingDefault(t *testing.T) {
	if !(db.OrgSettings{}).WorkflowSharingAllowedOrDefault() {
		t.Fatal("unset workflow-sharing must default to allowed")
	}
	fls := false
	if (db.OrgSettings{WorkflowSharingAllowed: &fls}).WorkflowSharingAllowedOrDefault() {
		t.Fatal("explicit false must resolve to false")
	}
	// nil repo → no restriction, no panic.
	if err := requireWorkflowSharingAllowed(context.Background(), nil, "t1"); err != nil {
		t.Fatalf("nil repo must be a no-op, got %v", err)
	}
}

// A7: the connector allowlist is a no-op when disabled and enforces membership
// when enabled.
func TestConnectorAllowlist(t *testing.T) {
	// Disabled (nil) → everything allowed.
	if !(db.OrgSettings{}).ConnectorAllowed("google_drive") {
		t.Fatal("disabled allowlist must permit any provider")
	}
	// Enabled but empty → nothing allowed.
	enabled := true
	s := db.OrgSettings{ConnectorAllowlistEnabled: &enabled, ConnectorAllowlist: nil}
	if s.ConnectorAllowed("google_drive") {
		t.Fatal("enabled empty allowlist must permit nothing")
	}
	// Enabled with a member → only that member allowed.
	s.ConnectorAllowlist = []string{"google_drive"}
	if !s.ConnectorAllowed("google_drive") {
		t.Fatal("listed provider must be allowed")
	}
	if s.ConnectorAllowed("onedrive") {
		t.Fatal("unlisted provider must be denied when allowlist enabled")
	}

	// Patch couples the flag and list.
	patch, _ := orgSettingsPatch(&brokerv1.OrgSettings{
		ConnectorAllowlistEnabled: &enabled,
		ConnectorAllowlist:        []string{"onedrive"},
	})
	if patch["connector_allowlist_enabled"] != true {
		t.Fatalf("enabled flag missing from patch: %v", patch)
	}
	got, ok := patch["connector_allowlist"].([]string)
	if !ok || len(got) != 1 || got[0] != "onedrive" {
		t.Fatalf("list missing/wrong in patch: %v", patch["connector_allowlist"])
	}
}

// A2: the disabled-effect-class set drives EffectClassDisabled, the wrapped
// capabilities message patches correctly, and the UI's class-name constants
// match the broker EffectClass enum String() names (the cross-layer contract).
func TestCapabilityDisable(t *testing.T) {
	s := db.OrgSettings{DisabledEffectClasses: []string{"NETWORK_EGRESS", "DESTRUCTIVE"}}
	if !s.EffectClassDisabled("NETWORK_EGRESS") {
		t.Fatal("listed class must be reported disabled")
	}
	if s.EffectClassDisabled("READ_ONLY") {
		t.Fatal("unlisted class must not be disabled")
	}

	// Wrapped capabilities message → patch key present with the list.
	patch, _ := orgSettingsPatch(&brokerv1.OrgSettings{
		Capabilities: &brokerv1.CapabilityPolicy{DisabledEffectClasses: []string{"WRITE_EXTERNAL"}},
	})
	got, ok := patch["disabled_effect_classes"].([]string)
	if !ok || len(got) != 1 || got[0] != "WRITE_EXTERNAL" {
		t.Fatalf("capabilities patch wrong: %v", patch["disabled_effect_classes"])
	}
	// Absent capabilities message → no key (leaves the set untouched).
	patchEmpty, _ := orgSettingsPatch(&brokerv1.OrgSettings{})
	if _, present := patchEmpty["disabled_effect_classes"]; present {
		t.Fatal("absent capabilities must not patch the disabled set")
	}

	// Cross-layer contract: the exact enum names the Capabilities.vue page sends.
	if planv1.EffectClass_NETWORK_EGRESS.String() != "NETWORK_EGRESS" {
		t.Fatalf("NETWORK_EGRESS enum name drift: %q", planv1.EffectClass_NETWORK_EGRESS.String())
	}
	if planv1.EffectClass_WRITE_EXTERNAL.String() != "WRITE_EXTERNAL" {
		t.Fatalf("WRITE_EXTERNAL enum name drift: %q", planv1.EffectClass_WRITE_EXTERNAL.String())
	}
	if planv1.EffectClass_CREDENTIAL_ACCESS.String() != "CREDENTIAL_ACCESS" {
		t.Fatalf("CREDENTIAL_ACCESS enum name drift: %q", planv1.EffectClass_CREDENTIAL_ACCESS.String())
	}
	if planv1.EffectClass_DESTRUCTIVE.String() != "DESTRUCTIVE" {
		t.Fatalf("DESTRUCTIVE enum name drift: %q", planv1.EffectClass_DESTRUCTIVE.String())
	}
}

// orgSettingsToProto must surface the preamble (even empty) as a set optional
// field so the client always gets a defined value, and must format provenance.
func TestOrgSettingsToProto(t *testing.T) {
	ts := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	out := orgSettingsToProto(db.OrgSettings{
		InstructionPreamble: "hello",
		UpdatedBy:           "admin@x.com",
		UpdatedAt:           ts,
	})
	if out.InstructionPreamble == nil || out.GetInstructionPreamble() != "hello" {
		t.Fatalf("preamble not surfaced: %v", out.InstructionPreamble)
	}
	if out.UpdatedBy != "admin@x.com" {
		t.Fatalf("updated_by = %q", out.UpdatedBy)
	}
	if out.UpdatedAt != "2026-07-05T12:00:00Z" {
		t.Fatalf("updated_at = %q, want RFC3339 UTC", out.UpdatedAt)
	}

	// Zero UpdatedAt → empty string, and empty preamble is still a set field.
	zero := orgSettingsToProto(db.OrgSettings{})
	if zero.UpdatedAt != "" {
		t.Fatalf("zero updated_at should be empty, got %q", zero.UpdatedAt)
	}
	if zero.InstructionPreamble == nil {
		t.Fatalf("empty preamble must still be a set optional field")
	}
}
