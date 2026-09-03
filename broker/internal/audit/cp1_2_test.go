package audit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"

	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
)

// fakeKeySource is a test double for SigningKeySource, standing in for a
// Vault-backed source without any network I/O — proves the emitter and
// VerifyChainVersioned depend only on the interface (CP1.2).
type fakeKeySource struct {
	currentKey     string
	currentVersion int32
	byVersion      map[int32]string // historic versions resolvable by ForVersion
	forVersionErr  error            // when set, ForVersion always fails with this
	calls          []int32          // records every ForVersion call, for round-trip counting
}

func (f *fakeKeySource) Current() (string, int32) { return f.currentKey, f.currentVersion }

func (f *fakeKeySource) ForVersion(_ context.Context, version int32) (string, error) {
	f.calls = append(f.calls, version)
	if f.forVersionErr != nil {
		return "", f.forVersionErr
	}
	key, ok := f.byVersion[version]
	if !ok {
		return "", fmt.Errorf("fakeKeySource: no key for version %d", version)
	}
	return key, nil
}

// ── Emitter + fake key source ────────────────────────────────────────────────

// TestEmitter_RecordsKeySourceVersion proves Emit records the key source's
// current version on the event, and signs with the source's key (not a
// hardcoded static one) — the wiring point Vault-backed rotation depends on.
func TestEmitter_RecordsKeySourceVersion(t *testing.T) {
	src := &fakeKeySource{currentKey: "vault-key-v3", currentVersion: 3, byVersion: map[int32]string{3: "vault-key-v3"}}
	e, err := NewEmitter(context.Background(), Config{SigningKeySource: src})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}

	event := &auditv1.AuditEvent{EventId: "evt-1", TenantId: "t1", EventType: "test.event"}
	if err := e.Emit(context.Background(), event); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	if event.SigningKeyVersion != 3 {
		t.Fatalf("want signing_key_version=3, got %d", event.SigningKeyVersion)
	}
	if event.Signature == "" {
		t.Fatal("want a non-empty signature")
	}

	// Recompute the HMAC independently from the source's key over the
	// canonical form, proving the signature is keyed from the source, not a
	// hardcoded value.
	canon := canonicalBytes(event)
	mac := hmac.New(sha256.New, []byte("vault-key-v3"))
	mac.Write(canon)
	want := hex.EncodeToString(mac.Sum(nil))
	if event.Signature != want {
		t.Fatalf("signature not keyed from the fake source's key: got %s want %s", event.Signature, want)
	}
}

// ── VerifyChainVersioned spanning a rotation ─────────────────────────────────

// buildVersionedChain emits n events through a real Emitter driven by src, so
// PriorEventHash/Signature/SigningKeyVersion are populated exactly as in
// production — src.currentVersion can be mutated between calls to simulate a
// rotation mid-chain.
func buildVersionedChain(t *testing.T, e *Emitter, ids []string) []*auditv1.AuditEvent {
	t.Helper()
	events := make([]*auditv1.AuditEvent, len(ids))
	for i, id := range ids {
		events[i] = &auditv1.AuditEvent{EventId: id, TenantId: "tenant1", EventType: "test.event"}
		if err := e.Emit(context.Background(), events[i]); err != nil {
			t.Fatalf("Emit[%d]: %v", i, err)
		}
	}
	return events
}

// TestVerifyChainVersioned_SpansRotation proves a chain with events signed
// under version 1 followed by events signed under version 2 (a rotation
// mid-chain) verifies cleanly when both versions resolve via the key source —
// and that each version is resolved from the source only once (call-scoped
// cache, minimal Vault round-trips).
func TestVerifyChainVersioned_SpansRotation(t *testing.T) {
	src := &fakeKeySource{
		currentKey: "key-v1", currentVersion: 1,
		byVersion: map[int32]string{1: "key-v1", 2: "key-v2"},
	}
	e, err := NewEmitter(context.Background(), Config{SigningKeySource: src})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	pre := buildVersionedChain(t, e, []string{"evt-0000", "evt-0001"})

	// Rotate: the source now signs new events under version 2.
	src.currentKey, src.currentVersion = "key-v2", 2
	post := buildVersionedChain(t, e, []string{"evt-0002", "evt-0003"})

	all := append(pre, post...)
	report := VerifyChainVersioned(context.Background(), all, src)

	if !report.OK {
		t.Fatalf("rotation-spanning chain: want ok=true; breaks=%v sig_failures=%v", report.Breaks, report.SignatureFailures)
	}
	if !report.Signed {
		t.Error("want signed=true")
	}
	if all[0].SigningKeyVersion != 1 || all[1].SigningKeyVersion != 1 {
		t.Fatalf("want first two events at version 1, got %d, %d", all[0].SigningKeyVersion, all[1].SigningKeyVersion)
	}
	if all[2].SigningKeyVersion != 2 || all[3].SigningKeyVersion != 2 {
		t.Fatalf("want last two events at version 2, got %d, %d", all[2].SigningKeyVersion, all[3].SigningKeyVersion)
	}

	// Each distinct version (1 and 2) resolved from the source at most once,
	// despite 4 events spanning both.
	seen := map[int32]int{}
	for _, v := range src.calls {
		seen[v]++
	}
	for v, n := range seen {
		if n > 1 {
			t.Errorf("version %d resolved %d times, want at most 1 (call-scoped cache)", v, n)
		}
	}
}

// TestVerifyChainVersioned_TamperedEventFails proves a tampered event inside a
// rotation-spanning chain is still caught (signature check remains sound per
// version, not just per whole-chain single key).
func TestVerifyChainVersioned_TamperedEventFails(t *testing.T) {
	src := &fakeKeySource{
		currentKey: "key-v1", currentVersion: 1,
		byVersion: map[int32]string{1: "key-v1", 2: "key-v2"},
	}
	e, err := NewEmitter(context.Background(), Config{SigningKeySource: src})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	pre := buildVersionedChain(t, e, []string{"evt-0000", "evt-0001"})
	src.currentKey, src.currentVersion = "key-v2", 2
	post := buildVersionedChain(t, e, []string{"evt-0002", "evt-0003"})
	all := append(pre, post...)

	// Tamper the second event (still version 1) after emission.
	all[1].EventType = "tampered.event"

	report := VerifyChainVersioned(context.Background(), all, src)
	if report.OK {
		t.Error("tampered event inside a rotation-spanning chain: want ok=false")
	}
	if len(report.Breaks) == 0 {
		t.Error("want at least one continuity break from the tampered event")
	}
}

// TestVerifyChainVersioned_SignatureDowngradeRejected proves the attack this
// checkpoint's reviewer flagged: in a chain where events are actually
// Vault-signed (version 1, non-empty key), rewriting one event's
// signing_key_version to 0 must NOT let it skip the signature check just
// because version 0 resolves to an empty key. Since the chain has real
// signing elsewhere (anySigned=true), the downgraded event is a
// SignatureFailure, not a silent pass.
func TestVerifyChainVersioned_SignatureDowngradeRejected(t *testing.T) {
	src := &fakeKeySource{
		currentKey: "key-v1", currentVersion: 1,
		byVersion: map[int32]string{1: "key-v1", 0: ""}, // version 0 = env fallback, unset
	}
	e, err := NewEmitter(context.Background(), Config{SigningKeySource: src})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	events := buildVersionedChain(t, e, []string{"evt-0000", "evt-0001", "evt-0002"})

	// Attacker rewrites the middle event's recorded version to 0 (which this
	// source resolves to an empty/unset key) to try to dodge signature
	// verification, without touching the signature bytes themselves.
	events[1].SigningKeyVersion = 0

	report := VerifyChainVersioned(context.Background(), events, src)

	if report.OK {
		t.Fatal("signature-downgrade via signing_key_version rewrite: want ok=false")
	}
	found := false
	for _, f := range report.SignatureFailures {
		if f.EventID == events[1].EventId {
			found = true
		}
	}
	if !found {
		t.Errorf("want a SignatureFailure for the downgraded event %s, got: %v", events[1].EventId, report.SignatureFailures)
	}
	if !report.Signed {
		t.Error("want signed=true — the chain has real (non-empty-key) signing elsewhere")
	}
}

// TestVerifyChainVersioned_WhollyUnsignedChainStillPasses proves the
// downgrade-rejection fix doesn't regress the legitimate legacy case: a chain
// where every event resolves to an empty key (no Vault, no env key ever
// configured) still verifies cleanly and reports Signed=false.
func TestVerifyChainVersioned_WhollyUnsignedChainStillPasses(t *testing.T) {
	src := &fakeKeySource{currentKey: "", currentVersion: 0, byVersion: map[int32]string{0: ""}}
	e, err := NewEmitter(context.Background(), Config{SigningKeySource: src})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	events := buildVersionedChain(t, e, []string{"evt-0000", "evt-0001", "evt-0002"})

	report := VerifyChainVersioned(context.Background(), events, src)

	if !report.OK {
		t.Errorf("wholly-unsigned chain: want ok=true; sig_failures=%v", report.SignatureFailures)
	}
	if report.Signed {
		t.Error("want signed=false for a wholly-unsigned chain")
	}
}

// TestVerifyChainVersioned_UnresolvableVersionIsFailure proves an event whose
// recorded signing_key_version can't be resolved (e.g. a Vault historic read
// failure) is reported as a signature failure, not silently skipped.
func TestVerifyChainVersioned_UnresolvableVersionIsFailure(t *testing.T) {
	src := &fakeKeySource{currentKey: "key-v1", currentVersion: 1, byVersion: map[int32]string{1: "key-v1"}}
	e, err := NewEmitter(context.Background(), Config{SigningKeySource: src})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	events := buildVersionedChain(t, e, []string{"evt-0000"})

	// Verify with a source that can no longer resolve version 1 (e.g. Vault
	// unreachable for a historic read).
	broken := &fakeKeySource{forVersionErr: errors.New("vault: historic read failed")}
	report := VerifyChainVersioned(context.Background(), events, broken)

	if report.OK {
		t.Error("unresolvable signing key version: want ok=false")
	}
	if len(report.SignatureFailures) != 1 {
		t.Fatalf("want 1 signature failure for the unresolvable version, got %d: %v", len(report.SignatureFailures), report.SignatureFailures)
	}
}

// ── Fallback: key source unavailable at construction time ───────────────────

// TestEmitter_FallsBackToStaticKeyWhenSourceNil proves that when
// Config.SigningKeySource is nil (the wiring layer couldn't build a
// Vault-backed source and fell back to the env key), the emitter signs with
// Config.SigningKey at version 0 — the fallback path CP1.2 requires main.go
// to hit when Vault is unavailable.
func TestEmitter_FallsBackToStaticKeyWhenSourceNil(t *testing.T) {
	e, err := NewEmitter(context.Background(), Config{SigningKey: "env-fallback-key"})
	if err != nil {
		t.Fatalf("NewEmitter: %v", err)
	}
	event := &auditv1.AuditEvent{EventId: "evt-1", TenantId: "t1", EventType: "test.event"}
	if err := e.Emit(context.Background(), event); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	if event.SigningKeyVersion != 0 {
		t.Fatalf("want signing_key_version=0 in fallback mode, got %d", event.SigningKeyVersion)
	}
	canon := canonicalBytes(event)
	mac := hmac.New(sha256.New, []byte("env-fallback-key"))
	mac.Write(canon)
	want := hex.EncodeToString(mac.Sum(nil))
	if event.Signature != want {
		t.Fatalf("fallback signature mismatch: got %s want %s", event.Signature, want)
	}

	// VerifyChain (the legacy, non-versioned entry point) must accept it.
	report := VerifyChain([]*auditv1.AuditEvent{event}, "env-fallback-key")
	if !report.OK {
		t.Errorf("fallback-signed event: want ok=true; sig_failures=%v", report.SignatureFailures)
	}
}
