package effectclass_test

import (
	"testing"

	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"

	"github.com/adamdekan/aikonos/broker/internal/effectclass"
)

// TestReadsSensitive_OnlyReadOnly verifies the C2 derivation ceiling: only
// READ_ONLY counts as a sensitive read; all 7 other classes do not.
func TestReadsSensitive_OnlyReadOnly(t *testing.T) {
	cases := []struct {
		class planv1.EffectClass
		want  bool
	}{
		{planv1.EffectClass_READ_ONLY, true},
		{planv1.EffectClass_WRITE_LOCAL, false},
		{planv1.EffectClass_WRITE_INTERNAL, false},
		{planv1.EffectClass_WRITE_EXTERNAL, false},
		{planv1.EffectClass_NETWORK_EGRESS, false},
		{planv1.EffectClass_CREDENTIAL_ACCESS, false},
		{planv1.EffectClass_DESTRUCTIVE, false},
		{planv1.EffectClass_INFRASTRUCTURE, false},
	}
	for _, tc := range cases {
		if got := effectclass.ReadsSensitive(tc.class); got != tc.want {
			t.Errorf("ReadsSensitive(%v) = %v, want %v", tc.class, got, tc.want)
		}
	}
}

// TestReadsSensitive_Unspecified verifies the unknown/UNSPECIFIED class is
// not sensitive (only the explicit READ_ONLY class is).
func TestReadsSensitive_Unspecified(t *testing.T) {
	if effectclass.ReadsSensitive(planv1.EffectClass_EFFECT_CLASS_UNSPECIFIED) {
		t.Error("ReadsSensitive(UNSPECIFIED) = true, want false")
	}
}
