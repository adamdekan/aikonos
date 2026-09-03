// broker/internal/auth/oidc.go
// OIDC bearer-token validation for north-bound calls.
//
// The validator verifies a JWT's signature against the issuer's JWKS, checks
// issuer/audience/expiry, and projects the claims into an *Identity. Keys are
// fetched from the issuer's discovery document (or an explicit JWKS URL) and
// cached, with a forced refresh on an unknown key id (handles key rotation).
//
// Dev mode: when Issuer is empty the validator is disabled — Validate is never
// called and the OIDC interceptor passes calls through. This keeps the broker
// runnable without Keycloak (the established local-dev posture) while making
// real enforcement a config flip.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// OIDCConfig configures the bearer-token validator.
type OIDCConfig struct {
	Issuer   string // OIDC issuer URL; empty disables validation (dev mode)
	Audience string // required `aud` value (the broker's client id)
	JWKSURL  string // explicit JWKS URL; empty → derived via OIDC discovery
	// SubjectClaim is the claim used as the caller's stable principal id; default
	// "sub". Set "oid" for Entra (immutable + stable across app registrations
	// within a tenant; "sub" is pairwise per app and would churn FGA tuples).
	SubjectClaim string
	TenantClaim  string // claim carrying the tenant id; default "tenant_id"
	// AuthorizedParty, when set, requires the token's `azp` claim to equal it.
	// Empty means no azp requirement (unchanged behavior for single-client
	// deployments that don't set azp at all).
	AuthorizedParty string
	// MinRefresh bounds how often a missing-kid miss can trigger a JWKS refetch.
	MinRefresh time.Duration
}

// Validator verifies OIDC bearer tokens against a cached JWKS.
type Validator struct {
	cfg        OIDCConfig
	httpClient *http.Client

	mu        sync.RWMutex
	keys      *jose.JSONWebKeySet
	jwksURL   string
	lastFetch time.Time
}

var sigAlgs = []jose.SignatureAlgorithm{jose.RS256, jose.RS384, jose.RS512, jose.ES256}

// NewValidator builds a validator. When cfg.Issuer is empty the returned
// validator is disabled (Enabled() == false). When cfg.Issuer is set, a
// non-empty cfg.Audience is required — an enabled validator with no audience
// would only check issuer+expiry, silently accepting a token minted for any
// other client of the same issuer. That misconfiguration is refused here
// (startup error) rather than allowed to validate per-request.
func NewValidator(cfg OIDCConfig) (*Validator, error) {
	if cfg.SubjectClaim == "" {
		cfg.SubjectClaim = "sub"
	}
	if cfg.TenantClaim == "" {
		cfg.TenantClaim = "tenant_id"
	}
	if cfg.MinRefresh == 0 {
		cfg.MinRefresh = time.Minute
	}
	if cfg.Issuer != "" && cfg.Audience == "" {
		return nil, errors.New("oidc: Audience is required when Issuer is set (empty audience accepts tokens for any client of the issuer)")
	}
	return &Validator{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		jwksURL:    cfg.JWKSURL,
	}, nil
}

// Enabled reports whether token validation is active.
func (v *Validator) Enabled() bool { return v.cfg.Issuer != "" }

// standardClaims is the subset of registered + Keycloak claims we read.
type oidcClaims struct {
	Email             string `json:"email"`
	PreferredUsername string `json:"preferred_username"`
	Scope             string `json:"scope"`
	AuthorizedParty   string `json:"azp"`
}

// Validate verifies a raw bearer token and returns the caller identity.
func (v *Validator) Validate(ctx context.Context, raw string) (*Identity, error) {
	if !v.Enabled() {
		return nil, errors.New("oidc validator disabled")
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("empty bearer token")
	}

	tok, err := jwt.ParseSigned(raw, sigAlgs)
	if err != nil {
		return nil, fmt.Errorf("parse token: %w", err)
	}
	if len(tok.Headers) == 0 {
		return nil, errors.New("token has no header")
	}
	kid := tok.Headers[0].KeyID

	key, err := v.keyForID(ctx, kid)
	if err != nil {
		return nil, err
	}

	var std jwt.Claims
	var custom oidcClaims
	var allClaims map[string]json.RawMessage
	if err := tok.Claims(key, &std, &custom, &allClaims); err != nil {
		return nil, fmt.Errorf("verify signature: %w", err)
	}

	expected := jwt.Expected{Issuer: v.cfg.Issuer}
	if v.cfg.Audience != "" {
		expected.AnyAudience = jwt.Audience{v.cfg.Audience}
	}
	if err := std.Validate(expected); err != nil {
		return nil, fmt.Errorf("claims invalid: %w", err)
	}
	if v.cfg.AuthorizedParty != "" && custom.AuthorizedParty != v.cfg.AuthorizedParty {
		return nil, fmt.Errorf("claims invalid: azp mismatch")
	}

	id := &Identity{
		Subject: std.Subject,
		Email:   custom.Email,
		Scopes:  splitScopes(custom.Scope),
	}
	// Override the principal from a configured claim (e.g. Entra "oid"). Falls
	// back to the standard "sub" when the claim is "sub" or absent/non-string.
	if v.cfg.SubjectClaim != "sub" {
		if s := stringClaim(allClaims, v.cfg.SubjectClaim); s != "" {
			id.Subject = s
		}
	}
	if id.Email == "" {
		id.Email = custom.PreferredUsername
	}
	id.Name = stringClaim(allClaims, "name")
	if s := stringClaim(allClaims, v.cfg.TenantClaim); s != "" {
		id.TenantID = s
	}
	if strings.HasPrefix(id.Subject, "svc-") {
		return nil, &SvcSubjectError{Subject: id.Subject, Email: id.Email}
	}
	return id, nil
}

// SvcSubjectError is returned by Validate when the resolved principal starts
// with "svc-". The interceptor distinguishes it from generic token failures to
// emit the correct audit event and gRPC status code.
type SvcSubjectError struct {
	Subject string
	Email   string
}

func (e *SvcSubjectError) Error() string {
	return fmt.Sprintf("svc- subject not permitted: %s", e.Subject)
}

// stringClaim reads a string-valued claim from the raw claims map; returns ""
// when absent or not a JSON string.
func stringClaim(claims map[string]json.RawMessage, name string) string {
	raw, ok := claims[name]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// keyForID returns the verification key for kid, refreshing the JWKS once if
// the key is not currently cached (key rotation).
func (v *Validator) keyForID(ctx context.Context, kid string) (interface{}, error) {
	if k := v.lookup(kid); k != nil {
		return k.Key, nil
	}
	if err := v.refresh(ctx); err != nil {
		return nil, fmt.Errorf("fetch jwks: %w", err)
	}
	if k := v.lookup(kid); k != nil {
		return k.Key, nil
	}
	return nil, fmt.Errorf("no JWKS key for kid %q", kid)
}

func (v *Validator) lookup(kid string) *jose.JSONWebKey {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.keys == nil {
		return nil
	}
	// kid may be empty if the IdP omits it and publishes a single key.
	if kid == "" && len(v.keys.Keys) == 1 {
		return &v.keys.Keys[0]
	}
	matches := v.keys.Key(kid)
	if len(matches) == 0 {
		return nil
	}
	return &matches[0]
}

func (v *Validator) refresh(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.lastFetch.IsZero() && time.Since(v.lastFetch) < v.cfg.MinRefresh {
		return nil // rate-limit refetch storms on bad kids
	}

	if v.jwksURL == "" {
		u, err := v.discoverJWKS(ctx)
		if err != nil {
			return err
		}
		v.jwksURL = u
	}

	set, err := v.fetchJWKS(ctx, v.jwksURL)
	if err != nil {
		return err
	}
	v.keys = set
	v.lastFetch = time.Now()
	return nil
}

func (v *Validator) discoverJWKS(ctx context.Context) (string, error) {
	wellKnown := strings.TrimRight(v.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wellKnown, nil)
	if err != nil {
		return "", err
	}
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("discovery returned %d", resp.StatusCode)
	}
	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", err
	}
	if doc.JWKSURI == "" {
		return "", errors.New("discovery document has no jwks_uri")
	}
	return doc.JWKSURI, nil
}

func (v *Validator) fetchJWKS(ctx context.Context, url string) (*jose.JSONWebKeySet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks endpoint returned %d", resp.StatusCode)
	}
	var set jose.JSONWebKeySet
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return nil, err
	}
	return &set, nil
}

func splitScopes(scope string) []string {
	if scope == "" {
		return nil
	}
	return strings.Fields(scope)
}
