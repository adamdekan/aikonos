package effectclass_test

import (
	"testing"

	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"

	"github.com/adamdekan/aikonos/broker/internal/effectclass"
)

// TestSeverityOrdering verifies the four-bucket ordering:
//
//	auto(read_only,write_local)=1 < approval(write_external,network_egress)=2
//	< step-up(credential_access,destructive,infrastructure)=3
//	< fail-closed(write_internal,unspecified/unknown)=4
func TestSeverityOrdering(t *testing.T) {
	cases := []struct {
		class planv1.EffectClass
		want  int
	}{
		{planv1.EffectClass_READ_ONLY, 1},
		{planv1.EffectClass_WRITE_LOCAL, 1},
		{planv1.EffectClass_WRITE_EXTERNAL, 2},
		{planv1.EffectClass_NETWORK_EGRESS, 2},
		{planv1.EffectClass_CREDENTIAL_ACCESS, 3},
		{planv1.EffectClass_DESTRUCTIVE, 3},
		{planv1.EffectClass_INFRASTRUCTURE, 3},
		// why: write_internal's all-false Routing means the engine's fail-closed
		// default denies it — severity 4 (strictest).
		{planv1.EffectClass_WRITE_INTERNAL, 4},
		// UNSPECIFIED is unknown → fail-closed bucket.
		{planv1.EffectClass_EFFECT_CLASS_UNSPECIFIED, 4},
	}
	for _, tc := range cases {
		got := effectclass.Severity(tc.class)
		if got != tc.want {
			t.Errorf("Severity(%v) = %d, want %d", tc.class, got, tc.want)
		}
	}
}

// TestStricterReturnsHigherSeverity verifies Stricter always returns the class
// with the higher (stricter) severity value.
func TestStricterReturnsHigherSeverity(t *testing.T) {
	cases := []struct {
		a, b planv1.EffectClass
		want planv1.EffectClass
	}{
		// declared weaker than authoritative → authoritative wins
		{planv1.EffectClass_READ_ONLY, planv1.EffectClass_WRITE_EXTERNAL, planv1.EffectClass_WRITE_EXTERNAL},
		// declared stronger than authoritative → declared wins
		{planv1.EffectClass_DESTRUCTIVE, planv1.EffectClass_READ_ONLY, planv1.EffectClass_DESTRUCTIVE},
		// equal severity same class → either (both equal)
		{planv1.EffectClass_READ_ONLY, planv1.EffectClass_WRITE_LOCAL, planv1.EffectClass_READ_ONLY}, // tie: first
		// write_internal vs any normal class → write_internal (fail-closed)
		{planv1.EffectClass_WRITE_EXTERNAL, planv1.EffectClass_WRITE_INTERNAL, planv1.EffectClass_WRITE_INTERNAL},
		// same class → same class
		{planv1.EffectClass_WRITE_EXTERNAL, planv1.EffectClass_WRITE_EXTERNAL, planv1.EffectClass_WRITE_EXTERNAL},
		// unspecified → fail-closed wins over any real class
		{planv1.EffectClass_READ_ONLY, planv1.EffectClass_EFFECT_CLASS_UNSPECIFIED, planv1.EffectClass_EFFECT_CLASS_UNSPECIFIED},
	}
	for _, tc := range cases {
		got := effectclass.Stricter(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("Stricter(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestStricterSymmetry verifies Stricter(a,b) severity == Stricter(b,a) severity
// (commutativity of the strictness decision).
func TestStricterSymmetry(t *testing.T) {
	classes := []planv1.EffectClass{
		planv1.EffectClass_READ_ONLY,
		planv1.EffectClass_WRITE_LOCAL,
		planv1.EffectClass_WRITE_INTERNAL,
		planv1.EffectClass_WRITE_EXTERNAL,
		planv1.EffectClass_NETWORK_EGRESS,
		planv1.EffectClass_CREDENTIAL_ACCESS,
		planv1.EffectClass_DESTRUCTIVE,
		planv1.EffectClass_INFRASTRUCTURE,
	}
	for _, a := range classes {
		for _, b := range classes {
			ab := effectclass.Stricter(a, b)
			ba := effectclass.Stricter(b, a)
			if effectclass.Severity(ab) != effectclass.Severity(ba) {
				t.Errorf("Stricter(%v,%v) severity %d != Stricter(%v,%v) severity %d",
					a, b, effectclass.Severity(ab), b, a, effectclass.Severity(ba))
			}
		}
	}
}
