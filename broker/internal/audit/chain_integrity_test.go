package audit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/notify"
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

// F30 review: a walk that runs out of time comes back Truncated, before the
// caller's deadline, instead of failing the RPC with no verdict at all.
func TestReader_VerifyChain_OutOfTimeIsIncompleteNotError(t *testing.T) {
	old := verifyMargin
	verifyMargin = 200 * time.Millisecond
	defer func() { verifyMargin = old }()
	events, _ := buildChain(t, 1000, "k")
	store := &fakeStore{objects: map[string][]byte{}, getDelay: 20 * time.Millisecond}
	for i, e := range events { // 25 day directories, in chain order
		store.objects[fmt.Sprintf("tenant1/2026/01/%02d/%s.json", i/40+1, e.EventId)] = mustMarshalEvent(e)
	}
	r := &Reader{store: store, cfg: Config{SigningKey: "k"}, logger: nopLogger()}
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()

	report, _, err := r.VerifyChain(ctx, "tenant1")
	if err != nil {
		t.Fatalf("out of time must give an incomplete report, not an error: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("VerifyChain must return before the caller's deadline")
	}
	if !report.Truncated || report.OK || report.Skipped != 0 || report.Total >= len(events) {
		t.Fatalf("want Truncated, !OK, no skips, partial Total; got %+v", report)
	}
	// The verified part is a gap-free prefix: an intact chain cut short shows
	// no break, so the verdict is "incomplete", never "broken".
	if len(report.Breaks) != 0 || len(report.SignatureFailures) != 0 {
		t.Fatalf("a cut walk over an intact chain must report no breaks, got %+v", report.Breaks)
	}
}

// F31 review: a key-resolution failure names no error: a Vault client error
// carries the internal Vault URL and KV path.
func TestVerifyChain_KeyResolutionFailureLeaksNoError(t *testing.T) {
	events, _ := buildChain(t, 1, "k")
	broken := &fakeKeySource{forVersionErr: errors.New(`Get "http://vault:8200/v1/secret/data/aikonos/audit?version=0": connection refused`)}
	report := VerifyChainVersioned(context.Background(), events, broken)
	if len(report.SignatureFailures) != 1 {
		t.Fatalf("want 1 signature failure, got %+v", report.SignatureFailures)
	}
	if d := report.SignatureFailures[0].Detail; d != "signing key for version 0 unavailable" {
		t.Fatalf("detail %q must be the fixed string", d)
	}
}

// fakeBus records what Emit publishes to the live stream.
type fakeBus struct {
	subjects []string
	payloads [][]byte
}

func (b *fakeBus) Enabled() bool { return true }
func (b *fakeBus) Publish(_ context.Context, subject string, payload []byte) error {
	b.subjects = append(b.subjects, subject)
	b.payloads = append(b.payloads, payload)
	return nil
}
func (b *fakeBus) Subscribe(string) (notify.Subscription, error) { return nil, nil }
func (b *fakeBus) Close() error                                  { return nil }

// F28 review: an event dropped on a full queue still reaches the live stream
// the alerting engine consumes, so a stalled store cannot hide a denial.
func TestEmit_DroppedEventStillPublished(t *testing.T) {
	bus := &fakeBus{}
	e := &Emitter{
		queue:       make(chan uploadJob), // no worker: always full
		dropTimeout: time.Millisecond,
		logger:      zap.NewNop(),
		lastHash:    map[string]string{},
		keySource:   staticKeySource{},
		bus:         bus,
	}
	deny := ev("denied", "t1")
	deny.Decision = auditv1.PolicyDecision_DENY
	if err := e.Emit(context.Background(), deny); err == nil {
		t.Fatal("Emit must report the drop")
	}
	if len(bus.payloads) != 1 || bus.subjects[0] != notify.AuditSubject("t1") {
		t.Fatalf("dropped event must be published once on the tenant audit subject, got %v", bus.subjects)
	}
	if got, err := unmarshalAuditEvent(bus.payloads[0]); err != nil || got.EventId != "denied" || got.Decision != auditv1.PolicyDecision_DENY {
		t.Fatalf("published payload is not the dropped event: %v %v", got, err)
	}
}

// F28 review: a write that landed although the client saw an error counts as
// persisted, so the next event does not fork the chain.
func TestEmit_LandedWriteDespiteErrorLeavesNoFork(t *testing.T) {
	ctx := context.Background()
	e, _ := NewEmitter(ctx, Config{SigningKey: "k"})
	store := &fakeStore{objects: map[string][]byte{}, putLandsErr: errors.New("timeout after write")}
	old := putBackoff
	putBackoff = time.Millisecond
	defer func() { putBackoff = old }()
	e.logger = zap.NewNop()
	e.startSink(ctx, store, 8)
	defer e.Close()

	if err := e.Emit(ctx, ev("a", "t1")); err != nil {
		t.Fatalf("a write that landed is persisted: %v", err)
	}
	store.putLandsErr = nil
	if err := e.Emit(ctx, ev("b", "t1")); err != nil {
		t.Fatal(err)
	}
	r := &Reader{store: store, cfg: Config{SigningKey: "k"}, logger: nopLogger()}
	report, _, err := r.VerifyChain(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK || report.Total != 2 {
		t.Fatalf("want one unforked chain of 2, got %+v", report)
	}
}

// F28 review: only the ALLOW per-RPC envelope skips the wait for persistence;
// a denied envelope and every other event report a failed write.
func TestEmit_OnlyAllowedRPCEnvelopeSkipsTheWait(t *testing.T) {
	ctx := context.Background()
	e, _ := NewEmitter(ctx, Config{})
	store := &fakeStore{objects: map[string][]byte{}, putErr: errors.New("minio down")}
	old := putBackoff
	putBackoff = time.Millisecond
	defer func() { putBackoff = old }()
	e.logger = zap.NewNop()
	e.startSink(ctx, store, 8)
	defer e.Close()

	for _, tc := range []struct {
		eventType string
		decision  auditv1.PolicyDecision
		wantErr   bool
	}{
		{RPCEventType, auditv1.PolicyDecision_ALLOW, false},
		{RPCEventType, auditv1.PolicyDecision_DENY, true},
		{"aikonos.memory.verified", auditv1.PolicyDecision_ALLOW, true},
	} {
		x := ev(tc.eventType+tc.decision.String(), "t1")
		x.EventType, x.Decision = tc.eventType, tc.decision
		if err := e.Emit(ctx, x); (err != nil) != tc.wantErr {
			t.Errorf("%s %s: err=%v, want error=%v", tc.eventType, tc.decision, err, tc.wantErr)
		}
	}
}
