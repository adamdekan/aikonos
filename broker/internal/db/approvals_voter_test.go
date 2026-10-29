package db

import "testing"

// MayVote: an empty set admits nobody (M-11); a non-empty set admits its
// members; the requester of an n >= 2 gate never votes (AG-07).
func TestMayVote_EmptySet_FailsClosed(t *testing.T) {
	if MayVote(nil, "owner", 2, "anyone@example.com") || MayVote([]string{}, "owner", 1, "anyone@example.com") {
		t.Error("an empty approverSet must admit nobody")
	}
}

func TestMayVote_Membership(t *testing.T) {
	set := []string{"alice@example.com", "bob@example.com"}
	if !MayVote(set, "owner", 2, "alice@example.com") || !MayVote(set, "owner", 2, "bob@example.com") {
		t.Error("members must be allowed")
	}
	if MayVote(set, "owner", 2, "eve@example.com") {
		t.Error("eve is not in the set and must be rejected")
	}
}

func TestMayVote_SeparationOfDuty(t *testing.T) {
	if MayVote([]string{"owner", "other"}, "owner", 2, "owner") {
		t.Error("requester voted on an n=2 gate")
	}
	if !MayVote([]string{"owner"}, "owner", 1, "owner") {
		t.Error("n=1: the requester in the set may vote")
	}
}
