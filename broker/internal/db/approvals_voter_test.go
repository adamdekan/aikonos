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

// TestMayVote_SeparationOfDuty: the requester of an n >= 2 gate never votes,
// even when the set is empty (which admits anyone else); n = 1 keeps the
// plain voterAllowed rule (AG-07).
func TestMayVote_SeparationOfDuty(t *testing.T) {
	if MayVote(nil, "owner", 2, "owner") {
		t.Error("requester voted on an n=2 gate with an empty set")
	}
	if !MayVote(nil, "owner", 2, "other") {
		t.Error("empty set must admit a non-requester")
	}
	if !MayVote([]string{"owner"}, "owner", 1, "owner") {
		t.Error("n=1: the requester in the set may vote")
	}
}
