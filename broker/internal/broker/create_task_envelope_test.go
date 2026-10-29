package broker

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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

func (s *envelopeCreateStore) CountEnvelopeTasks(_ context.Context, _, owner, id string) (int, error) {
	n := 0
	for _, t := range s.created {
		if t.OwnerUserID == owner && t.EnvelopeID != nil && t.EnvelopeID.String() == id {
			n++
		}
	}
	return n, nil
}

const testEnvelopeID = "55555555-5555-5555-5555-555555555555"

// AG-17: the inbox "Send to agent" path dismisses the envelope and then
// starts a chat carrying its id ("Start a new session" leaves it delivered);
// the recipient's task names the sender.
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

// F139: one envelope stamps at most maxEnvelopeTasks tasks as delegated by
// its sender; past that the recipient's tasks run as their own and the
// ignore is audited.
func TestCreateTask_EnvelopeID_TaskCap(t *testing.T) {
	env := deliveredEnvelope(testTenantUUID, testEnvelopeID, "bob@example.com")
	env.State = db.EnvelopeDismissed
	store := &envelopeCreateStore{envelope: env}
	svc := newBrokerSvcForCreateTaskTest(t, store)
	em, sub := buildAuditBus(t, testTenantUUID)
	svc.deps.Audit = em
	for i := 0; i <= maxEnvelopeTasks; i++ {
		if _, err := svc.CreateTask(ctxWithIdentity(testTenantUUID, "bob@example.com"), &brokerv1.CreateTaskRequest{
			TenantId: testTenantUUID, Prompt: "p", EnvelopeId: testEnvelopeID,
		}); err != nil {
			t.Fatalf("task %d: CreateTask: %v", i+1, err)
		}
	}
	if d := store.created[maxEnvelopeTasks-1].DelegatedByUserID; d == nil {
		t.Fatalf("task %d: not delegated, want the cap to allow it", maxEnvelopeTasks)
	}
	if d := store.created[maxEnvelopeTasks].DelegatedByUserID; d != nil {
		t.Fatalf("task %d: delegated_by = %q, want unset past the cap", maxEnvelopeTasks+1, *d)
	}
	ev := receiveAuditEvent(t, sub, "aikonos.broker.task.delegation_ignored", 2*time.Second)
	if r := ev.Context.Fields["reason"].GetStringValue(); r != "envelope_task_cap" {
		t.Errorf("reason = %q, want envelope_task_cap", r)
	}
}

// G48/F179: oversized prompt and client_request_id are refused before
// anything is stored.
func TestCreateTask_FieldCaps(t *testing.T) {
	for name, req := range map[string]*brokerv1.CreateTaskRequest{
		"prompt":            {TenantId: testTenantUUID, Prompt: strings.Repeat("a", maxPromptBytes+1)},
		"client_request_id": {TenantId: testTenantUUID, Prompt: "p", ClientRequestId: strings.Repeat("a", maxClientRequestIDBytes+1)},
	} {
		store := &envelopeCreateStore{}
		svc := newBrokerSvcForCreateTaskTest(t, store)
		_, err := svc.CreateTask(ctxWithIdentity(testTenantUUID, "bob@example.com"), req)
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("%s: got %v, want InvalidArgument", name, err)
		}
		if len(store.created) != 0 {
			t.Fatalf("%s: task stored despite the cap", name)
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
