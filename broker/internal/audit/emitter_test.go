package audit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

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

// chainOf emits n events for tenant through a logging-only emitter, returning
// them in chain order with their chain hashes.
func chainOf(t *testing.T, tenant string, n int) ([]*auditv1.AuditEvent, []string) {
	t.Helper()
	e, _ := NewEmitter(context.Background(), Config{SigningKeySource: &fakeKeySource{currentKey: "k", currentVersion: 2}})
	var evs []*auditv1.AuditEvent
	var hashes []string
	for i := 0; i < n; i++ {
		x := ev(string(rune('a'+i)), tenant)
		_ = e.Emit(context.Background(), x)
		evs = append(evs, x)
		hashes = append(hashes, e.lastHash[tenant])
	}
	return evs, hashes
}

// Uploads can land out of chain order (enqueue happens after the chain lock),
// so LastModified order != chain order. Resume must pick the true tail, also
// when the tail sits in the previous day's directory.
func TestResumeTail_OutOfOrderUploads(t *testing.T) {
	evs, hashes := chainOf(t, "t1", 4)
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	store := &fakeStore{objects: map[string][]byte{}, modTime: map[string]time.Time{}}
	put := func(key string, ev *auditv1.AuditEvent, at time.Duration) {
		store.objects[key] = mustMarshalEvent(ev)
		store.modTime[key] = base.Add(at)
	}
	// Chain a→b→c→d. d (the tail) is in 2026/02/28 (month boundary) and was
	// uploaded before c, which is the LastModified-newest object.
	put("t1/2026/02/28/a.json", evs[0], -3*time.Second)
	put("t1/2026/02/28/b.json", evs[1], -2*time.Second)
	put("t1/2026/02/28/d.json", evs[3], 10*time.Millisecond)
	put("t1/2026/03/01/c.json", evs[2], 20*time.Millisecond)
	// Old history outside the window must not be read.
	put("t1/2026/02/28/old.json", evs[0], -time.Hour)
	store.getErrs = map[string]error{"t1/2026/02/28/old.json": errors.New("must not be read")}

	got, ok := resumeTail(context.Background(), store, "t1/2026/03/01/c.json")
	if !ok || got != hashes[3] {
		t.Fatalf("resumeTail = %q,%v; want true tail %q (newest-by-LastModified is %q)", got, ok, hashes[3], hashes[2])
	}
}

func TestResumeTail_SingleObject(t *testing.T) {
	evs, hashes := chainOf(t, "t1", 1)
	store := &fakeStore{objects: map[string][]byte{"t1/2026/03/01/a.json": mustMarshalEvent(evs[0])}}
	got, ok := resumeTail(context.Background(), store, "t1/2026/03/01/a.json")
	if !ok || got != hashes[0] {
		t.Fatalf("resumeTail = %q,%v; want %q", got, ok, hashes[0])
	}
}

func TestResumeTail_EmptyStore(t *testing.T) {
	if _, ok := resumeTail(context.Background(), &fakeStore{objects: map[string][]byte{}}, "t1/2026/03/01/a.json"); ok {
		t.Fatal("empty store must not resolve a tail")
	}
}

// Two open ends (an already-forked history) is ambiguous → fall back.
func TestChainTail_AmbiguousFallsBack(t *testing.T) {
	evs, _ := chainOf(t, "t1", 2)
	fork, _ := chainOf(t, "t1", 2)
	fork[1].EventId = "fork" // same prior as evs[1], different hash
	if _, ok := chainTail([]*auditv1.AuditEvent{evs[0], evs[1], fork[1]}); ok {
		t.Fatal("forked candidates must be ambiguous")
	}
}
