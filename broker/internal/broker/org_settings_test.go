package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/db"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
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

// F41: a settings read failure on publish fails closed without handing the
// DB connect error (host, user, database) to a non-admin member.
func TestRequireWorkflowSharingAllowed_ReadErrorIsGeneric(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://leakuser@127.0.0.1:1/leakdb?connect_timeout=2")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	err = requireWorkflowSharingAllowed(context.Background(), db.NewOrgSettingsRepo(pool, zap.NewNop()), "t1")
	if status.Code(err) != codes.Internal {
		t.Fatalf("want Internal (fail closed), got %v", err)
	}
	for _, leak := range []string{"leakuser", "leakdb", "127.0.0.1"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error leaks %q: %v", leak, err)
		}
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

// G6: UpdateOrgSettings refuses input it would store but never apply as
// written; values at the bounds pass.
func TestValidateOrgSettings(t *testing.T) {
	str := func(s string) *string { return &s }
	on := true
	long := strings.Repeat("a", orgPreambleMaxUnits)
	astral := strings.Repeat("😀", orgPreambleMaxUnits/2+1) // 2 UTF-16 units each
	many := make([]string, orgConnectorAllowedMax+1)
	for i := range many {
		many[i] = "c"
	}
	for _, tc := range []struct {
		name string
		in   *brokerv1.OrgSettings
		ok   bool
	}{
		{"preamble at cap", &brokerv1.OrgSettings{InstructionPreamble: str(long)}, true},
		{"preamble over cap", &brokerv1.OrgSettings{InstructionPreamble: str(long + "a")}, false},
		{"preamble over cap in UTF-16", &brokerv1.OrgSettings{InstructionPreamble: str(astral)}, false},
		{"known effect class", &brokerv1.OrgSettings{Capabilities: &brokerv1.CapabilityPolicy{DisabledEffectClasses: []string{"DESTRUCTIVE"}}}, true},
		{"typo effect class", &brokerv1.OrgSettings{Capabilities: &brokerv1.CapabilityPolicy{DisabledEffectClasses: []string{"Destructive"}}}, false},
		{"unspecified effect class", &brokerv1.OrgSettings{Capabilities: &brokerv1.CapabilityPolicy{DisabledEffectClasses: []string{"EFFECT_CLASS_UNSPECIFIED"}}}, false},
		{"allowlist", &brokerv1.OrgSettings{ConnectorAllowlistEnabled: &on, ConnectorAllowlist: []string{"google_drive"}}, true},
		{"allowlist too long", &brokerv1.OrgSettings{ConnectorAllowlistEnabled: &on, ConnectorAllowlist: many}, false},
		{"blank allowlist entry", &brokerv1.OrgSettings{ConnectorAllowlistEnabled: &on, ConnectorAllowlist: []string{""}}, false},
	} {
		if err := validateOrgSettings(tc.in); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

// G8: the org_settings audit event names the resource and decision, records
// the non-preamble new values so a downgrade is visible, and carries the
// preamble only as a sha256 and length, never its text.
func TestEmitOrgSettingsUpdated_DigestsPreambleAndRecordsValues(t *testing.T) {
	sink := &capturingAuditSink{}
	svc := NewBrokerService(Deps{Audit: sink})
	preamble, off, on := "Never share customer data.", false, true
	patch, changed := orgSettingsPatch(&brokerv1.OrgSettings{
		InstructionPreamble:       &preamble,
		UnattendedAllowed:         &off,
		ConnectorAllowlistEnabled: &on,
		ConnectorAllowlist:        []string{"google_drive"},
	})

	svc.emitOrgSettingsUpdated(context.Background(), "trace-1", "t1", "admin@example.com", patch, changed)

	ev := sink.last()
	if ev == nil {
		t.Fatal("expected an audit event")
	}
	if ev.ResourceRef != "aikonos:tenant:t1" || ev.Decision != auditv1.PolicyDecision_ALLOW || ev.TraceId != "trace-1" {
		t.Fatalf("want resource aikonos:tenant:t1, ALLOW, trace-1; got %q %v %q", ev.ResourceRef, ev.Decision, ev.TraceId)
	}
	f := ev.Context.Fields
	sum := sha256.Sum256([]byte(preamble))
	if f["instruction_preamble_sha256"].GetStringValue() != hex.EncodeToString(sum[:]) ||
		f["instruction_preamble_length"].GetNumberValue() != float64(len(preamble)) {
		t.Fatalf("want the preamble digest and length, got %v", f)
	}
	if _, leaked := f["instruction_preamble"]; leaked || strings.Contains(ev.Context.String(), "customer data") {
		t.Fatal("the preamble text must never reach the audit trail")
	}
	if v, ok := f["unattended_allowed"]; !ok || v.GetBoolValue() {
		t.Fatalf("want unattended_allowed=false recorded, got %v", f["unattended_allowed"])
	}
	if l := f["connector_allowlist"].GetListValue(); l == nil || len(l.Values) != 1 || l.Values[0].GetStringValue() != "google_drive" {
		t.Fatalf("want connector_allowlist recorded, got %v", f["connector_allowlist"])
	}
}
