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

var testGrant = Grant{TenantID: "tenant-abc", Subject: "user-xyz", Actor: "agent-1", Kind: KindScheduled, GrantID: "sched-1"}

// roundTrip: Mint then Verify returns the same Grant.
// WHY: the happy path — the gateway presents a broker-issued grant and gets
// back the identity the broker authorised, not an attacker-asserted one.
func TestRoundTrip(t *testing.T) {
	token, err := Mint(testKey, testGrant, time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	for _, acceptV1 := range []bool{true, false} {
		g, err := Verify(testKey, token, acceptV1)
		if err != nil {
			t.Fatalf("Verify(acceptV1=%v): %v", acceptV1, err)
		}
		if exp := g.Expires; exp.Before(time.Now().Add(59*time.Minute)) || exp.After(time.Now().Add(time.Hour)) {
			t.Errorf("Expires %v, want ~1h ahead", exp)
		}
		if iat := g.IssuedAt; iat.Before(time.Now().Add(-time.Minute)) || iat.After(time.Now()) {
			t.Errorf("IssuedAt %v, want ~now", iat)
		}
		g.Expires, g.IssuedAt = time.Time{}, time.Time{}
		if g != testGrant {
			t.Errorf("got %+v, want %+v", g, testGrant)
		}
	}
}

// v1 tokens verify only while accept_v1 is on, and map to a legacy Grant.
func TestV1AcceptedOnlyWithFlag(t *testing.T) {
	token := buildRawToken(testKey, []byte("v1|tenant-abc|svc-a1|9999999999"))
	g, err := Verify(testKey, token, true)
	if err != nil {
		t.Fatalf("Verify(acceptV1): %v", err)
	}
	if want := (Grant{TenantID: "tenant-abc", Subject: "svc-a1", Kind: KindLegacy, Expires: time.Unix(9999999999, 0)}); g != want {
		t.Errorf("got %+v, want %+v", g, want)
	}
	if g.Owner() != "svc-a1" {
		t.Errorf("legacy owner: got %q", g.Owner())
	}
	if _, err := Verify(testKey, token, false); !errors.Is(err, ErrMalformed) {
		t.Errorf("v1 with acceptV1=false: got %v, want ErrMalformed", err)
	}
}

// A delimiter in any field would shift the v2 fields; Mint must refuse it.
func TestMintRejectsDelimiter(t *testing.T) {
	for _, g := range []Grant{
		{TenantID: "t|x"}, {Subject: "u|x"}, {Actor: "a|x"}, {Kind: "k|x"}, {GrantID: "g|x"},
	} {
		if _, err := Mint(testKey, g, time.Hour); err == nil {
			t.Errorf("Mint(%+v): want error", g)
		}
	}
}

// Owner keeps the svc-<agent> encoding for subject-less (service) grants.
func TestOwner(t *testing.T) {
	if got := (Grant{Actor: "a1", Kind: KindExternal}).Owner(); got != "svc-a1" {
		t.Errorf("service owner: got %q", got)
	}
	if got := testGrant.Owner(); got != "user-xyz" {
		t.Errorf("subject owner: got %q", got)
	}
}

// tamperedPayload: modifying the decoded payload must be rejected with ErrBadMAC.
// WHY: a breached gateway cannot forge a different owner by mutating the payload
// without holding the HMAC key.
func TestTamperedPayload(t *testing.T) {
	token, err := Mint(testKey, testGrant, time.Hour)
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
	_, err = Verify(testKey, tampered, true)
	if !errors.Is(err, ErrBadMAC) {
		t.Errorf("tampered payload: got %v, want ErrBadMAC", err)
	}
}

// wrongKey: a token signed with a different key must be rejected with ErrBadMAC.
// WHY: tokens from a different broker (or a fabricated key) cannot be accepted.
func TestWrongKey(t *testing.T) {
	otherKey := []byte("99999999999999999999999999999999") // 32 bytes, different
	token, err := Mint(otherKey, testGrant, time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	_, err = Verify(testKey, token, true)
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
	token, err := Mint(testKey, testGrant, time.Hour)
	nowFn = func() time.Time { return now } // restore before Verify
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	_, err = Verify(testKey, token, true)
	if !errors.Is(err, ErrExpired) {
		t.Errorf("expired token: got %v, want ErrExpired", err)
	}
}

// malformedNoDot: a token with no "." separator must be ErrMalformed.
// WHY: format contract is payload.mac; anything else is garbled input.
func TestMalformedNoDot(t *testing.T) {
	_, err := Verify(testKey, "nodotinthisstring", true)
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("no dot: got %v, want ErrMalformed", err)
	}
}

// malformedBadBase64Payload: non-base64 payload bytes must be ErrMalformed.
func TestMalformedBadBase64Payload(t *testing.T) {
	_, err := Verify(testKey, "!!!notbase64!!!.dmFsaWRtYWM=", true)
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("bad base64 payload: got %v, want ErrMalformed", err)
	}
}

// malformedBadBase64MAC: non-base64 MAC bytes must be ErrMalformed.
func TestMalformedBadBase64MAC(t *testing.T) {
	// Valid base64 payload, invalid base64 MAC.
	payload := base64.RawURLEncoding.EncodeToString([]byte("v1|t|u|9999999999"))
	_, err := Verify(testKey, payload+".!!!notbase64!!!", true)
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("bad base64 MAC: got %v, want ErrMalformed", err)
	}
}

// malformedWrongFieldCount: a payload with wrong pipe field count must be ErrMalformed.
// WHY: prevents stripped-down or extended payloads from being misinterpreted.
func TestMalformedWrongFieldCount(t *testing.T) {
	badPayload := []byte("v2|tenant|owner|9999999999") // 4 fields, v2 needs 7
	token := buildRawToken(testKey, badPayload)
	_, err := Verify(testKey, token, true)
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("wrong field count: got %v, want ErrMalformed", err)
	}
}

// malformedWrongVersion: an unknown version prefix must be ErrMalformed.
// WHY: version gate blocks future format changes from being misinterpreted.
func TestMalformedWrongVersion(t *testing.T) {
	badPayload := []byte("v4|tenant|subject|actor|kind|grant|1|9999999999")
	token := buildRawToken(testKey, badPayload)
	_, err := Verify(testKey, token, true)
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("wrong version: got %v, want ErrMalformed", err)
	}
}

// wrongMACCorrectLength: a MAC of correct length but wrong bytes must be rejected.
// WHY: asserts constant-time comparison — the check must not be short-circuited
// by a length check that could be used as a timing oracle.
func TestWrongMACCorrectLength(t *testing.T) {
	token, err := Mint(testKey, testGrant, time.Hour)
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
	_, err = Verify(testKey, tampered, true)
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

// A v2 token minted before the v3 upgrade still verifies, with no IssuedAt.
func TestV2StillAccepted(t *testing.T) {
	token := buildRawToken(testKey, []byte("v2|tenant-abc|user-xyz|agent-1|scheduled|sched-1|9999999999"))
	g, err := Verify(testKey, token, false)
	if err != nil {
		t.Fatalf("Verify v2: %v", err)
	}
	if !g.IssuedAt.IsZero() || g.Subject != "user-xyz" || !g.Expires.Equal(time.Unix(9999999999, 0)) {
		t.Errorf("v2 grant = %+v", g)
	}
}
