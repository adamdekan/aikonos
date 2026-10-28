// broker/internal/gatewaygrant/gatewaygrant.go
// Stateless HMAC-signed owner-grant tokens.
//
// A grant token lets the broker delegate a short-lived, owner-locked credential
// to the agent-gateway so the gateway can drive CreateGatewayTask without
// asserting an arbitrary owner. The broker mints the grant at claim/resolve
// time; the gateway echoes it back on every south call within the TTL.
//
// Token format: base64url(payload) + "." + base64url(HMAC_SHA256(key, payload))
// Payload (canonical, pipe-joined):
//
//	v2|<tenant>|<subject>|<actor>|<kind>|<grantID>|<expiresUnix>
//
// Legacy v1|<tenantID>|<ownerUserID>|<expiresUnix> is accepted by Verify only
// when acceptV1 is set (AG-17 rollout window).
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

// Grant kinds (AG-17). KindLegacy marks a verified v1 token.
const (
	KindInteractive = "interactive"
	KindScheduled   = "scheduled"
	KindExternal    = "external"
	KindWorkflow    = "workflow"
	KindLegacy      = "legacy"
)

// Grant is the on-behalf-of context a token carries. Subject is the human the
// run acts for ("" for a service run); Actor is the agent UUID ("" when
// unknown). GrantID names the standing authority behind the grant: the
// scheduled_runs id for KindScheduled, the api_keys id for KindExternal.
type Grant struct {
	TenantID string
	Subject  string
	Actor    string
	Kind     string
	GrantID  string
	// Expires is set by Verify from the token; Mint ignores it.
	Expires time.Time
}

// Owner is the task owner_user_id this grant binds: Subject, or svc-<actor>
// for a subject-less (service) run — the same encoding db.Task.OnBehalfOf reads.
func (g Grant) Owner() string {
	if g.Subject == "" && g.Actor != "" {
		return "svc-" + g.Actor
	}
	return g.Subject
}

// Mint builds a signed owner-grant token.
// key must be 32 bytes (callers obtain it from the Vault bootstrap).
// ttl is the validity window; the gateway must present the grant within this
// window — each tool-call creates a new task but reuses the same grant, so ttl
// should cover a full multi-step agent loop (default 1h).
func Mint(key []byte, g Grant, ttl time.Duration) (string, error) {
	if len(key) == 0 {
		return "", fmt.Errorf("gatewaygrant: Mint: empty key")
	}
	for _, f := range []string{g.TenantID, g.Subject, g.Actor, g.Kind, g.GrantID} {
		if strings.Contains(f, "|") {
			return "", fmt.Errorf("gatewaygrant: Mint: field contains delimiter")
		}
	}
	expires := nowFn().Add(ttl).Unix()
	payload := []byte(fmt.Sprintf("v2|%s|%s|%s|%s|%s|%d", g.TenantID, g.Subject, g.Actor, g.Kind, g.GrantID, expires))
	mac := computeMAC(key, payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac), nil
}

// Verify parses and validates a grant token and returns the Grant the broker
// originally bound. A v1 token is accepted only when acceptV1 is true and maps
// to {Subject: owner, Kind: KindLegacy}. On failure it returns one of
// ErrMalformed, ErrBadMAC, or ErrExpired — all map to PermissionDenied at the
// RPC layer.
func Verify(key []byte, token string, acceptV1 bool) (Grant, error) {
	dot := strings.Index(token, ".")
	if dot < 0 {
		return Grant{}, ErrMalformed
	}

	payloadB64 := token[:dot]
	macB64 := token[dot+1:]

	payloadBytes, decErr := base64.RawURLEncoding.DecodeString(payloadB64)
	if decErr != nil {
		return Grant{}, ErrMalformed
	}
	macBytes, decErr := base64.RawURLEncoding.DecodeString(macB64)
	if decErr != nil {
		return Grant{}, ErrMalformed
	}

	// Constant-time MAC check — must happen before parsing to avoid leaking
	// structural information on bad-MAC paths.
	expected := computeMAC(key, payloadBytes)
	if !hmac.Equal(expected, macBytes) {
		return Grant{}, ErrBadMAC
	}

	// MAC is valid; parse the payload.
	parts := strings.Split(string(payloadBytes), "|")
	var g Grant
	switch {
	case len(parts) == 7 && parts[0] == "v2":
		g = Grant{TenantID: parts[1], Subject: parts[2], Actor: parts[3], Kind: parts[4], GrantID: parts[5]}
	case len(parts) == 4 && parts[0] == "v1" && acceptV1:
		g = Grant{TenantID: parts[1], Subject: parts[2], Kind: KindLegacy}
	default:
		return Grant{}, ErrMalformed
	}

	expiresUnix, parseErr := strconv.ParseInt(parts[len(parts)-1], 10, 64)
	if parseErr != nil {
		return Grant{}, ErrMalformed
	}
	if expiresUnix <= nowFn().Unix() {
		return Grant{}, ErrExpired
	}
	g.Expires = time.Unix(expiresUnix, 0)

	return g, nil
}

func computeMAC(key, payload []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(payload)
	return h.Sum(nil)
}
