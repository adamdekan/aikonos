package audit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
)

// ── #2: Query must return an error on list failure ────────────────────────────

func TestReader_Query_ListError_ReturnsError(t *testing.T) {
	store := &fakeStore{
		listErr: errors.New("simulated list failure"),
		objects: map[string][]byte{},
	}
	r := &Reader{store: store, cfg: Config{}, logger: nopLogger()}

	_, _, _, err := r.Query(context.Background(), "tenant1", QueryFilter{Limit: 10})
	if err == nil {
		t.Fatal("want non-nil error when list fails, got nil — caller can't distinguish configured-but-broken from empty")
	}
}

func TestReader_Query_EmptyResult_NoError(t *testing.T) {
	// Configured store with no objects → (nil, "", true, nil)
	store := &fakeStore{objects: map[string][]byte{}}
	r := &Reader{store: store, cfg: Config{}, logger: nopLogger()}

	events, next, ok, err := r.Query(context.Background(), "tenant1", QueryFilter{Limit: 10})
	if err != nil {
		t.Fatalf("empty store must not error: %v", err)
	}
	if !ok {
		t.Error("want ok=true (store configured)")
	}
	if next != "" {
		t.Errorf("want empty next_cursor, got %q", next)
	}
	if len(events) != 0 {
		t.Errorf("want 0 events, got %d", len(events))
	}
}

func TestReader_Query_Unconfigured_OkFalseNoError(t *testing.T) {
	r := &Reader{store: nil, cfg: Config{}, logger: nopLogger()}

	_, _, ok, err := r.Query(context.Background(), "tenant1", QueryFilter{Limit: 10})
	if ok {
		t.Error("want ok=false for unconfigured reader")
	}
	if err != nil {
		t.Errorf("want nil error for unconfigured reader, got %v", err)
	}
}

// ── #1: readAll / VerifyChain surfaces truncated + skipped ───────────────────

// buildManyMetas builds a fakeStore with n+1 keys for the tenant; the n+1-th
// puts the total over maxScanObjects to trigger the truncation cap.
func buildOverLimitStore(n int) *fakeStore {
	objects := make(map[string][]byte, n)
	for i := 0; i < n; i++ {
		ev := &auditv1.AuditEvent{
			EventId:   ids_for_test(i),
			TenantId:  "tenant1",
			EventType: "test.event",
		}
		objects["tenant1/"+ev.EventId+".json"] = mustMarshalEvent(ev)
	}
	return &fakeStore{objects: objects}
}

func TestReader_VerifyChain_Truncated(t *testing.T) {
	// Build maxScanObjects+1 objects so readAll truncates.
	n := maxScanObjects + 1
	store := buildOverLimitStore(n)
	r := &Reader{store: store, cfg: Config{}, logger: nopLogger()}

	report, ok, err := r.VerifyChain(context.Background(), "tenant1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("want ok=true (store configured)")
	}
	if !report.Truncated {
		t.Error("want Truncated=true when store has more than maxScanObjects objects")
	}
}

func TestReader_VerifyChain_Skipped(t *testing.T) {
	ev1 := makeEvent("aaa", "alice", "task.created", auditv1.PolicyDecision_ALLOW, t0)
	ev2 := makeEvent("bbb", "alice", "task.created", auditv1.PolicyDecision_ALLOW, t1)

	store := &fakeStore{
		objects: map[string][]byte{
			"tenant1/aaa.json": mustMarshalEvent(ev1),
			"tenant1/bbb.json": mustMarshalEvent(ev2),
		},
		getErrs: map[string]error{
			"tenant1/bbb.json": errors.New("simulated get failure"),
		},
	}
	r := &Reader{store: store, cfg: Config{}, logger: nopLogger()}

	report, ok, err := r.VerifyChain(context.Background(), "tenant1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("want ok=true (store configured)")
	}
	if report.Skipped < 1 {
		t.Errorf("want Skipped>=1 when a get fails, got %d", report.Skipped)
	}
}

// ── #6: emit → verify round-trip proves canonicalization is shared ────────────

func TestEmitVerifyRoundTrip_OkTrue(t *testing.T) {
	key := "round-trip-key"
	e, err := NewEmitter(context.Background(), Config{SigningKey: key})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	ctx := context.Background()

	events := make([]*auditv1.AuditEvent, 4)
	for i := range events {
		events[i] = &auditv1.AuditEvent{
			EventId:   ids_for_test(i),
			TenantId:  "tenant1",
			EventType: "test.event",
		}
		if err := e.Emit(ctx, events[i]); err != nil {
			t.Fatalf("Emit[%d]: %v", i, err)
		}
	}

	// Emit mutates each event in-place (sets PriorEventHash + Signature), so the
	// slice VerifyChain reads here is the same populated slice Emit produced.
	report := VerifyChain(events, key)
	if !report.OK {
		t.Errorf("emit→verify round-trip: want ok=true; breaks=%v sig_failures=%v",
			report.Breaks, report.SignatureFailures)
	}
}

// ── #7: canonicalBytes is the shared contract between emitter and verifier ───
//
// TestCanonicalBytes_MatchesEmitterSignature ties the shared helper explicitly
// to the emitter's signed output: it emits two events through a logging-only
// Emitter with a signing key, then independently recomputes both the HMAC
// signature and the chain-link hash using canonicalBytes and asserts they match
// what the emitter stored. This catches any drift between emitter.go and
// verify.go without relying on TestHashChain/TestSignature (which inline bare
// json.Marshal instead of calling canonicalBytes).
func TestCanonicalBytes_MatchesEmitterSignature(t *testing.T) {
	key := "canonical-contract-key"
	e, err := NewEmitter(context.Background(), Config{SigningKey: key})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	ctx := context.Background()

	ev0 := &auditv1.AuditEvent{EventId: ids_for_test(0), TenantId: "tenant1", EventType: "test.event"}
	ev1 := &auditv1.AuditEvent{EventId: ids_for_test(1), TenantId: "tenant1", EventType: "test.event"}
	if err := e.Emit(ctx, ev0); err != nil {
		t.Fatalf("Emit[0]: %v", err)
	}
	if err := e.Emit(ctx, ev1); err != nil {
		t.Fatalf("Emit[1]: %v", err)
	}

	// Assert 1: HMAC-SHA256(canonicalBytes(ev0), key) == ev0.Signature.
	canon0 := canonicalBytes(ev0)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(canon0)
	wantSig := hex.EncodeToString(mac.Sum(nil))
	if ev0.Signature != wantSig {
		t.Errorf("ev0 signature: emitter stored %s, canonicalBytes recompute gives %s — emitter↔verifier canonical form has drifted", ev0.Signature, wantSig)
	}

	// Assert 2: hex(sha256(canonicalBytes(ev0))) == ev1.PriorEventHash.
	// This proves canonicalBytes is what the chain links on, not some other form.
	sum := sha256.Sum256(canon0)
	wantChainLink := hex.EncodeToString(sum[:])
	if ev1.PriorEventHash != wantChainLink {
		t.Errorf("ev1.PriorEventHash: emitter stored %s, sha256(canonicalBytes(ev0)) gives %s — chain link and canonical form have drifted", ev1.PriorEventHash, wantChainLink)
	}
}
