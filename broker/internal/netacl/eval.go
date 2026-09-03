// Package netacl evaluates the agent web access-list: given a tenant's host
// rules and the acting principal, decide whether a destination host is ALLOW,
// DENY, or ASK. The logic is pure and tool-agnostic; the Tool Proxy web.fetch
// handler and SubmitPlan both call it.
//
// Precedence: the most specific matching rule wins — most specific SCOPE
// (USER > GROUP > TENANT), then most specific HOST (exact > "*.suffix" > "*").
// On a tie (same scope + host specificity) the most restrictive action wins
// (DENY > ASK > ALLOW). With no matching rule the result is ALLOW — the
// per-tenant catch-all "*" rule is what turns the list into default-deny.
package netacl

import "strings"

type Action string

const (
	Allow Action = "ALLOW"
	Deny  Action = "DENY"
	Ask   Action = "ASK"
)

type ScopeKind string

const (
	ScopeTenant ScopeKind = "TENANT"
	ScopeGroup  ScopeKind = "GROUP"
	ScopeUser   ScopeKind = "USER"
)

// Rule is a single host rule (db.NetworkRule mapped into the evaluator's terms).
type Rule struct {
	ScopeKind   ScopeKind
	ScopeValue  string // group name / user email / "" for tenant
	Action      Action
	HostPattern string
}

// Principal is who the fetch acts as.
type Principal struct {
	User   string
	Groups []string
}

func normalizeHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if i := strings.LastIndexByte(host, ':'); i >= 0 && !strings.Contains(host[i:], "]") {
		host = host[:i] // strip :port
	}
	return strings.TrimSuffix(host, ".")
}

func matchHost(pattern, host string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if pattern == "*" {
		return true
	}
	if suffix, ok := strings.CutPrefix(pattern, "*."); ok {
		return host == suffix || strings.HasSuffix(host, "."+suffix)
	}
	return pattern == host
}

func hostRank(pattern string) int {
	switch {
	case pattern == "*":
		return 1
	case strings.HasPrefix(pattern, "*."):
		return 2
	default:
		return 3
	}
}

func scopeRank(k ScopeKind) int {
	switch k {
	case ScopeUser:
		return 3
	case ScopeGroup:
		return 2
	default:
		return 1
	}
}

func actionRank(a Action) int {
	switch a {
	case Deny:
		return 3
	case Ask:
		return 2
	default:
		return 1
	}
}

func scopeApplies(r Rule, p Principal) bool {
	switch r.ScopeKind {
	case ScopeTenant:
		return true
	case ScopeUser:
		return strings.EqualFold(r.ScopeValue, p.User)
	case ScopeGroup:
		for _, g := range p.Groups {
			if strings.EqualFold(g, r.ScopeValue) {
				return true
			}
		}
	}
	return false
}

// Decide resolves the effective action for host under the given rules. Unmatched
// → Allow.
func Decide(rules []Rule, p Principal, host string) Action {
	host = normalizeHost(host)
	best := -1
	winner := Allow
	matched := false
	for _, r := range rules {
		if !scopeApplies(r, p) || !matchHost(r.HostPattern, host) {
			continue
		}
		score := scopeRank(r.ScopeKind)*10 + hostRank(r.HostPattern)
		switch {
		case score > best:
			best = score
			winner = r.Action
			matched = true
		case score == best && actionRank(r.Action) > actionRank(winner):
			winner = r.Action
		}
	}
	if !matched {
		return Allow
	}
	return winner
}
