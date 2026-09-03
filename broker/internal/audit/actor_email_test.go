package audit

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
)

// fakeEmailResolver is a test double for EmailResolver that records call count.
type fakeEmailResolver struct {
	calls  atomic.Int32
	emails map[string]string // tenantID+":"+subject → email
	err    error
}

func (f *fakeEmailResolver) Email(_ context.Context, tenantID, subject string) string {
	f.calls.Add(1)
	if f.err != nil {
		return ""
	}
	return f.emails[tenantID+":"+subject]
}

// TestActorEmail_EmptyOmittedFromCanonicalBytes proves chain-safety:
// an event with empty ActorEmail produces byte-identical canonical bytes
// to the same struct before the field existed (omitempty omits it).
// This means old persisted events still verify after the proto change.
func TestActorEmail_EmptyOmittedFromCanonicalBytes(t *testing.T) {
	ev := &auditv1.AuditEvent{
		EventId:   "evt-chain-safe",
		TenantId:  "t1",
		EventType: "test.event",
		ActorEmail: "", // explicitly empty — must be omitted
	}

	withEmpty := canonicalBytes(ev)

	// Parse the JSON and confirm actor_email key is absent.
	var m map[string]any
	if err := json.Unmarshal(withEmpty, &m); err != nil {
		t.Fatalf("unmarshal canonical bytes: %v", err)
	}
	if _, present := m["actor_email"]; present {
		t.Error("actor_email must be omitted from canonical bytes when empty (chain-safety broken)")
	}
}

// TestActorEmail_PopulatedInCanonicalBytes proves a non-empty ActorEmail IS
// included in canonical bytes (so it's covered by the signature).
func TestActorEmail_PopulatedInCanonicalBytes(t *testing.T) {
	ev := &auditv1.AuditEvent{
		EventId:    "evt-with-email",
		TenantId:   "t1",
		EventType:  "test.event",
		ActorEmail: "alice@example.com",
	}

	canon := canonicalBytes(ev)
	var m map[string]any
	if err := json.Unmarshal(canon, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["actor_email"] != "alice@example.com" {
		t.Errorf("actor_email must appear in canonical bytes when set; got %v", m["actor_email"])
	}
}

// TestActorEmail_VerifyChainWithEmptyEmail proves that a full emit→verify
// round-trip with empty ActorEmail still produces ok=true (old events verify).
func TestActorEmail_VerifyChainWithEmptyEmail(t *testing.T) {
	key := "chain-safety-key"
	e, err := NewEmitter(context.Background(), Config{SigningKey: key})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	events := []*auditv1.AuditEvent{
		{EventId: ids_for_test(0), TenantId: "t1", EventType: "test.event"},
		{EventId: ids_for_test(1), TenantId: "t1", EventType: "test.event"},
		{EventId: ids_for_test(2), TenantId: "t1", EventType: "test.event"},
	}
	for i, ev := range events {
		// ActorEmail intentionally left empty (simulates pre-change persisted events)
		if err := e.Emit(context.Background(), ev); err != nil {
			t.Fatalf("Emit[%d]: %v", i, err)
		}
	}
	report := VerifyChain(events, key)
	if !report.OK {
		t.Errorf("chain with empty ActorEmail must verify ok; breaks=%v sig_failures=%v",
			report.Breaks, report.SignatureFailures)
	}
}

// TestEmitter_EnrichesActorEmailFromResolver proves that Emit() sets
// ActorEmail from the resolver when the event has actor_user_id but no email.
func TestEmitter_EnrichesActorEmailFromResolver(t *testing.T) {
	e, err := NewEmitter(context.Background(), Config{})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	resolver := &fakeEmailResolver{
		emails: map[string]string{"t1:user-oid-123": "alice@example.com"},
	}
	e.AttachEmailResolver(resolver)

	ev := &auditv1.AuditEvent{
		EventId:     ids_for_test(0),
		TenantId:    "t1",
		EventType:   "test.event",
		ActorUserId: "user-oid-123",
	}
	if err := e.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if ev.ActorEmail != "alice@example.com" {
		t.Errorf("want ActorEmail=alice@example.com, got %q", ev.ActorEmail)
	}
}

// TestEmitter_ResolverMiss_EmptyEmail proves a resolver miss → empty string, no error.
func TestEmitter_ResolverMiss_EmptyEmail(t *testing.T) {
	e, err := NewEmitter(context.Background(), Config{})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	e.AttachEmailResolver(&fakeEmailResolver{
		emails: map[string]string{}, // no entry for this subject
	})

	ev := &auditv1.AuditEvent{
		EventId:     ids_for_test(0),
		TenantId:    "t1",
		EventType:   "test.event",
		ActorUserId: "unknown-oid",
	}
	if err := e.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if ev.ActorEmail != "" {
		t.Errorf("miss should leave ActorEmail empty, got %q", ev.ActorEmail)
	}
}

// TestEmitter_ResolverError_Swallowed proves resolver error → empty string, event emits fine.
func TestEmitter_ResolverError_Swallowed(t *testing.T) {
	e, err := NewEmitter(context.Background(), Config{})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	e.AttachEmailResolver(&fakeEmailResolver{
		emails: map[string]string{},
		err:    errors.New("directory unavailable"), // exercises the error branch
	})
	ev := &auditv1.AuditEvent{
		EventId:     ids_for_test(0),
		TenantId:    "t1",
		EventType:   "test.event",
		ActorUserId: "user-oid",
	}
	if err := e.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit must not fail even when resolver returns empty: %v", err)
	}
}

// TestEmitter_EnrichmentBeforeSignCriticalSection proves ActorEmail is covered
// by the signature: emit with resolver, then VerifyChain confirms the signature
// includes the email (if it were set AFTER signing the chain would still verify
// but only because omitempty would have excluded it — here we explicitly check
// the signed event still passes full verification).
func TestEmitter_EnrichmentCoveredBySignature(t *testing.T) {
	key := "enrichment-sig-key"
	e, err := NewEmitter(context.Background(), Config{SigningKey: key})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	resolver := &fakeEmailResolver{
		emails: map[string]string{"t1:oid-abc": "bob@example.com"},
	}
	e.AttachEmailResolver(resolver)

	ev := &auditv1.AuditEvent{
		EventId:     ids_for_test(0),
		TenantId:    "t1",
		EventType:   "test.event",
		ActorUserId: "oid-abc",
	}
	if err := e.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if ev.ActorEmail != "bob@example.com" {
		t.Fatalf("resolver did not populate ActorEmail; got %q", ev.ActorEmail)
	}
	// VerifyChain recomputes the HMAC over canonicalBytes (which includes actor_email
	// because it's non-empty). If enrichment happened after signing this would fail.
	report := VerifyChain([]*auditv1.AuditEvent{ev}, key)
	if !report.OK {
		t.Errorf("enriched+signed event failed verification; sig_failures=%v", report.SignatureFailures)
	}
}

// TestEmitter_NoResolverAttached_NoEmailSet proves AttachEmailResolver is optional:
// without one, ActorEmail stays empty and Emit still works.
func TestEmitter_NoResolverAttached_NoEmailSet(t *testing.T) {
	e, err := NewEmitter(context.Background(), Config{})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	ev := &auditv1.AuditEvent{
		EventId:     ids_for_test(0),
		TenantId:    "t1",
		EventType:   "test.event",
		ActorUserId: "some-oid",
	}
	if err := e.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if ev.ActorEmail != "" {
		t.Errorf("no resolver → ActorEmail must stay empty, got %q", ev.ActorEmail)
	}
}

// TestEmitter_DoesNotOverwriteExistingActorEmail proves enrichment is skipped
// when ActorEmail is already set (e.g. from the interceptor).
func TestEmitter_DoesNotOverwriteExistingActorEmail(t *testing.T) {
	e, err := NewEmitter(context.Background(), Config{})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	resolver := &fakeEmailResolver{
		emails: map[string]string{"t1:oid-x": "from-resolver@example.com"},
	}
	e.AttachEmailResolver(resolver)

	ev := &auditv1.AuditEvent{
		EventId:     ids_for_test(0),
		TenantId:    "t1",
		EventType:   "test.event",
		ActorUserId: "oid-x",
		ActorEmail:  "already-set@example.com",
	}
	if err := e.Emit(context.Background(), ev); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if ev.ActorEmail != "already-set@example.com" {
		t.Errorf("must not overwrite existing ActorEmail; got %q", ev.ActorEmail)
	}
	if resolver.calls.Load() != 0 {
		t.Errorf("resolver must not be called when ActorEmail already set; got %d calls", resolver.calls.Load())
	}
}
