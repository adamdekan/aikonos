// broker/internal/provisioning/validate.go
// Input validation and rule-matching for email-based JIT provisioning.
package provisioning

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/adamdekan/aikonos/broker/internal/db"
)

// exactRe matches a valid, lowercase exact email address.
var exactRe = regexp.MustCompile(`^[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}$`)

// wildcardRe matches a valid, lowercase domain wildcard (*@domain.tld).
var wildcardRe = regexp.MustCompile(`^\*@[a-z0-9.\-]+\.[a-z]{2,}$`)

// groupRe matches a valid group name: starts with [a-z0-9], then [a-z0-9._-]{0,63}.
var groupRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._\-]{0,63}$`)

// NormalizeMatcher lowercases the input and validates it as either an exact
// email address or a domain wildcard (*@domain.tld).
// Bare "*" and locals starting with "svc-" are rejected.
func NormalizeMatcher(s string) (string, error) {
	lower := strings.ToLower(strings.TrimSpace(s))
	if lower == "" {
		return "", fmt.Errorf("matcher must not be empty")
	}
	if lower == "*" {
		return "", fmt.Errorf("bare '*' is not a valid matcher; use '*@domain.tld'")
	}
	if wildcardRe.MatchString(lower) {
		return lower, nil
	}
	if exactRe.MatchString(lower) {
		// Reject service-principal email locals.
		local, _, _ := strings.Cut(lower, "@")
		if strings.HasPrefix(local, "svc-") {
			return "", fmt.Errorf("matcher local %q is reserved for service principals", local)
		}
		return lower, nil
	}
	return "", fmt.Errorf("matcher %q is not a valid email address or '*@domain.tld' wildcard", s)
}

// ValidateGroups checks that groups is a non-empty list (1–20 entries) where
// each entry matches ^[a-z0-9][a-z0-9._-]{0,63}$ (no "group:" prefix, no
// wildcards, no "#" userset suffixes, no uppercase).
func ValidateGroups(groups []string) error {
	if len(groups) == 0 {
		return fmt.Errorf("at least one group is required")
	}
	if len(groups) > 20 {
		return fmt.Errorf("at most 20 groups allowed; got %d", len(groups))
	}
	for _, g := range groups {
		if !groupRe.MatchString(g) {
			return fmt.Errorf("invalid group name %q: must match ^[a-z0-9][a-z0-9._-]{0,63}$", g)
		}
	}
	return nil
}

// MatchRules returns the union of groups from all rules that match email,
// applying exact rules first then wildcard (*@domain) rules. The result is
// deduped and sorted for deterministic ordering. Empty slice (not nil) when
// no rules match.
func MatchRules(email string, rules []db.ProvisioningRule) []string {
	lower := strings.ToLower(email)
	groupSet := make(map[string]struct{})

	// Exact rules.
	for _, r := range rules {
		if r.Matcher == lower {
			for _, g := range r.Groups {
				groupSet[g] = struct{}{}
			}
		}
	}

	// Domain wildcard rules.
	_, domain, hasDomain := strings.Cut(lower, "@")
	if hasDomain {
		wildcard := "*@" + domain
		for _, r := range rules {
			if r.Matcher == wildcard {
				for _, g := range r.Groups {
					groupSet[g] = struct{}{}
				}
			}
		}
	}

	out := make([]string, 0, len(groupSet))
	for g := range groupSet {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}
