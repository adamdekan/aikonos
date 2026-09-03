package broker

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ── CP2 (F24 "implement for real"): send-path behavior tests ───────────────
//
//  CP2. These tests prove the FGA-derived
// scope set (senderDelegableScopes, wired in CP1) actually reaches the
// SendEnvelope decision and mint — not merely that senderDelegableScopes
// itself computes the right value in isolation (delegation_scopes_test.go
// already covers that).

// opaScopeSubsetCheck is a stub OPA server mirroring the real
// policies/opa/envelope_send.rego subset-check rule: allow iff
// fga_send_decision == "allow" and every attenuated scope is present in
// sender.capability_scopes. This is the actual teeth of the attenuation
// invariant — SenderScopes must be the FGA-derived set for this check to be
// meaningful.
func opaScopeSubsetCheck(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Input struct {
				FGASendDecision string `json:"fga_send_decision"`
				Sender          struct {
					CapabilityScopes []string `json:"capability_scopes"`
				} `json:"sender"`
				Envelope struct {
					Delegation struct {
						AttenuatedScopes []string `json:"attenuated_scopes"`
					} `json:"delegation"`
				} `json:"envelope"`
			} `json:"input"`
		}
		_ = json.Unmarshal(body, &req)

		granted := make(map[string]bool, len(req.Input.Sender.CapabilityScopes))
		for _, s := range req.Input.Sender.CapabilityScopes {
			granted[s] = true
		}
		allow := req.Input.FGASendDecision == "allow"
		for _, s := range req.Input.Envelope.Delegation.AttenuatedScopes {
			if !granted[s] {
				allow = false
				break
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{"allow": allow, "auto_accept": false},
		})
	}))
}

// TestSendEnvelope_FGAEnabled_ScopeSubsetCheck_AllowsWithinDerivedSet proves
// the FGA-derived scope set reaches the OPA decision: alice is granted only
// skill:doc.write (→ scope "doc:write"), and a delegation attenuated to that
// same scope is allowed, and the "fga-derived" audit marker is present.
func TestSendEnvelope_FGAEnabled_ScopeSubsetCheck_AllowsWithinDerivedSet(t *testing.T) {
	f := &fakeFGA{
		checks: map[string]bool{
			"user:alice@example.com|can_delegate_to_user|user:bob@example.com": true,
		},
		listObjectsResultByUser: map[string][]string{
			"user:alice@example.com": {"skill:doc.write"},
		},
	}
	fgaSrv := f.server(t)
	defer fgaSrv.Close()

	opaSrv := opaScopeSubsetCheck(t)
	defer opaSrv.Close()

	em := &recordingEmitter{}
	deps := testEnvelopeDeps(t, fgaSrv.URL, opaSrv.URL)
	deps.Audit = em
	svc := NewBrokerService(deps)
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	h, err := svc.SendEnvelope(ctx, &brokerv1.SendEnvelopeRequest{
		TenantId:   testTenantUUID,
		FromUserId: "alice@example.com",
		Recipient:  &brokerv1.EnvelopeRecipient{Target: &brokerv1.EnvelopeRecipient_UserId{UserId: "bob@example.com"}},
		Task:       &brokerv1.EnvelopeTask{Intent: "delegate this"},
		Delegation: &brokerv1.EnvelopeDelegation{AttenuatedScopes: []string{"doc:write"}},
	})
	if err != nil {
		t.Fatalf("delegation within the FGA-derived scope set should be allowed, got %v", err)
	}
	if h == nil || h.EnvelopeId == "" {
		t.Fatalf("allow path returned no envelope handle: %+v", h)
	}

	var sent *auditv1.AuditEvent
	for _, ev := range em.events {
		if ev.EventType == "aikonos.broker.envelope.sent" {
			sent = ev
			break
		}
	}
	if sent == nil {
		t.Fatalf("no aikonos.broker.envelope.sent event emitted; got %d events", len(em.events))
	}
	got, _ := sent.GetContext().AsMap()["delegation_scopes"].(string)
	if got != "fga-derived" {
		t.Errorf("envelope-send audit event context[delegation_scopes] = %q, want %q", got, "fga-derived")
	}
}

// TestSendEnvelope_FGAEnabled_ScopeSubsetCheck_DeniesOutsideDerivedSet is the
// deny-side twin: alice is granted only skill:doc.write, but the delegation
// requests "slack:write" — a scope outside her FGA-derived set. The OPA
// subset check must deny. This is the real teeth of the attenuation
// invariant: without CP1's wiring, SenderScopes would have been the
// hardcoded dev-stub list (which does include "slack:write"), and this
// request would have wrongly been allowed.
func TestSendEnvelope_FGAEnabled_ScopeSubsetCheck_DeniesOutsideDerivedSet(t *testing.T) {
	f := &fakeFGA{
		checks: map[string]bool{
			"user:alice@example.com|can_delegate_to_user|user:bob@example.com": true,
		},
		listObjectsResultByUser: map[string][]string{
			"user:alice@example.com": {"skill:doc.write"},
		},
	}
	fgaSrv := f.server(t)
	defer fgaSrv.Close()

	opaSrv := opaScopeSubsetCheck(t)
	defer opaSrv.Close()

	svc := NewBrokerService(testEnvelopeDeps(t, fgaSrv.URL, opaSrv.URL))
	ctx := ctxWithIdentity(testTenantUUID, "alice@example.com")

	_, err := svc.SendEnvelope(ctx, &brokerv1.SendEnvelopeRequest{
		TenantId:   testTenantUUID,
		FromUserId: "alice@example.com",
		Recipient:  &brokerv1.EnvelopeRecipient{Target: &brokerv1.EnvelopeRecipient_UserId{UserId: "bob@example.com"}},
		Task:       &brokerv1.EnvelopeTask{Intent: "delegate this"},
		Delegation: &brokerv1.EnvelopeDelegation{AttenuatedScopes: []string{"slack:write"}},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("delegation requesting a scope outside the FGA-derived set should be denied, got %v", err)
	}
}

// TestSendEnvelope_ListObjectsError_NoEnvelopeRowCreated extends the CP1
// fail-closed coverage (TestSendEnvelope_DelegationGroupFallback_FailClosed_OnListError,
// delegation_groups_test.go) with the CP2-required assertion that a
// scope-resolution failure creates no envelope row — the deny must be
// complete, not merely reported.
func TestSendEnvelope_ListObjectsError_NoEnvelopeRowCreated(t *testing.T) {
	f := &fakeFGA{listObjectsErr: true}
	fgaSrv := f.server(t)
	defer fgaSrv.Close()

	opaSrv := opaAlwaysAllow(t)
	defer opaSrv.Close()

	store := &trackingEnvelopeStore{}
	em := &recordingEmitter{}
	deps := testEnvelopeDeps(t, fgaSrv.URL, opaSrv.URL)
	deps.Tasks = store
	deps.Audit = em
	svc := NewBrokerService(deps)
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
	if store.count != 0 {
		t.Errorf("no envelope row should be created on scope-resolution failure, got %d CreateEnvelope calls", store.count)
	}

	var denied *auditv1.AuditEvent
	for _, ev := range em.events {
		if ev.EventType == "aikonos.broker.envelope.denied" {
			denied = ev
			break
		}
	}
	if denied == nil {
		t.Fatalf("no aikonos.broker.envelope.denied event emitted; got %d events", len(em.events))
	}
}
