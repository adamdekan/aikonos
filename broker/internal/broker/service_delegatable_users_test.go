package broker

import (
	"testing"

	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// memberTuple is the canonical group-membership shape: group:X#member@user:Y.
func memberTuple(user, group string) fgaKey {
	return fgaKey{User: "user:" + user, Relation: "member", Object: group}
}

func userIDsOf(resp *brokerv1.ListDelegatableUsersResponse) []string {
	out := make([]string, 0, len(resp.GetUsers()))
	for _, u := range resp.GetUsers() {
		out = append(out, u.GetUserId())
	}
	return out
}

func callDelegatable(t *testing.T, f *fakeFGA) *brokerv1.ListDelegatableUsersResponse {
	t.Helper()
	fgaSrv := f.server(t)
	defer fgaSrv.Close()
	opaSrv := opaAlwaysAllow(t)
	defer opaSrv.Close()

	svc := NewBrokerService(testEnvelopeDeps(t, fgaSrv.URL, opaSrv.URL))
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")
	resp, err := svc.ListDelegatableUsers(ctx, &brokerv1.ListDelegatableUsersRequest{
		TenantId: testTenantUUID,
		UserId:   "alice@example.com",
	})
	if err != nil {
		t.Fatalf("ListDelegatableUsers returned error: %v", err)
	}
	return resp
}

// The caller's delegatable-group peers are the valid delegation targets — exactly
// the set SendEnvelope's sharesDelegatableGroup gate would later admit.
func TestListDelegatableUsers_SharedGroupMembers(t *testing.T) {
	f := &fakeFGA{
		listObjectsResultByUser: map[string][]string{
			"user:alice@example.com": {"group:security-team"},
		},
		reads: map[string][]fgaKey{
			"group:security-team": {
				memberTuple("alice@example.com", "group:security-team"),
				memberTuple("bob@example.com", "group:security-team"),
				memberTuple("carol@example.com", "group:security-team"),
			},
		},
	}
	got := userIDsOf(callDelegatable(t, f))
	// alice (the caller) must never appear; result is sorted for stable UI/tests.
	want := []string{"bob@example.com", "carol@example.com"}
	if !equalStrings(got, want) {
		t.Fatalf("delegatable users = %v, want %v", got, want)
	}
}

// Display name falls back to the user id when no UserDirectory is wired (the unit
// path) — the popover must still render something selectable.
func TestListDelegatableUsers_DisplayNameFallsBackToUserID(t *testing.T) {
	f := &fakeFGA{
		listObjectsResultByUser: map[string][]string{
			"user:alice@example.com": {"group:security-team"},
		},
		reads: map[string][]fgaKey{
			"group:security-team": {memberTuple("bob@example.com", "group:security-team")},
		},
	}
	resp := callDelegatable(t, f)
	if len(resp.GetUsers()) != 1 || resp.GetUsers()[0].GetDisplayName() != "bob@example.com" {
		t.Fatalf("display name fallback wrong: %+v", resp.GetUsers())
	}
}

// No delegatable group → no peers. The composer shows an empty palette, never an error.
func TestListDelegatableUsers_NoDelegatableGroups(t *testing.T) {
	f := &fakeFGA{listObjectsResultByUser: map[string][]string{"user:alice@example.com": {}}}
	if got := userIDsOf(callDelegatable(t, f)); len(got) != 0 {
		t.Fatalf("no groups should yield empty, got %v", got)
	}
}

// FGA transport error must fail closed (empty list) and never surface an error to
// the caller — a broken directory must not break the chat composer.
func TestListDelegatableUsers_FGAErrorFailsClosed(t *testing.T) {
	f := &fakeFGA{listObjectsErr: true}
	if got := userIDsOf(callDelegatable(t, f)); len(got) != 0 {
		t.Fatalf("FGA error should fail closed to empty, got %v", got)
	}
}

// A peer shared via two groups appears once — dedup across the caller's groups.
func TestListDelegatableUsers_DedupAcrossGroups(t *testing.T) {
	f := &fakeFGA{
		listObjectsResultByUser: map[string][]string{
			"user:alice@example.com": {"group:a", "group:b"},
		},
		reads: map[string][]fgaKey{
			"group:a": {memberTuple("bob@example.com", "group:a")},
			"group:b": {memberTuple("bob@example.com", "group:b")},
		},
	}
	want := []string{"bob@example.com"}
	if got := userIDsOf(callDelegatable(t, f)); !equalStrings(got, want) {
		t.Fatalf("dedup failed: got %v, want %v", got, want)
	}
}

// Non-user subjects (nested group memberships) are not delegation targets and must
// be filtered out — only user: subjects are real people to delegate to.
func TestListDelegatableUsers_IgnoresNonUserSubjects(t *testing.T) {
	f := &fakeFGA{
		listObjectsResultByUser: map[string][]string{
			"user:alice@example.com": {"group:security-team"},
		},
		reads: map[string][]fgaKey{
			"group:security-team": {
				memberTuple("bob@example.com", "group:security-team"),
				{User: "group:subteam#member", Relation: "member", Object: "group:security-team"},
			},
		},
	}
	want := []string{"bob@example.com"}
	if got := userIDsOf(callDelegatable(t, f)); !equalStrings(got, want) {
		t.Fatalf("non-user subject leaked: got %v, want %v", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ── CP1: group discovery ──────────────────────────────────────────────────────

// groupIDsOf extracts group_id values from the response in their returned order.
func groupIDsOf(resp *brokerv1.ListDelegatableUsersResponse) []string {
	out := make([]string, 0, len(resp.GetGroups()))
	for _, g := range resp.GetGroups() {
		out = append(out, g.GetGroupId())
	}
	return out
}

// Caller in a group with two other members → one DelegatableGroup with member_count=2.
func TestListDelegatableUsers_GroupsReturnedWithMemberCount(t *testing.T) {
	f := &fakeFGA{
		listObjectsResultByUser: map[string][]string{
			"user:alice@example.com": {"group:security-team"},
		},
		reads: map[string][]fgaKey{
			"group:security-team": {
				memberTuple("alice@example.com", "group:security-team"),
				memberTuple("bob@example.com", "group:security-team"),
				memberTuple("carol@example.com", "group:security-team"),
			},
		},
	}
	resp := callDelegatable(t, f)
	groups := resp.GetGroups()
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d: %v", len(groups), groups)
	}
	g := groups[0]
	if g.GetGroupId() != "group:security-team" {
		t.Errorf("group_id = %q, want %q", g.GetGroupId(), "group:security-team")
	}
	// member_count excludes alice (the caller)
	if g.GetMemberCount() != 2 {
		t.Errorf("member_count = %d, want 2", g.GetMemberCount())
	}
	// display_name falls back to group_id when no directory is wired
	if g.GetDisplayName() != "group:security-team" {
		t.Errorf("display_name = %q, want %q", g.GetDisplayName(), "group:security-team")
	}
}

// Caller in no delegatable group → empty groups slice.
func TestListDelegatableUsers_NoGroupsWhenCallerInNone(t *testing.T) {
	f := &fakeFGA{listObjectsResultByUser: map[string][]string{"user:alice@example.com": {}}}
	resp := callDelegatable(t, f)
	if len(resp.GetGroups()) != 0 {
		t.Fatalf("expected 0 groups, got %v", resp.GetGroups())
	}
}

// A group where alice is the only member → omitted (zero other members).
func TestListDelegatableUsers_GroupWithOnlyCallerOmitted(t *testing.T) {
	f := &fakeFGA{
		listObjectsResultByUser: map[string][]string{
			"user:alice@example.com": {"group:solo"},
		},
		reads: map[string][]fgaKey{
			"group:solo": {
				memberTuple("alice@example.com", "group:solo"),
			},
		},
	}
	resp := callDelegatable(t, f)
	if len(resp.GetGroups()) != 0 {
		t.Fatalf("solo group should be omitted, got %v", resp.GetGroups())
	}
}

// FGA error path → empty groups (fail closed), no error returned to caller.
func TestListDelegatableUsers_FGAErrorFailsClosedForGroups(t *testing.T) {
	f := &fakeFGA{listObjectsErr: true}
	resp := callDelegatable(t, f)
	if len(resp.GetGroups()) != 0 {
		t.Fatalf("FGA error should fail closed, got %v", resp.GetGroups())
	}
}

// Multiple groups are sorted by group_id for deterministic output.
func TestListDelegatableUsers_GroupsSortedByGroupID(t *testing.T) {
	f := &fakeFGA{
		listObjectsResultByUser: map[string][]string{
			"user:alice@example.com": {"group:z-team", "group:a-team"},
		},
		reads: map[string][]fgaKey{
			"group:z-team": {
				memberTuple("alice@example.com", "group:z-team"),
				memberTuple("bob@example.com", "group:z-team"),
			},
			"group:a-team": {
				memberTuple("alice@example.com", "group:a-team"),
				memberTuple("carol@example.com", "group:a-team"),
			},
		},
	}
	resp := callDelegatable(t, f)
	got := groupIDsOf(resp)
	want := []string{"group:a-team", "group:z-team"}
	if !equalStrings(got, want) {
		t.Fatalf("groups sorted wrong: got %v, want %v", got, want)
	}
}
