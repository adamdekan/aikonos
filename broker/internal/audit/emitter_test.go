package audit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
)

func ev(id, tenant string) *auditv1.AuditEvent {
	return &auditv1.AuditEvent{EventId: id, TenantId: tenant, EventType: "test.event"}
}

// Logging-only mode (no MinIO config) must accept events without a client.
func TestEmitLoggingOnly(t *testing.T) {
	e, err := NewEmitter(context.Background(), Config{})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	if e.client != nil {
		t.Fatal("expected logging-only emitter (nil client)")
	}
	if err := e.Emit(context.Background(), ev("a", "t1")); err != nil {
		t.Fatalf("Emit: %v", err)
	}
}

func TestEmitValidatesRequiredFields(t *testing.T) {
	e, _ := NewEmitter(context.Background(), Config{})
	if err := e.Emit(context.Background(), &auditv1.AuditEvent{EventId: "x"}); err == nil {
		t.Fatal("expected error for missing tenant_id/event_type")
	}
}

// The per-tenant hash chain links events: each event's prior_event_hash is the
// SHA-256 of the previous event's canonical (unsigned) form, and chains are
// independent per tenant.
func TestHashChain(t *testing.T) {
	e, _ := NewEmitter(context.Background(), Config{})
	ctx := context.Background()

	e1 := ev("1", "t1")
	_ = e.Emit(ctx, e1)
	if e1.PriorEventHash != "" {
		t.Fatalf("first event should have empty prior hash, got %q", e1.PriorEventHash)
	}

	e2 := ev("2", "t1")
	_ = e.Emit(ctx, e2)
	if e2.PriorEventHash == "" {
		t.Fatal("second event should chain to the first")
	}

	// e2.prior must equal sha256(canonical(e1)).
	clone := ev("1", "t1")
	clone.PriorEventHash = "" // first event's prior
	clone.Signature = ""
	canonical, _ := json.Marshal(clone)
	want := sha256.Sum256(canonical)
	if e2.PriorEventHash != hex.EncodeToString(want[:]) {
		t.Fatalf("chain mismatch: prior=%s want=%s", e2.PriorEventHash, hex.EncodeToString(want[:]))
	}

	// A different tenant starts its own chain.
	other := ev("3", "t2")
	_ = e.Emit(ctx, other)
	if other.PriorEventHash != "" {
		t.Fatalf("new tenant should start a fresh chain, got %q", other.PriorEventHash)
	}
}

func TestSignature(t *testing.T) {
	key := "test-signing-key"
	e, _ := NewEmitter(context.Background(), Config{SigningKey: key})
	event := ev("1", "t1")
	_ = e.Emit(context.Background(), event)
	if event.Signature == "" {
		t.Fatal("expected a signature when SigningKey is set")
	}

	// Recompute: HMAC over the canonical form (prior hash set, signature empty).
	verify := ev("1", "t1")
	verify.PriorEventHash = event.PriorEventHash
	verify.Signature = ""
	canonical, _ := json.Marshal(verify)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(canonical)
	if want := hex.EncodeToString(mac.Sum(nil)); want != event.Signature {
		t.Fatalf("signature mismatch: got %s want %s", event.Signature, want)
	}
}

func TestObjectKey(t *testing.T) {
	got := objectKey(ev("01KTBKJB-evt", "tenant-x"))
	// Without occurred_at it uses time.Now(); just assert the prefix/suffix shape.
	if len(got) < len("tenant-x/0000/00/00/01KTBKJB-evt.json") {
		t.Fatalf("unexpected key shape: %s", got)
	}
	if got[:len("tenant-x/")] != "tenant-x/" {
		t.Fatalf("key should start with tenant: %s", got)
	}
}
