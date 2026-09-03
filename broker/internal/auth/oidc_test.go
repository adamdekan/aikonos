package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// testIDP spins up an httptest server exposing an OIDC discovery doc + JWKS for
// a freshly generated RSA key, and returns a signer bound to that key.
type testIDP struct {
	srv    *httptest.Server
	signer jose.Signer
	kid    string
}

func newTestIDP(t *testing.T) *testIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	const kid = "test-key-1"

	pub := jose.JSONWebKey{Key: &key.PublicKey, KeyID: kid, Algorithm: "RS256", Use: "sig"}
	jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{pub}}

	mux := http.NewServeMux()
	idp := &testIDP{kid: kid}
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":   idp.srv.URL,
			"jwks_uri": idp.srv.URL + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jwks)
	})
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid),
	)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	idp.signer = signer
	return idp
}

func (idp *testIDP) mint(t *testing.T, claims jwt.Claims, extra map[string]any) string {
	t.Helper()
	tok, err := jwt.Signed(idp.signer).Claims(claims).Claims(extra).Serialize()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return tok
}

// mustValidator wraps NewValidator for callers that expect construction to
// succeed (every OIDCConfig{} used across this package's tests is either
// fully disabled or carries a non-empty Audience alongside its Issuer).
func mustValidator(t *testing.T, cfg OIDCConfig) *Validator {
	t.Helper()
	v, err := NewValidator(cfg)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	return v
}

// TestNewValidator_EmptyAudienceFailsClosed verifies CP2 fix 1: an enabled
// validator (Issuer set) with no Audience must refuse to construct rather
// than silently validating issuer+expiry only.
func TestNewValidator_EmptyAudienceFailsClosed(t *testing.T) {
	if _, err := NewValidator(OIDCConfig{Issuer: "https://idp.example.test"}); err == nil {
		t.Fatal("expected an error when Issuer is set but Audience is empty")
	}
}

// TestNewValidator_DisabledAllowsEmptyAudience verifies the dev-mode path
// (Issuer unset) is untouched by the fail-closed audience check.
func TestNewValidator_DisabledAllowsEmptyAudience(t *testing.T) {
	v, err := NewValidator(OIDCConfig{})
	if err != nil {
		t.Fatalf("disabled validator (no issuer) must still construct: %v", err)
	}
	if v.Enabled() {
		t.Fatal("validator with empty issuer should be disabled")
	}
}

func TestValidator_Disabled(t *testing.T) {
	v := mustValidator(t, OIDCConfig{})
	if v.Enabled() {
		t.Fatal("validator with empty issuer should be disabled")
	}
	if _, err := v.Validate(context.Background(), "anything"); err == nil {
		t.Fatal("disabled validator should refuse to validate")
	}
}

func TestValidator_ValidToken(t *testing.T) {
	idp := newTestIDP(t)
	v := mustValidator(t, OIDCConfig{
		Issuer:      idp.srv.URL,
		Audience:    "aikonos-broker",
		TenantClaim: "tenant_id",
	})

	tok := idp.mint(t, jwt.Claims{
		Issuer:   idp.srv.URL,
		Subject:  "alice@example.com",
		Audience: jwt.Audience{"aikonos-broker"},
		Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
		IssuedAt: jwt.NewNumericDate(time.Now()),
	}, map[string]any{
		"email":     "alice@example.com",
		"tenant_id": "11111111-1111-1111-1111-111111111111",
		"scope":     "openid task.create task.approve",
	})

	id, err := v.Validate(context.Background(), tok)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if id.Subject != "alice@example.com" {
		t.Errorf("subject = %q", id.Subject)
	}
	if id.TenantID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("tenant = %q", id.TenantID)
	}
	if len(id.Scopes) != 3 {
		t.Errorf("scopes = %v", id.Scopes)
	}
}

// TestValidator_EntraClaims: with SubjectClaim "oid" and TenantClaim "tid"
// (the Entra mapping), the principal + tenant come from those claims, not the
// pairwise-per-app "sub". This is what makes the Entra swap config-only.
func TestValidator_EntraClaims(t *testing.T) {
	idp := newTestIDP(t)
	v := mustValidator(t, OIDCConfig{
		Issuer:       idp.srv.URL,
		Audience:     "api://aikonos-broker",
		SubjectClaim: "oid",
		TenantClaim:  "tid",
	})

	const oid = "00000000-aaaa-bbbb-cccc-111111111111"
	const tid = "22222222-3333-4444-5555-666666666666"
	tok := idp.mint(t, jwt.Claims{
		Issuer:   idp.srv.URL,
		Subject:  "pairwise-per-app-sub-do-not-use",
		Audience: jwt.Audience{"api://aikonos-broker"},
		Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
		IssuedAt: jwt.NewNumericDate(time.Now()),
	}, map[string]any{
		"oid":                oid,
		"tid":                tid,
		"preferred_username": "alice@contoso.com",
	})

	id, err := v.Validate(context.Background(), tok)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if id.Subject != oid {
		t.Errorf("subject = %q, want oid %q (not the pairwise sub)", id.Subject, oid)
	}
	if id.TenantID != tid {
		t.Errorf("tenant = %q, want tid %q", id.TenantID, tid)
	}
	if id.Email != "alice@contoso.com" {
		t.Errorf("email = %q, want preferred_username fallback", id.Email)
	}
}

func TestValidator_WrongAudience(t *testing.T) {
	idp := newTestIDP(t)
	v := mustValidator(t, OIDCConfig{Issuer: idp.srv.URL, Audience: "aikonos-broker"})

	tok := idp.mint(t, jwt.Claims{
		Issuer:   idp.srv.URL,
		Subject:  "alice@example.com",
		Audience: jwt.Audience{"some-other-app"},
		Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}, nil)

	if _, err := v.Validate(context.Background(), tok); err == nil {
		t.Fatal("expected audience mismatch to fail")
	}
}

func TestValidator_Expired(t *testing.T) {
	idp := newTestIDP(t)
	v := mustValidator(t, OIDCConfig{Issuer: idp.srv.URL, Audience: "aikonos-broker"})

	tok := idp.mint(t, jwt.Claims{
		Issuer:   idp.srv.URL,
		Subject:  "alice@example.com",
		Audience: jwt.Audience{"aikonos-broker"},
		Expiry:   jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
	}, nil)

	if _, err := v.Validate(context.Background(), tok); err == nil {
		t.Fatal("expected expired token to fail")
	}
}

func TestValidator_UnknownKey(t *testing.T) {
	idp := newTestIDP(t)
	// A second IdP signs with a key the first IdP's JWKS doesn't publish.
	other := newTestIDP(t)
	v := mustValidator(t, OIDCConfig{Issuer: idp.srv.URL, Audience: "aikonos-broker"})

	tok := other.mint(t, jwt.Claims{
		Issuer:   idp.srv.URL,
		Subject:  "mallory@example.com",
		Audience: jwt.Audience{"aikonos-broker"},
		Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}, nil)

	if _, err := v.Validate(context.Background(), tok); err == nil {
		t.Fatal("expected token signed by unknown key to fail")
	}
}

func TestValidator_Malformed(t *testing.T) {
	idp := newTestIDP(t)
	v := mustValidator(t, OIDCConfig{Issuer: idp.srv.URL, Audience: "aikonos-broker"})
	if _, err := v.Validate(context.Background(), "not-a-jwt"); err == nil {
		t.Fatal("expected malformed token to fail")
	}
	if _, err := v.Validate(context.Background(), ""); err == nil {
		t.Fatal("expected empty token to fail")
	}
}

// TestValidator_AzpMismatch_RejectedOnlyWhenConfigured verifies CP2 fix 2:
// a token whose azp doesn't match AuthorizedParty is rejected only when
// AuthorizedParty is set; the same token is accepted when it's unset.
func TestValidator_AzpMismatch_RejectedOnlyWhenConfigured(t *testing.T) {
	idp := newTestIDP(t)
	mint := func() string {
		return idp.mint(t, jwt.Claims{
			Issuer:   idp.srv.URL,
			Subject:  "alice@example.com",
			Audience: jwt.Audience{"aikonos-broker"},
			Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
		}, map[string]any{"azp": "some-other-client"})
	}

	strict := mustValidator(t, OIDCConfig{Issuer: idp.srv.URL, Audience: "aikonos-broker", AuthorizedParty: "aikonos-webui"})
	if _, err := strict.Validate(context.Background(), mint()); err == nil {
		t.Fatal("expected azp mismatch to be rejected when AuthorizedParty is set")
	}

	lenient := mustValidator(t, OIDCConfig{Issuer: idp.srv.URL, Audience: "aikonos-broker"})
	if _, err := lenient.Validate(context.Background(), mint()); err != nil {
		t.Fatalf("expected no azp requirement when AuthorizedParty is unset: %v", err)
	}
}

// TestValidator_NoAzpClaim_RejectedWhenConfigured verifies a token that
// carries no azp claim at all (not merely a mismatched one) is rejected when
// AuthorizedParty is configured — fail closed, not "no claim to check".
func TestValidator_NoAzpClaim_RejectedWhenConfigured(t *testing.T) {
	idp := newTestIDP(t)
	v := mustValidator(t, OIDCConfig{Issuer: idp.srv.URL, Audience: "aikonos-broker", AuthorizedParty: "aikonos-webui"})

	tok := idp.mint(t, jwt.Claims{
		Issuer:   idp.srv.URL,
		Subject:  "alice@example.com",
		Audience: jwt.Audience{"aikonos-broker"},
		Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}, nil)

	if _, err := v.Validate(context.Background(), tok); err == nil {
		t.Fatal("expected token with no azp claim to be rejected when AuthorizedParty is set")
	}
}

// TestValidator_AzpMatch_Accepted verifies a token whose azp matches a
// configured AuthorizedParty is accepted.
func TestValidator_AzpMatch_Accepted(t *testing.T) {
	idp := newTestIDP(t)
	v := mustValidator(t, OIDCConfig{Issuer: idp.srv.URL, Audience: "aikonos-broker", AuthorizedParty: "aikonos-webui"})

	tok := idp.mint(t, jwt.Claims{
		Issuer:   idp.srv.URL,
		Subject:  "alice@example.com",
		Audience: jwt.Audience{"aikonos-broker"},
		Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}, map[string]any{"azp": "aikonos-webui"})

	if _, err := v.Validate(context.Background(), tok); err != nil {
		t.Fatalf("expected matching azp to be accepted: %v", err)
	}
}
