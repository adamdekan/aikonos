package broker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// fakeEnvelopeStore satisfies taskStore (CreateEnvelope only; the rest via
// the embedded stub) for injection via Deps.Tasks: it returns a fresh id so
// the SendEnvelope allow path completes without Postgres.
type fakeEnvelopeStore struct {
	stubTaskStore
}

func (fakeEnvelopeStore) CreateEnvelope(context.Context, *db.Envelope, db.EnvelopeState) (uuid.UUID, error) {
	return uuid.New(), nil
}

// ── sharedAny unit tests ────────────────────────────────────────────────────

func TestSharedAny(t *testing.T) {
	cases := []struct {
		name string
		a, b []string
		want bool
	}{
		{"both empty", nil, nil, false},
		{"a empty", nil, []string{"group:x"}, false},
		{"b empty", []string{"group:x"}, nil, false},
		{"disjoint", []string{"group:a"}, []string{"group:b"}, false},
		{"overlap one", []string{"group:a", "group:b"}, []string{"group:b", "group:c"}, true},
		{"overlap duplicates in a", []string{"group:a", "group:a"}, []string{"group:a"}, true},
		{"exact match single", []string{"group:security-team"}, []string{"group:security-team"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sharedAny(tc.a, tc.b); got != tc.want {
				t.Errorf("sharedAny(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// ── SendEnvelope self-deny ──────────────────────────────────────────────────

func TestSendEnvelope_SelfDenyBeforeFGA(t *testing.T) {
	// Self-delegation must be rejected before any FGA/OPA call.
	// listObjectsErr=true would cause an HTTP 500 if ListObjects were called —
	// the self-deny guard must fire first, so we never reach it.
	f := &fakeFGA{
		admins:         map[string]bool{},
		listObjectsErr: true,
	}
	fgaSrv := f.server(t)
	defer fgaSrv.Close()

	opaSrv := opaAlwaysAllow(t)
	defer opaSrv.Close()

	svc := NewBrokerService(testEnvelopeDeps(t, fgaSrv.URL, opaSrv.URL))
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	_, err := svc.SendEnvelope(ctx, &brokerv1.SendEnvelopeRequest{
		TenantId:   testTenantUUID,
		FromUserId: "alice@example.com",
		Recipient:  &brokerv1.EnvelopeRecipient{Target: &brokerv1.EnvelopeRecipient_UserId{UserId: "alice@example.com"}},
		Task:       &brokerv1.EnvelopeTask{Intent: "do something"},
		Delegation: &brokerv1.EnvelopeDelegation{},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("self-delegation: want PermissionDenied, got %v", err)
	}
	if status.Convert(err).Message() != "self-delegation not permitted" {
		t.Errorf("self-deny message = %q, want %q", status.Convert(err).Message(), "self-delegation not permitted")
	}
}

// ── SendEnvelope delegation-group fallback ─────────────────────────────────

func TestSendEnvelope_DelegationGroupFallback_Allow(t *testing.T) {
	// CheckFGA(can_delegate_to_user) → false.
	// Both users share group:security-team via delegatable_member → allow.
	// OPA stub: allows when fga_send_decision=allow.
	f := &fakeFGA{
		checks: map[string]bool{
			"user:alice@example.com|can_delegate_to_user|user:bob@example.com": false,
		},
		listObjectsResultByUser: map[string][]string{
			"user:alice@example.com": {"group:security-team"},
			"user:bob@example.com":   {"group:security-team"},
		},
	}
	fgaSrv := f.server(t)
	defer fgaSrv.Close()

	opaSrv := opaEchoFGADecision(t)
	defer opaSrv.Close()

	// Inject a fake envelope store so the allow path persists without Postgres.
	deps := testEnvelopeDeps(t, fgaSrv.URL, opaSrv.URL)
	deps.Tasks = &fakeEnvelopeStore{}
	svc := NewBrokerService(deps)
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	h, err := svc.SendEnvelope(ctx, &brokerv1.SendEnvelopeRequest{
		TenantId:   testTenantUUID,
		FromUserId: "alice@example.com",
		Recipient:  &brokerv1.EnvelopeRecipient{Target: &brokerv1.EnvelopeRecipient_UserId{UserId: "bob@example.com"}},
		Task:       &brokerv1.EnvelopeTask{Intent: "delegate this"},
		Delegation: &brokerv1.EnvelopeDelegation{},
	})
	if err != nil {
		t.Fatalf("shared delegatable group should allow the send, got %v", err)
	}
	if h == nil || h.EnvelopeId == "" {
		t.Fatalf("allow path returned no envelope handle: %+v", h)
	}
}

func TestSendEnvelope_DelegationGroupFallback_Deny_Disjoint(t *testing.T) {
	// CheckFGA denies. Users share no delegatable group → fgaDecision stays "deny"
	// → OPA receives deny → returns allow=false → PermissionDenied.
	f := &fakeFGA{
		checks: map[string]bool{
			"user:alice@example.com|can_delegate_to_user|user:bob@example.com": false,
		},
		listObjectsResultByUser: map[string][]string{
			"user:alice@example.com": {"group:security-team"},
			"user:bob@example.com":   {"group:other-team"},
		},
	}
	fgaSrv := f.server(t)
	defer fgaSrv.Close()

	opaSrv := opaEchoFGADecision(t)
	defer opaSrv.Close()

	svc := NewBrokerService(testEnvelopeDeps(t, fgaSrv.URL, opaSrv.URL))
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	_, err := svc.SendEnvelope(ctx, &brokerv1.SendEnvelopeRequest{
		TenantId:   testTenantUUID,
		FromUserId: "alice@example.com",
		Recipient:  &brokerv1.EnvelopeRecipient{Target: &brokerv1.EnvelopeRecipient_UserId{UserId: "bob@example.com"}},
		Task:       &brokerv1.EnvelopeTask{Intent: "delegate this"},
		Delegation: &brokerv1.EnvelopeDelegation{},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("disjoint groups: want PermissionDenied, got %v", err)
	}
	if status.Convert(err).Message() == "self-delegation not permitted" {
		t.Error("hit self-deny path; should have been FGA/OPA deny")
	}
}

func TestSendEnvelope_DelegationGroupFallback_FailClosed_OnListError(t *testing.T) {
	// A ListObjects HTTP 500 fails closed regardless of which caller hits it
	// first. Since F24 CP1, senderDelegableScopes resolves the sender's real
	// scopes via ListObjects(can_invoke, skill) before the delegatable-group
	// fallback's own ListObjects(delegatable_member, group) call ever runs —
	// so with fakeFGA erroring on every list-objects request, the deny now
	// surfaces as the scope-resolution Internal error rather than the
	// group-fallback's PermissionDenied. Either way: fail closed, no envelope
	// created.
	f := &fakeFGA{
		checks: map[string]bool{
			"user:alice@example.com|can_delegate_to_user|user:bob@example.com": false,
		},
		listObjectsErr: true,
	}
	fgaSrv := f.server(t)
	defer fgaSrv.Close()

	opaSrv := opaEchoFGADecision(t)
	defer opaSrv.Close()

	svc := NewBrokerService(testEnvelopeDeps(t, fgaSrv.URL, opaSrv.URL))
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	_, err := svc.SendEnvelope(ctx, &brokerv1.SendEnvelopeRequest{
		TenantId:   testTenantUUID,
		FromUserId: "alice@example.com",
		Recipient:  &brokerv1.EnvelopeRecipient{Target: &brokerv1.EnvelopeRecipient_UserId{UserId: "bob@example.com"}},
		Task:       &brokerv1.EnvelopeTask{Intent: "delegate this"},
		Delegation: &brokerv1.EnvelopeDelegation{},
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("list-objects error should fail closed (Internal, scope resolution), got %v", err)
	}
}

// ── roles: validateAssignable for delegatable ──────────────────────────────

func TestValidateAssignable_DelegatableMarker_Accept(t *testing.T) {
	// group:X#member, delegatable, group:X — the canonical marker shape.
	svc := NewBrokerService(testAdminDeps(t, ""))
	rule, err := svc.validateAssignable(tuple("group:security-team#member", "delegatable", "group:security-team"))
	if err != nil {
		t.Fatalf("valid delegatable marker rejected: %v", err)
	}
	if rule == nil || rule.relation != "delegatable" || rule.objectType != "group" {
		t.Errorf("wrong rule returned: %+v", rule)
	}
}

func TestValidateAssignable_DelegatableMarker_RejectUserSubject(t *testing.T) {
	// user:a@x, delegatable, group:X — subject kind must be group#member only.
	svc := NewBrokerService(testAdminDeps(t, ""))
	_, err := svc.validateAssignable(tuple("user:a@x", "delegatable", "group:security-team"))
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("user subject for delegatable should be InvalidArgument, got %v", err)
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

// testEnvelopeDeps wires a policy.Engine against both the given FGA stub URL
// and the given OPA stub URL. Deps.Tasks defaults to fakeEnvelopeStore so
// the allow path completes without Postgres; tests may override it.
func testEnvelopeDeps(t *testing.T, fgaURL, opaURL string) Deps {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := policy.Config{OPAEndpoint: opaURL, OpenFGAEndpoint: fgaURL}
	if fgaURL != "" {
		cfg.OpenFGAStoreID = "store-1"
	}
	eng, err := policy.NewEngine(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return Deps{
		Logger: zap.NewNop(),
		Audit:  em,
		Policy: eng,
		// senderDelegableScopes (delegation_scopes.go, F24 CP1) requires a
		// non-nil ToolRegistry whenever FGA is enabled — fails closed
		// otherwise. Production always wires one; tests need the same so the
		// FGA-enabled envelope-send path doesn't spuriously deny.
		ToolRegistry: newTestToolRegistry(),
		TenantID:     "aikonos-dev",
		KnownUsers:   []string{"alice@example.com", "bob@example.com"},
		Tasks:        &fakeEnvelopeStore{},
	}
}

// opaAlwaysAllow returns a stub OPA server that unconditionally allows.
func opaAlwaysAllow(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{"allow": true, "auto_accept": false},
		})
	}))
}

// opaEchoFGADecision returns a stub OPA server that allows iff
// input.fga_send_decision == "allow", mirroring the real envelope_send policy's
// primary gate so the FGA fallback logic is observable end-to-end.
func opaEchoFGADecision(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Input struct {
				FGASendDecision string `json:"fga_send_decision"`
			} `json:"input"`
		}
		_ = json.Unmarshal(body, &req)
		allow := req.Input.FGASendDecision == "allow"
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{"allow": allow, "auto_accept": false},
		})
	}))
}
