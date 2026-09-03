package broker

import (
	"testing"

	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// ── CP1 (F24 "implement for real"): dev-stub marker only in FGA-disabled mode ─
//
// With FGA disabled, senderDelegableScopes (delegation_scopes.go) falls back
// to the hardcoded stub list — this test proves the audit trail still marks
// that fallback loudly ("dev-stub") so an operator can tell the attenuation
// was never grounded in a real OpenFGA grant. Passing "" as the fgaURL to
// testEnvelopeDeps leaves OpenFGAStoreID unset, i.e. FGAEnabled() == false —
// the real dev-stack-unseeded case this marker exists for.

func TestSendEnvelope_Sent_CarriesDelegationScopesDevStubMarker(t *testing.T) {
	opaSrv := opaAlwaysAllow(t)
	defer opaSrv.Close()

	em := &recordingEmitter{}
	deps := testEnvelopeDeps(t, "", opaSrv.URL)
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
	if err != nil {
		t.Fatalf("SendEnvelope: unexpected error: %v", err)
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
	if got != "dev-stub" {
		t.Errorf("envelope-send audit event context[delegation_scopes] = %q, want %q", got, "dev-stub")
	}
}
