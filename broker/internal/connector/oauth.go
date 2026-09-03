// Package connector builds the per-provider OAuth flows (Google Drive, OneDrive)
// for the user-connector feature and holds the short-lived PKCE/state store that
// bridges BeginConnectorAuth → browser consent → CompleteConnectorAuth.
//
// Provider authorize/token endpoints are hardcoded rather than pulled from
// golang.org/x/oauth2/google|microsoft to avoid those packages' transitive
// dependencies (GCE metadata client etc.) — the URLs are stable.
package connector

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"go.uber.org/zap"
	"golang.org/x/oauth2"
)

// Provider identifies a cloud-storage backend. The string value is what is
// persisted in mcp_connections.json and the Vault path segment.
type Provider string

const (
	ProviderGoogleDrive Provider = "google_drive"
	ProviderOneDrive    Provider = "onedrive"
)

// ConnectorID is the fixed per-provider connector id. v1 supports one
// connection per provider per user, so the id is deterministic.
func (p Provider) ConnectorID() string {
	if pl, ok := pluginFor(p); ok {
		return pl.ConnectorID()
	}
	return ""
}

// DisplayName is the human label shown in the connector UI.
func (p Provider) DisplayName() string {
	if pl, ok := pluginFor(p); ok {
		return pl.DisplayName()
	}
	return string(p)
}

var googleEndpoint = oauth2.Endpoint{
	AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
	TokenURL: "https://oauth2.googleapis.com/token",
}

// googleRevokeURL is hit best-effort on RevokeConnector (Microsoft has no
// equivalent simple per-token revoke).
const googleRevokeURL = "https://oauth2.googleapis.com/revoke"

func microsoftEndpoint(tenant string) oauth2.Endpoint {
	if tenant == "" {
		tenant = "common"
	}
	return oauth2.Endpoint{
		AuthURL:  fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/authorize", tenant),
		TokenURL: fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", tenant),
	}
}

// AppCredentials is one provider's registered OAuth app (platform secret).
type AppCredentials struct {
	ClientID     string
	ClientSecret string
}

// Registry holds the platform OAuth app credentials and redirect URI. It is the
// single object both the connector RPCs and the Tool Proxy (for token refresh)
// depend on.
type Registry struct {
	RedirectURI string
	// Credentials holds each provider's platform OAuth app credentials, keyed by
	// Provider. A provider with no entry (or a zero-value entry) is unconfigured —
	// credsFor and Configured treat "missing key" and "present zero value"
	// identically, so plugins never need a Registry code change to add credential
	// support.
	Credentials     map[Provider]AppCredentials
	MicrosoftTenant string
	// Logger is optional; when set, FreshAccessToken reports a failed token
	// write-back (which can silently brick a connection on refresh-token
	// rotation) rather than dropping it.
	Logger *zap.Logger
	// Status is optional; when set, FreshAccessToken reflects refresh outcomes
	// into the stored connector entry (reconnect_needed on failure, connected
	// on the next success) so ListConnectors stops echoing a stale "connected"
	// for a dead connection. Nil disables status tracking without changing the
	// refresh flow itself. Implemented by connectorstore.Store.
	Status StatusUpdater
}

// StatusUpdater is the narrow persistence seam FreshAccessToken uses to record
// connector health across refreshes.
type StatusUpdater interface {
	UpdateStatus(tenant, user, connectorID, status string) error
}

// Connector status values written by FreshAccessToken.
const (
	StatusConnected       = "connected"
	StatusReconnectNeeded = "reconnect_needed"
)

// recordStatus is best-effort: a store failure is logged and never surfaces to
// the caller of FreshAccessToken or alters its result.
func (r *Registry) recordStatus(tenant, user, connectorID, status string) {
	if r.Status == nil {
		return
	}
	if err := r.Status.UpdateStatus(tenant, user, connectorID, status); err != nil && r.Logger != nil {
		r.Logger.Warn("connector status update failed",
			zap.String("connector_id", connectorID),
			zap.String("status", status),
			zap.Error(err))
	}
}

// credsFor returns the AppCredentials for the given provider. Credential
// values are deployment state stored on Registry, not plugin data — a map
// lookup, so any registered plugin (built-in or third-party) is credentialed
// the same way. A missing key yields the zero value, exactly like an absent
// built-in field did before this became a map.
func (r *Registry) credsFor(p Provider) AppCredentials {
	return r.Credentials[p]
}

// Configured reports whether a provider's app credentials are present. An
// unconfigured provider's RPCs return FailedPrecondition and its tools are not
// registered.
func (r *Registry) Configured(p Provider) bool {
	c := r.credsFor(p)
	return c.ClientID != "" && c.ClientSecret != ""
}

// OAuthConfig builds the oauth2.Config for a provider. Empty scopes → provider
// defaults.
func (r *Registry) OAuthConfig(p Provider, scopes []string) (*oauth2.Config, error) {
	if !r.Configured(p) {
		return nil, fmt.Errorf("connector: provider %q not configured", p)
	}
	pl, ok := pluginFor(p)
	if !ok {
		return nil, fmt.Errorf("connector: no plugin for provider %q", p)
	}
	if len(scopes) == 0 {
		scopes = pl.DefaultScopes()
	}
	creds := r.credsFor(p)
	cfg := &oauth2.Config{
		ClientID:     creds.ClientID,
		ClientSecret: creds.ClientSecret,
		RedirectURL:  r.RedirectURI,
		Scopes:       scopes,
		Endpoint:     pl.Endpoint(EndpointOpts{MicrosoftTenant: r.MicrosoftTenant}),
	}
	return cfg, nil
}

// AuthCodeURL builds the provider consent URL for a state + PKCE verifier.
// Google needs access_type=offline + prompt=consent to reliably return a
// refresh token; Microsoft uses the offline_access scope instead.
func (r *Registry) AuthCodeURL(p Provider, scopes []string, state, verifier string) (string, error) {
	cfg, err := r.OAuthConfig(p, scopes)
	if err != nil {
		return "", err
	}
	opts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(verifier)}
	if pl, ok := pluginFor(p); ok {
		opts = append(opts, pl.AuthCodeOptions()...)
	}
	return cfg.AuthCodeURL(state, opts...), nil
}

// Exchange swaps an authorization code (with PKCE verifier) for tokens.
func (r *Registry) Exchange(ctx context.Context, p Provider, scopes []string, code, verifier string) (*oauth2.Token, error) {
	cfg, err := r.OAuthConfig(p, scopes)
	if err != nil {
		return nil, err
	}
	return cfg.Exchange(ctx, code, oauth2.VerifierOption(verifier))
}

// EffectiveScopes resolves the scopes that will be requested (defaults applied).
func EffectiveScopes(p Provider, scopes []string) []string {
	if len(scopes) == 0 {
		if pl, ok := pluginFor(p); ok {
			return pl.DefaultScopes()
		}
		return nil
	}
	return scopes
}

// GenerateState returns a cryptographically-random opaque state value.
func GenerateState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// GenerateVerifier returns a PKCE code verifier (delegates to oauth2 so the
// challenge derivation stays consistent).
func GenerateVerifier() string { return oauth2.GenerateVerifier() }

// Revoke best-effort revokes a token at the provider. Google has a revoke
// endpoint; Microsoft has no simple per-token revoke (relies on expiry), so
// this is a no-op there. Errors are advisory — the caller still deletes the
// Vault secret regardless.
func (r *Registry) Revoke(ctx context.Context, p Provider, token string) error {
	var revokeURL string
	if pl, ok := pluginFor(p); ok {
		revokeURL = pl.RevokeURL()
	}
	if revokeURL == "" || token == "" {
		return nil
	}
	form := url.Values{"token": {token}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, revokeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("connector: revoke status %d for provider %q", resp.StatusCode, p)
	}
	return nil
}
