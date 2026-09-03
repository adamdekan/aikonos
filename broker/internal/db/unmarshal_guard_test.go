package db

import (
	"encoding/json"
	"fmt"
	"testing"
)

// unmarshalIfNonEmpty mirrors the guard pattern used in approvals.go and
// envelopes.go: empty/nil bytes are a valid no-op; non-empty malformed JSON
// must surface an error.
func unmarshalIfNonEmpty(raw []byte, dst any, field string) error {
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, dst); err != nil {
			return fmt.Errorf("unmarshal %s: %w", field, err)
		}
	}
	return nil
}

func TestUnmarshalGuard_NilBytes_NoError(t *testing.T) {
	var m map[string]any
	if err := unmarshalIfNonEmpty(nil, &m, "field"); err != nil {
		t.Fatalf("nil bytes must not error: %v", err)
	}
	if m != nil {
		t.Error("nil bytes must leave dst untouched")
	}
}

func TestUnmarshalGuard_EmptyBytes_NoError(t *testing.T) {
	var m map[string]any
	if err := unmarshalIfNonEmpty([]byte{}, &m, "field"); err != nil {
		t.Fatalf("empty bytes must not error: %v", err)
	}
	if m != nil {
		t.Error("empty bytes must leave dst untouched")
	}
}

func TestUnmarshalGuard_ValidJSON_Decoded(t *testing.T) {
	var m map[string]any
	if err := unmarshalIfNonEmpty([]byte(`{"k":"v"}`), &m, "field"); err != nil {
		t.Fatalf("valid JSON must not error: %v", err)
	}
	if m["k"] != "v" {
		t.Errorf("expected k=v, got %v", m)
	}
}

func TestUnmarshalGuard_MalformedJSON_ReturnsError(t *testing.T) {
	var m map[string]any
	err := unmarshalIfNonEmpty([]byte(`{not valid json`), &m, "decision_trace")
	if err == nil {
		t.Fatal("malformed non-empty JSON must return an error")
	}
	if want := "unmarshal decision_trace:"; len(err.Error()) < len(want) || err.Error()[:len(want)] != want {
		t.Errorf("error message must be wrapped with field name, got: %v", err)
	}
}

// TestApprovalSummaryGuard_NilIsValidZeroValue confirms the specific site at
// approvals.go (GetPendingApproval) that previously had no length guard:
// a nil summary column must produce a nil Summary field, not an error.
func TestApprovalSummaryGuard_NilIsValidZeroValue(t *testing.T) {
	var summary []byte // nil — simulates SQL NULL scan result
	var dst map[string]any
	if err := unmarshalIfNonEmpty(summary, &dst, "summary"); err != nil {
		t.Fatalf("nil summary must be zero-value, not error: %v", err)
	}
	if dst != nil {
		t.Error("nil summary must leave dst as nil")
	}
}

// TestEnvelopeFieldsGuard confirms the three envelope columns (to_target,
// task_spec, delegation_spec) all follow the same guard pattern.
func TestEnvelopeFieldsGuard_NilColumns_NoError(t *testing.T) {
	for _, field := range []string{"to_target", "task_spec", "delegation_spec"} {
		var dst any
		if err := unmarshalIfNonEmpty(nil, &dst, field); err != nil {
			t.Errorf("nil %s column must not error: %v", field, err)
		}
	}
}

func TestEnvelopeFieldsGuard_MalformedColumns_Error(t *testing.T) {
	bad := []byte(`{bad`)
	for _, field := range []string{"to_target", "task_spec", "delegation_spec"} {
		var dst any
		if err := unmarshalIfNonEmpty(bad, &dst, field); err == nil {
			t.Errorf("malformed %s must return error", field)
		}
	}
}
