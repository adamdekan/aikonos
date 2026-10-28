package broker

import (
	"context"
	"testing"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/db"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// envelopeCreateStore records created tasks and serves one envelope by id.
type envelopeCreateStore struct {
	fakeCreateStore
	envelope *db.Envelope
}

func (s *envelopeCreateStore) GetEnvelope(_ context.Context, _, id string) (*db.Envelope, error) {
	if s.envelope == nil || s.envelope.EnvelopeID.String() != id {
		return nil, db.ErrNotFound
	}
	return s.envelope, nil
}

const testEnvelopeID = "55555555-5555-5555-5555-555555555555"

// AG-17: the inbox "Send to agent" path dismisses the envelope and then
// starts a chat carrying its id; the recipient's task names the sender.
func TestCreateTask_EnvelopeID_Recipient(t *testing.T) {
	for _, state := range []db.EnvelopeState{db.EnvelopeDelivered, db.EnvelopeDismissed, db.EnvelopeAccepted} {
		env := deliveredEnvelope(testTenantUUID, testEnvelopeID, "bob@example.com")
		env.State = state
		store := &envelopeCreateStore{envelope: env}
		svc := newBrokerSvcForCreateTaskTest(t, store)
		if _, err := svc.CreateTask(ctxWithIdentity(testTenantUUID, "bob@example.com"), &brokerv1.CreateTaskRequest{
			TenantId: testTenantUUID, Prompt: "p", EnvelopeId: testEnvelopeID,
		}); err != nil {
			t.Fatalf("%s: CreateTask: %v", state, err)
		}
		if d := store.created[0].DelegatedByUserID; d == nil || *d != "alice@example.com" {
			t.Errorf("%s: delegated_by = %v, want alice@example.com", state, d)
		}
	}
}

// A caller who is not the envelope's recipient still gets their task, but it
// is not marked delegated and the ignore is audited.
func TestCreateTask_EnvelopeID_NotRecipient(t *testing.T) {
	store := &envelopeCreateStore{envelope: deliveredEnvelope(testTenantUUID, testEnvelopeID, "bob@example.com")}
	svc := newBrokerSvcForCreateTaskTest(t, store)
	em, sub := buildAuditBus(t, testTenantUUID)
	svc.deps.Audit = em

	if _, err := svc.CreateTask(ctxWithIdentity(testTenantUUID, "mallory@example.com"), &brokerv1.CreateTaskRequest{
		TenantId: testTenantUUID, Prompt: "p", EnvelopeId: testEnvelopeID,
	}); err != nil {
		t.Fatalf("CreateTask must not fail on a foreign envelope: %v", err)
	}
	if d := store.created[0].DelegatedByUserID; d != nil {
		t.Errorf("delegated_by = %q, want unset", *d)
	}
	ev := receiveAuditEvent(t, sub, "aikonos.broker.task.delegation_ignored", 2*time.Second)
	if r := ev.Context.Fields["reason"].GetStringValue(); r != "envelope_not_recipient" {
		t.Errorf("reason = %q, want envelope_not_recipient", r)
	}
}

// Unknown, malformed and rejected envelope ids leave the task undelegated.
func TestCreateTask_EnvelopeID_Ignored(t *testing.T) {
	rejected := deliveredEnvelope(testTenantUUID, testEnvelopeID, "bob@example.com")
	rejected.State = db.EnvelopeRejected
	for name, tc := range map[string]struct {
		env *db.Envelope
		id  string
	}{
		"unknown":  {nil, testEnvelopeID},
		"invalid":  {nil, "not-a-uuid"},
		"rejected": {rejected, testEnvelopeID},
	} {
		store := &envelopeCreateStore{envelope: tc.env}
		svc := newBrokerSvcForCreateTaskTest(t, store)
		if _, err := svc.CreateTask(ctxWithIdentity(testTenantUUID, "bob@example.com"), &brokerv1.CreateTaskRequest{
			TenantId: testTenantUUID, Prompt: "p", EnvelopeId: tc.id,
		}); err != nil {
			t.Fatalf("%s: CreateTask: %v", name, err)
		}
		if d := store.created[0].DelegatedByUserID; d != nil {
			t.Errorf("%s: delegated_by = %q, want unset", name, *d)
		}
	}
}
