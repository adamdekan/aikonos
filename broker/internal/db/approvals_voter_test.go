package db

import "testing"

// TestVoterAllowed covers the three invariants of voterAllowed:
//   - empty set → legacy allow (any voter satisfies the gate)
//   - non-empty set, voter is a member → allowed
//   - non-empty set, voter is not a member → rejected
func TestVoterAllowed_EmptySet_LegacyAllow(t *testing.T) {
	if !voterAllowed(nil, "anyone@example.com") {
		t.Error("empty approverSet must allow any voter (legacy rows)")
	}
	if !voterAllowed([]string{}, "anyone@example.com") {
		t.Error("empty approverSet must allow any voter (legacy rows)")
	}
}

func TestVoterAllowed_Member_Allowed(t *testing.T) {
	set := []string{"alice@example.com", "bob@example.com"}
	if !voterAllowed(set, "alice@example.com") {
		t.Error("alice is in the set and must be allowed")
	}
	if !voterAllowed(set, "bob@example.com") {
		t.Error("bob is in the set and must be allowed")
	}
}

func TestVoterAllowed_NonMember_Rejected(t *testing.T) {
	set := []string{"alice@example.com", "bob@example.com"}
	if voterAllowed(set, "eve@example.com") {
		t.Error("eve is not in the set and must be rejected")
	}
}
