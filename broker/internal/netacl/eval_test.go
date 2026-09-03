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
