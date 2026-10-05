package netacl

import "testing"

func r(kind ScopeKind, val string, act Action, host string) Rule {
	return Rule{ScopeKind: kind, ScopeValue: val, Action: act, HostPattern: host}
}

var alice = Principal{User: "alice@example.com", Groups: []string{"security-team"}}

func TestDecide_EmptyAllows(t *testing.T) {
	if got := Decide(nil, alice, "example.com"); got != Allow {
		t.Fatalf("no rules → Allow, got %s", got)
	}
}

func TestDecide_CatchAllDenyMakesAllowlist(t *testing.T) {
	rules := []Rule{
		r(ScopeTenant, "", Deny, "*"), // default-deny
		r(ScopeTenant, "", Allow, "*.githubusercontent.com"),
	}
	if got := Decide(rules, alice, "raw.githubusercontent.com"); got != Allow {
		t.Errorf("allowed host → Allow, got %s", got)
	}
	if got := Decide(rules, alice, "githubusercontent.com"); got != Allow {
		t.Errorf("*.suffix should also match the apex, got %s", got)
	}
	if got := Decide(rules, alice, "evil.com"); got != Deny {
		t.Errorf("unlisted host under default-deny → Deny, got %s", got)
	}
}

func TestDecide_HostSpecificityWins(t *testing.T) {
	rules := []Rule{
		r(ScopeTenant, "", Allow, "*.example.com"),
		r(ScopeTenant, "", Deny, "secret.example.com"), // more specific host
	}
	if got := Decide(rules, alice, "secret.example.com"); got != Deny {
		t.Errorf("exact host beats wildcard, got %s", got)
	}
	if got := Decide(rules, alice, "ok.example.com"); got != Allow {
		t.Errorf("wildcard applies elsewhere, got %s", got)
	}
}

func TestDecide_ScopeSpecificityWins(t *testing.T) {
	rules := []Rule{
		r(ScopeTenant, "", Deny, "api.x.com"),
		r(ScopeUser, "alice@example.com", Allow, "api.x.com"), // user beats tenant
	}
	if got := Decide(rules, alice, "api.x.com"); got != Allow {
		t.Errorf("user scope overrides tenant, got %s", got)
	}
	bob := Principal{User: "bob@example.com"}
	if got := Decide(rules, bob, "api.x.com"); got != Deny {
		t.Errorf("the user rule must not apply to bob, got %s", got)
	}
}

func TestDecide_GroupScope(t *testing.T) {
	rules := []Rule{
		r(ScopeTenant, "", Deny, "*"),
		r(ScopeGroup, "security-team", Allow, "siem.corp"),
	}
	if got := Decide(rules, alice, "siem.corp"); got != Allow {
		t.Errorf("group member allowed, got %s", got)
	}
	outsider := Principal{User: "carol@example.com", Groups: []string{"interns"}}
	if got := Decide(rules, outsider, "siem.corp"); got != Deny {
		t.Errorf("non-member falls to default-deny, got %s", got)
	}
}

func TestDecide_DenyWinsTie(t *testing.T) {
	// Same scope + same host specificity → most restrictive wins.
	rules := []Rule{
		r(ScopeTenant, "", Allow, "x.com"),
		r(ScopeTenant, "", Deny, "x.com"),
	}
	if got := Decide(rules, alice, "x.com"); got != Deny {
		t.Errorf("deny beats allow on a tie, got %s", got)
	}
}

func TestDecide_PortAndCaseNormalized(t *testing.T) {
	rules := []Rule{r(ScopeTenant, "", Deny, "*"), r(ScopeTenant, "", Allow, "API.Example.com")}
	if got := Decide(rules, alice, "api.example.com:443"); got != Allow {
		t.Errorf("case + port should normalize, got %s", got)
	}
}

func TestDecide_Ask(t *testing.T) {
	rules := []Rule{r(ScopeTenant, "", Ask, "*")}
	if got := Decide(rules, alice, "anything.com"); got != Ask {
		t.Errorf("catch-all ask, got %s", got)
	}
}

func TestDecideAnyGroups_StrictestMembershipWins(t *testing.T) {
	rules := []Rule{
		r(ScopeTenant, "", Deny, "*"),
		r(ScopeTenant, "", Allow, "*.corp"),
		r(ScopeGroup, "security-team", Allow, "siem.corp"),
		r(ScopeGroup, "interns", Deny, "*.corp"),
	}
	cases := []struct {
		host string
		want Action
	}{
		// Allowed for the tenant and for security-team, but a user only in
		// interns would be denied: unknown membership denies.
		{"siem.corp", Deny},
		{"wiki.corp", Deny},
		// No group rule matches: the tenant rules decide.
		{"elsewhere.example", Deny},
	}
	for _, c := range cases {
		if got := DecideAnyGroups(rules, "carol@example.com", c.host); got != c.want {
			t.Errorf("%s: want %s, got %s", c.host, c.want, got)
		}
	}
	// With the interns rule gone, the tenant ALLOW for *.corp stands.
	if got := DecideAnyGroups(rules[:3], "carol@example.com", "wiki.corp"); got != Allow {
		t.Errorf("wiki.corp without the interns rule: want ALLOW, got %s", got)
	}
}

func TestDecideAnyGroups_GroupAskNotLost(t *testing.T) {
	rules := []Rule{r(ScopeGroup, "finance", Ask, "ledger.example")}
	if got := Decide(rules, Principal{User: "carol@example.com"}, "ledger.example"); got != Allow {
		t.Fatalf("precondition: without groups the ASK does not apply, got %s", got)
	}
	if got := DecideAnyGroups(rules, "carol@example.com", "ledger.example"); got != Ask {
		t.Errorf("unknown membership must keep the group's ASK, got %s", got)
	}
	if got := DecideAnyGroups(rules, "carol@example.com", "other.example"); got != Allow {
		t.Errorf("a host no rule matches stays ALLOW, got %s", got)
	}
}

func TestDecideAnyGroups_UserRuleStillWins(t *testing.T) {
	rules := []Rule{
		r(ScopeGroup, "interns", Deny, "*"),
		r(ScopeUser, "carol@example.com", Allow, "docs.example"),
	}
	if got := DecideAnyGroups(rules, "carol@example.com", "docs.example"); got != Allow {
		t.Errorf("a USER rule outranks every group rule, got %s", got)
	}
	if got := DecideAnyGroups(rules, "carol@example.com", "elsewhere.example"); got != Deny {
		t.Errorf("without a USER rule the group DENY applies, got %s", got)
	}
}
