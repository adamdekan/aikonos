package broker

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

func netRule(scope, val, action, host string) *brokerv1.NetworkRule {
	return &brokerv1.NetworkRule{ScopeKind: scope, ScopeValue: val, Action: action, HostPattern: host}
}

// With no Network repo wired, the RPCs fail closed (FailedPrecondition) — used to
// confirm the admin gate runs before the disabled check is irrelevant; here we
// assert the disabled path. (DB-backed happy paths run in-cluster.)
func TestNetworkRules_DisabledFailsClosed(t *testing.T) {
	svc := NewBrokerService(testAdminDeps(t, "")) // no Network repo
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	if _, err := svc.ListNetworkRules(ctx, &brokerv1.ListNetworkRulesRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("disabled list want FailedPrecondition, got %v", err)
	}
}

func TestValidateNetworkRule(t *testing.T) {
	ok := []*brokerv1.NetworkRule{
		netRule("TENANT", "", "DENY", "*"),
		netRule("TENANT", "", "ALLOW", "*.githubusercontent.com"),
		netRule("USER", "alice@example.com", "ALLOW", "api.x.com"),
		netRule("GROUP", "security-team", "ASK", "siem.corp"),
	}
	for _, r := range ok {
		if _, _, err := validateNetworkRule(r); err != nil {
			t.Errorf("expected %+v valid: %v", r, err)
		}
	}
	bad := []*brokerv1.NetworkRule{
		netRule("PLANET", "", "ALLOW", "x.com"),          // bad scope
		netRule("TENANT", "", "MAYBE", "x.com"),          // bad action
		netRule("USER", "", "ALLOW", "x.com"),            // user scope needs a value
		netRule("TENANT", "", "ALLOW", "https://x.com"),  // scheme not allowed
		netRule("TENANT", "", "ALLOW", "x.com/path"),     // path not allowed
		netRule("TENANT", "", "ALLOW", "a*b.com"),        // bad wildcard
		netRule("TENANT", "", "ALLOW", "with space.com"), // whitespace
		netRule("TENANT", "", "ALLOW", ""),               // empty
	}
	for _, r := range bad {
		if _, _, err := validateNetworkRule(r); status.Code(err) != codes.InvalidArgument {
			t.Errorf("expected %+v invalid, got %v", r, err)
		}
	}
}
