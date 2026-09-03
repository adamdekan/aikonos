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

// TestValidator_DecoupledIssuerJWKS proves the split-horizon behaviour required
// for the compose deployment: the token's iss claim is the browser-facing URL
// (http://localhost:18080/realms/aikonos) while the JWKS are fetched from the
// container-facing URL (http://keycloak:8080/…/certs). The validator must accept
// such a token when Issuer=browser-URL and JWKSURL=container-URL.
//
// If the implementation ignores JWKSURL and derives the key source from the
// issuer discovery document instead, this test fails because the container-facing
// stub serves ONLY /jwks — no OpenID-Connect discovery doc — so discovery would
// 404 and the key lookup would fail.
func TestValidator_DecoupledIssuerJWKS(t *testing.T) {
	const kid = "split-horizon-key"

	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}

	pub := jose.JSONWebKey{Key: &privKey.PublicKey, KeyID: kid, Algorithm: "RS256", Use: "sig"}
	jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{pub}}

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: privKey},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid),
	)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}

	// Container-facing stub: serves ONLY /jwks — no discovery doc at /.well-known.
	// Any validator that falls back to discovery will get a 404 and fail.
	containerFacing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jwks" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	t.Cleanup(containerFacing.Close)

	// Browser-facing issuer: a different URL used only for iss-claim matching.
	// It serves nothing useful — any key fetch attempt here would return a 500,
	// proving that key resolution went through JWKSURL, not through discovery.
	browserFacing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "browser-facing server: must not be contacted for JWKS", http.StatusInternalServerError)
	}))
	t.Cleanup(browserFacing.Close)

	v := mustValidator(t, OIDCConfig{
		Issuer:   browserFacing.URL,             // iss-claim expectation = browser URL
		JWKSURL:  containerFacing.URL + "/jwks", // key source        = container URL
		Audience: "aikonos-broker",
	})

	raw, err := jwt.Signed(signer).Claims(jwt.Claims{
		Issuer:   browserFacing.URL, // token iss = browser-facing
		Subject:  "alice@example.com",
		Audience: jwt.Audience{"aikonos-broker"},
		Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
		IssuedAt: jwt.NewNumericDate(time.Now()),
	}).Claims(map[string]any{
		"email":     "alice@example.com",
		"tenant_id": "11111111-1111-1111-1111-111111111111",
	}).Serialize()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	id, err := v.Validate(context.Background(), raw)
	if err != nil {
		t.Fatalf("Validate with decoupled issuer/JWKS: %v", err)
	}
	if id.Subject != "alice@example.com" {
		t.Errorf("subject = %q, want alice@example.com", id.Subject)
	}
	if id.TenantID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("tenant = %q, want 11111111-1111-1111-1111-111111111111", id.TenantID)
	}
}
