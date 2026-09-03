// broker/internal/gatewaygrant/gatewaygrant.go
// Stateless HMAC-signed owner-grant tokens.
//
// A grant token lets the broker delegate a short-lived, owner-locked credential
// to the agent-gateway so the gateway can drive CreateGatewayTask without
// asserting an arbitrary owner. The broker mints the grant at claim/resolve
// time; the gateway echoes it back on every south call within the TTL.
//
// Token format: base64url(payload) + "." + base64url(HMAC_SHA256(key, payload))
// Payload (canonical, pipe-joined): v1|<tenantID>|<ownerUserID>|<expiresUnix>
package gatewaygrant

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Sentinel errors — callers use errors.Is.
var (
	// ErrMalformed is returned when the token is structurally invalid (wrong
	// format, bad base64, wrong version, wrong field count).
	ErrMalformed = errors.New("gatewaygrant: malformed token")
	// ErrBadMAC is returned when the HMAC does not match (wrong key or tampered
	// payload). Always surface as PermissionDenied — a bad grant is an authz
	// failure, not an internal error.
	ErrBadMAC = errors.New("gatewaygrant: bad MAC")
	// ErrExpired is returned when the token's expiry has passed.
	ErrExpired = errors.New("gatewaygrant: token expired")
)

// nowFn returns the current time. Overridden in tests to make expiry testable
// without sleeping.
var nowFn = func() time.Time { return time.Now() }

// Mint builds a signed owner-grant token.
// key must be 32 bytes (callers obtain it from the Vault bootstrap).
// ttl is the validity window; the gateway must present the grant within this
// window — each tool-call creates a new task but reuses the same grant, so ttl
// should cover a full multi-step agent loop (default 1h).
func Mint(key []byte, tenantID, ownerUserID string, ttl time.Duration) (string, error) {
	if len(key) == 0 {
		return "", fmt.Errorf("gatewaygrant: Mint: empty key")
	}
	expires := nowFn().Add(ttl).Unix()
	payload := []byte(fmt.Sprintf("v1|%s|%s|%d", tenantID, ownerUserID, expires))
	mac := computeMAC(key, payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac), nil
}

// Verify parses and validates a grant token. On success it returns the
// (tenantID, ownerUserID) the broker originally bound. On failure it returns
// one of ErrMalformed, ErrBadMAC, or ErrExpired — all map to PermissionDenied
// at the RPC layer.
func Verify(key []byte, token string) (tenantID, ownerUserID string, err error) {
	dot := strings.Index(token, ".")
	if dot < 0 {
		return "", "", ErrMalformed
	}

	payloadB64 := token[:dot]
	macB64 := token[dot+1:]

	payloadBytes, decErr := base64.RawURLEncoding.DecodeString(payloadB64)
	if decErr != nil {
		return "", "", ErrMalformed
	}
	macBytes, decErr := base64.RawURLEncoding.DecodeString(macB64)
	if decErr != nil {
		return "", "", ErrMalformed
	}

	// Constant-time MAC check — must happen before parsing to avoid leaking
	// structural information on bad-MAC paths.
	expected := computeMAC(key, payloadBytes)
	if !hmac.Equal(expected, macBytes) {
		return "", "", ErrBadMAC
	}

	// MAC is valid; parse the payload.
	parts := strings.Split(string(payloadBytes), "|")
	if len(parts) != 4 {
		return "", "", ErrMalformed
	}
	if parts[0] != "v1" {
		return "", "", ErrMalformed
	}

	expiresUnix, parseErr := strconv.ParseInt(parts[3], 10, 64)
	if parseErr != nil {
		return "", "", ErrMalformed
	}
	if expiresUnix <= nowFn().Unix() {
		return "", "", ErrExpired
	}

	return parts[1], parts[2], nil
}

func computeMAC(key, payload []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(payload)
	return h.Sum(nil)
}
