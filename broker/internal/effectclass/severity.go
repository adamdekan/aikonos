package effectclass

import planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"

// severityTable maps each effect class to an integer severity bucket for
// routing-posture comparison.  Higher = stricter = more restricted.
//
// Buckets:
//
//	1 — auto (read_only, write_local): no human in the loop
//	2 — approval (write_external, network_egress): HITL required
//	3 — step-up (credential_access, destructive, infrastructure): HITL + step-up
//	4 — fail-closed (write_internal, unspecified/unknown): engine denies by default
//
// The invariant is monotonic-stricter: routing may only escalate, never relax.
var severityTable = map[planv1.EffectClass]int{
	planv1.EffectClass_READ_ONLY:         1,
	planv1.EffectClass_WRITE_LOCAL:       1,
	planv1.EffectClass_WRITE_EXTERNAL:    2,
	planv1.EffectClass_NETWORK_EGRESS:    2,
	planv1.EffectClass_CREDENTIAL_ACCESS: 3,
	planv1.EffectClass_DESTRUCTIVE:       3,
	planv1.EffectClass_INFRASTRUCTURE:    3,
	// write_internal's all-false Routing makes the engine fail closed (deny).
	// Severity 4 ensures Stricter always preserves the deny posture.
	planv1.EffectClass_WRITE_INTERNAL: 4,
}

// Severity returns the routing-posture severity of ec (higher = stricter).
// UNSPECIFIED and any unknown value return 4 (fail-closed) so unknown classes
// never route weaker than known ones.
func Severity(ec planv1.EffectClass) int {
	if s, ok := severityTable[ec]; ok {
		return s
	}
	return 4 // unknown → fail-closed
}

// Stricter returns whichever of a or b has the higher severity (stricter routing
// posture).  When severities are equal, a is returned (tie-keeps-declared).
// This is the single enforcement point for the monotonic-stricter invariant:
// routing may escalate (declared < authoritative → use authoritative) but never
// relax (declared ≥ authoritative → keep declared).
func Stricter(a, b planv1.EffectClass) planv1.EffectClass {
	if Severity(b) > Severity(a) {
		return b
	}
	return a
}
