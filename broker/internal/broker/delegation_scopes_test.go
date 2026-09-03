package broker

import (
	"testing"
)

// ── CP1 (F24 "implement for real"): senderDelegableScopes ──────────────────
//
//  CP1: with FGA enabled, delegable scopes
// are derived from ListObjects(can_invoke, skill) mapped through
// ToolRegistry.RequiredScope; with FGA disabled the hardcoded dev-stub list
// stands in; a ListObjects error fails closed (returns an error, never the
// stub).

func TestSenderDelegableScopes_FGADisabled_ReturnsStubList(t *testing.T) {
	// No FGA server at all: testEnvelopeDeps("") leaves OpenFGAStoreID unset,
	// so FGAEnabled() is false — mirrors real dev-stack (unseeded) behavior.
	svc := NewBrokerService(testEnvelopeDeps(t, "", ""))

	scopes, source, err := svc.senderDelegableScopes(ctxWithIdentity(testTenantUUID, "alice@example.com"), "alice@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if source != "dev-stub" {
		t.Errorf("source = %q, want %q", source, "dev-stub")
	}
	if len(scopes) != len(stubDelegationScopes) {
		t.Errorf("scopes = %v, want stub list %v", scopes, stubDelegationScopes)
	}
}

func TestSenderDelegableScopes_FGAEnabled_DerivesFromGrantedSkills(t *testing.T) {
	f := &fakeFGA{
		listObjectsResultByUser: map[string][]string{
			// mcp: and capability-skill ids have no RequiredScope entry and
			// must be silently skipped; doc.write and siem.query duplicate
			// scope isn't possible here but distinct+sorted is still checked
			// via slack.post + doc.write + siem.query → 3 distinct scopes.
			"user:alice@example.com": {
				"skill:siem.query",
				"skill:slack.post",
				"skill:doc.write",
				// Connector-level MCP grants have no per-tool RequiredScope
				// entry (only the fully-qualified "mcp:<conn>:<tool>" form
				// resolves via the prefix rule, and that shape is never a
				// valid can_invoke/skill FGA object — service_plan.go notes
				// the colon-bearing id fails FGA object-id syntax).
				"skill:mcp:github",
				"skill:workflows",
				"skill:not-a-real-tool",
			},
		},
	}
	fgaSrv := f.server(t)
	defer fgaSrv.Close()

	deps := testEnvelopeDeps(t, fgaSrv.URL, "")
	deps.ToolRegistry = newTestToolRegistry()
	svc := NewBrokerService(deps)

	scopes, source, err := svc.senderDelegableScopes(ctxWithIdentity(testTenantUUID, "alice@example.com"), "alice@example.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if source != "fga-derived" {
		t.Errorf("source = %q, want %q", source, "fga-derived")
	}
	want := []string{"doc:write", "siem:read", "slack:write"}
	if len(scopes) != len(want) {
		t.Fatalf("scopes = %v, want %v", scopes, want)
	}
	for i, w := range want {
		if scopes[i] != w {
			t.Errorf("scopes[%d] = %q, want %q (full: %v)", i, scopes[i], w, scopes)
		}
	}
}

func TestSenderDelegableScopes_ListObjectsError_FailsClosed(t *testing.T) {
	f := &fakeFGA{listObjectsErr: true}
	fgaSrv := f.server(t)
	defer fgaSrv.Close()

	deps := testEnvelopeDeps(t, fgaSrv.URL, "")
	deps.ToolRegistry = newTestToolRegistry()
	svc := NewBrokerService(deps)

	scopes, source, err := svc.senderDelegableScopes(ctxWithIdentity(testTenantUUID, "alice@example.com"), "alice@example.com")
	if err == nil {
		t.Fatal("expected error on ListObjects failure, got nil")
	}
	if scopes != nil || source != "" {
		t.Errorf("on error want zero values, got scopes=%v source=%q", scopes, source)
	}
}

func TestSenderDelegableScopes_NilToolRegistry_FGAEnabled_ReturnsError(t *testing.T) {
	f := &fakeFGA{listObjectsResultByUser: map[string][]string{"user:alice@example.com": {"skill:siem.query"}}}
	fgaSrv := f.server(t)
	defer fgaSrv.Close()

	deps := testEnvelopeDeps(t, fgaSrv.URL, "")
	// testEnvelopeDeps defaults ToolRegistry to a real Registry; explicitly
	// nil it out here — misconfiguration must fail closed rather than
	// silently derive an empty scope set.
	deps.ToolRegistry = nil
	svc := NewBrokerService(deps)

	_, _, err := svc.senderDelegableScopes(ctxWithIdentity(testTenantUUID, "alice@example.com"), "alice@example.com")
	if err == nil {
		t.Fatal("expected error when ToolRegistry is nil with FGA enabled, got nil")
	}
}
