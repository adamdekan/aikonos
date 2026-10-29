package audit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
)

// F30: Verify walks the whole trail, not just the oldest 5000 objects, so a
// tampered event past that point is caught.
func TestReader_VerifyChain_TamperPast5000IsCaught(t *testing.T) {
	const key = "k"
	events, _ := buildChain(t, maxScanObjects+1, key)
	last := events[len(events)-1]
	last.EventType = "forged.event"
	r := &Reader{store: storeFor("tenant1", events), cfg: Config{SigningKey: key}, logger: nopLogger()}

	report, _, err := r.VerifyChain(context.Background(), "tenant1")
	if err != nil {
		t.Fatal(err)
	}
	if report.OK || report.Truncated || report.Total != len(events) {
		t.Fatalf("want a complete, failing walk over %d events; got %+v", len(events), report)
	}
	if len(report.SignatureFailures) != 1 || report.SignatureFailures[0].EventID != last.EventId {
		t.Fatalf("want the tampered tail as the only signature failure, got %+v", report.SignatureFailures)
	}
}

// F30: an object overwritten with garbage is skipped, and a skip is never OK.
func TestReader_VerifyChain_GarbageObjectNotOK(t *testing.T) {
	events, _ := buildChain(t, 3, "k")
	store := storeFor("tenant1", events)
	store.objects["tenant1/"+events[2].EventId+".json"] = []byte("not json")
	r := &Reader{store: store, cfg: Config{SigningKey: "k"}, logger: nopLogger()}

	report, _, err := r.VerifyChain(context.Background(), "tenant1")
	if err != nil {
		t.Fatal(err)
	}
	if report.OK || report.Skipped != 1 {
		t.Fatalf("want OK=false Skipped=1, got %+v", report)
	}
}

// F31: a signature failure must not hand back the MAC the verifier expected —
// that is a valid signature for the stored (forged) content.
func TestVerifyChain_SignatureFailureLeaksNoMAC(t *testing.T) {
	const key = "oracle-key"
	events, _ := buildChain(t, 2, key)
	events[1].EventType = "forged.event"
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(canonicalBytes(events[1]))
	want := hex.EncodeToString(mac.Sum(nil))

	for _, sig := range []string{events[1].Signature, "zz-not-hex"} {
		events[1].Signature = sig
		report := VerifyChain(events, key)
		if len(report.SignatureFailures) != 1 {
			t.Fatalf("sig %q: want 1 signature failure, got %+v", sig, report.SignatureFailures)
		}
		if d := report.SignatureFailures[0].Detail; d != "HMAC mismatch" || strings.Contains(d, want) {
			t.Fatalf("sig %q: detail %q must be the fixed string and must not contain the expected MAC", sig, d)
		}
	}
}

// F28: a write that fails is reported to the caller and does not move the
// chain head, so the next persisted event links to the last persisted one and
// the trail still verifies once the store recovers.
func TestEmit_FailedWriteReturnsErrorAndLeavesNoHole(t *testing.T) {
	ctx := context.Background()
	e, _ := NewEmitter(ctx, Config{SigningKey: "k"})
	store := &fakeStore{objects: map[string][]byte{}, putErr: errors.New("minio down")}
	old := putBackoff
	putBackoff = time.Millisecond
	defer func() { putBackoff = old }()
	e.logger = zap.NewNop()
	e.startSink(ctx, store, 8)
	defer e.Close()

	if err := e.Emit(ctx, ev("lost", "t1")); err == nil {
		t.Fatal("Emit must report a write that never landed")
	}
	if h := e.lastHash["t1"]; h != "" {
		t.Fatalf("chain head advanced past an unpersisted event: %q", h)
	}

	store.putErr = nil
	for _, id := range []string{"a", "b"} {
		if err := e.Emit(ctx, ev(id, "t1")); err != nil {
			t.Fatalf("Emit %s: %v", id, err)
		}
	}
	r := &Reader{store: store, cfg: Config{SigningKey: "k"}, logger: nopLogger()}
	report, _, err := r.VerifyChain(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK || report.Total != 2 {
		t.Fatalf("trail after recovery must verify as one unbroken chain: %+v", report)
	}
}

// F28: a full queue is an error for the caller, not a silent drop.
func TestEmit_QueueFullReturnsError(t *testing.T) {
	e := &Emitter{
		queue:       make(chan uploadJob), // no worker: always full
		dropTimeout: time.Millisecond,
		logger:      zap.NewNop(),
		lastHash:    map[string]string{},
		keySource:   staticKeySource{},
	}
	if err := e.Emit(context.Background(), ev("x", "t1")); err == nil {
		t.Fatal("Emit must report an event dropped on a full queue")
	}
}

// F46: a tenant id cannot steer its objects or chain into another prefix.
func TestKeySegment_ConfinesTenantToItsPrefix(t *testing.T) {
	const uuid = "0199a4f0-3c2e-7d41-9b6a-1f2e3d4c5b6a"
	if got := keySegment(uuid); got != uuid {
		t.Fatalf("UUIDs must map to themselves, got %q", got)
	}
	for _, tenant := range []string{"victim/2026/10/29/x", "_policy", "..", ".", "a%2Fb"} {
		seg := keySegment(tenant)
		if strings.Contains(seg, "/") || strings.HasPrefix(seg, "_") || strings.HasPrefix(seg, ".") {
			t.Errorf("tenant %q → unsafe segment %q", tenant, seg)
		}
	}
	if keySegment("a/b") == keySegment("a%2Fb") {
		t.Error("keySegment must be injective")
	}
	key := objectKey(&auditv1.AuditEvent{EventId: "e/../x", TenantId: "victim/2026/10/29/x"})
	if n := strings.Count(key, "/"); n != 4 {
		t.Fatalf("object key %q must have exactly the 4 layout separators, got %d", key, n)
	}
}
