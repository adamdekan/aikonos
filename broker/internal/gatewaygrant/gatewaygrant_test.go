// broker/internal/gatewaygrant/gatewaygrant_test.go
// Tests for the stateless HMAC owner-grant token.
//
// Each case maps to a class of attack the grant mechanism must block:
// user impersonation, cross-tenant replay, token forgery, expiry bypass.
package gatewaygrant

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

var testKey = []byte("00000000000000000000000000000001") // 32 bytes

// roundTrip: Mint then Verify returns the same tenant and owner.
// WHY: the happy path — the gateway presents a broker-issued grant and gets
// back the identity the broker authorised, not an attacker-asserted one.
func TestRoundTrip(t *testing.T) {
	token, err := Mint(testKey, "tenant-abc", "user-xyz", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	tenant, owner, err := Verify(testKey, token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if tenant != "tenant-abc" {
		t.Errorf("tenant: got %q, want %q", tenant, "tenant-abc")
	}
	if owner != "user-xyz" {
		t.Errorf("owner: got %q, want %q", owner, "user-xyz")
	}
}

// tamperedPayload: modifying the decoded payload must be rejected with ErrBadMAC.
// WHY: a breached gateway cannot forge a different owner by mutating the payload
// without holding the HMAC key.
func TestTamperedPayload(t *testing.T) {
	token, err := Mint(testKey, "tenant-abc", "user-xyz", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// Decode the payload, flip a byte, re-encode so the base64 stays valid.
	// This exercises the MAC check path, not the base64 decode path.
	dot := strings.Index(token, ".")
	payloadBytes, decErr := base64.RawURLEncoding.DecodeString(token[:dot])
	if decErr != nil {
		t.Fatalf("decode payload: %v", decErr)
	}
	payloadBytes[3] ^= 0xFF
	tampered := base64.RawURLEncoding.EncodeToString(payloadBytes) + token[dot:]
	_, _, err = Verify(testKey, tampered)
	if !errors.Is(err, ErrBadMAC) {
		t.Errorf("tampered payload: got %v, want ErrBadMAC", err)
	}
}

// wrongKey: a token signed with a different key must be rejected with ErrBadMAC.
// WHY: tokens from a different broker (or a fabricated key) cannot be accepted.
func TestWrongKey(t *testing.T) {
	otherKey := []byte("99999999999999999999999999999999") // 32 bytes, different
	token, err := Mint(otherKey, "tenant-abc", "user-xyz", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	_, _, err = Verify(testKey, token)
	if !errors.Is(err, ErrBadMAC) {
		t.Errorf("wrong key: got %v, want ErrBadMAC", err)
	}
}

// expiredToken: a token whose TTL has elapsed must be rejected with ErrExpired.
// WHY: short TTL bounds the impersonation window if a grant is stolen; without
// expiry enforcement the grant becomes a permanent credential.
func TestExpiredToken(t *testing.T) {
	now := time.Now()
	// Mint 2 hours in the past with a 1h TTL → expired 1 hour ago.
	nowFn = func() time.Time { return now.Add(-2 * time.Hour) }
	t.Cleanup(func() { nowFn = time.Now })
	token, err := Mint(testKey, "tenant-abc", "user-xyz", time.Hour)
	nowFn = func() time.Time { return now } // restore before Verify
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	_, _, err = Verify(testKey, token)
	if !errors.Is(err, ErrExpired) {
		t.Errorf("expired token: got %v, want ErrExpired", err)
	}
}

// malformedNoDot: a token with no "." separator must be ErrMalformed.
// WHY: format contract is payload.mac; anything else is garbled input.
func TestMalformedNoDot(t *testing.T) {
	_, _, err := Verify(testKey, "nodotinthisstring")
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("no dot: got %v, want ErrMalformed", err)
	}
}

// malformedBadBase64Payload: non-base64 payload bytes must be ErrMalformed.
func TestMalformedBadBase64Payload(t *testing.T) {
	_, _, err := Verify(testKey, "!!!notbase64!!!.dmFsaWRtYWM=")
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("bad base64 payload: got %v, want ErrMalformed", err)
	}
}

// malformedBadBase64MAC: non-base64 MAC bytes must be ErrMalformed.
func TestMalformedBadBase64MAC(t *testing.T) {
	// Valid base64 payload, invalid base64 MAC.
	payload := base64.RawURLEncoding.EncodeToString([]byte("v1|t|u|9999999999"))
	_, _, err := Verify(testKey, payload+".!!!notbase64!!!")
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("bad base64 MAC: got %v, want ErrMalformed", err)
	}
}

// malformedWrongFieldCount: a payload with wrong pipe field count must be ErrMalformed.
// WHY: prevents stripped-down or extended payloads from being misinterpreted.
func TestMalformedWrongFieldCount(t *testing.T) {
	badPayload := []byte("v1|tenant|owner") // 3 fields, needs 4
	token := buildRawToken(testKey, badPayload)
	_, _, err := Verify(testKey, token)
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("wrong field count: got %v, want ErrMalformed", err)
	}
}

// malformedWrongVersion: a non-"v1" version prefix must be ErrMalformed.
// WHY: version gate blocks future format changes from being misinterpreted.
func TestMalformedWrongVersion(t *testing.T) {
	badPayload := []byte("v2|tenant|owner|9999999999")
	token := buildRawToken(testKey, badPayload)
	_, _, err := Verify(testKey, token)
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("wrong version: got %v, want ErrMalformed", err)
	}
}

// wrongMACCorrectLength: a MAC of correct length but wrong bytes must be rejected.
// WHY: asserts constant-time comparison — the check must not be short-circuited
// by a length check that could be used as a timing oracle.
func TestWrongMACCorrectLength(t *testing.T) {
	token, err := Mint(testKey, "tenant-abc", "user-xyz", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	dot := strings.LastIndex(token, ".")
	if dot < 0 {
		t.Fatal("no dot in minted token")
	}
	// Replace MAC with 32 zero bytes (same length as SHA-256 HMAC, different value).
	fakeMAC := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	tampered := token[:dot+1] + fakeMAC
	_, _, err = Verify(testKey, tampered)
	if !errors.Is(err, ErrBadMAC) {
		t.Errorf("wrong MAC correct length: got %v, want ErrBadMAC", err)
	}
}

// buildRawToken signs an arbitrary payload with testKey and returns a token
// string. Used to craft structurally valid but semantically wrong tokens.
func buildRawToken(key, payload []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(payload)
	sig := mac.Sum(nil)
	return fmt.Sprintf("%s.%s",
		base64.RawURLEncoding.EncodeToString(payload),
		base64.RawURLEncoding.EncodeToString(sig),
	)
}
