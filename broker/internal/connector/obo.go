// obo.go — the on-behalf-of (OBO) Entra token exchange primitive backing
// tenant-wide OneDrive access. CP1 shipped
// the exchange + AADSTS classification; CP2 adds OBOBroker, the caching layer
// that writes the exchanged token to the existing per-user connector Vault
// path and refreshes it on expiry — the same write-back semantics as
// Registry.FreshAccessToken (token.go:18-65).
package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/secrets"
)

// microsoftLoginBase is the Entra token-endpoint base, overridable in tests
// (points at an httptest fake login.microsoftonline.com).
var microsoftLoginBase = "https://login.microsoftonline.com"

// M365App is the tenant-wide Entra app registration used for the OBO
// exchange — the same login app registration extended with Graph delegated
// Files.ReadWrite + offline_access.
type M365App struct {
	TenantID     string
	ClientID     string
	ClientSecret string
}

// TenantAppSource resolves the configured M365App for a tenant. ok=false
// means the tenant has no M365 connection configured.
type TenantAppSource interface {
	M365App(ctx context.Context, tenant string) (app M365App, ok bool, err error)
}

// oboScope is the only Graph scope ever requested — delegated file access
// plus offline_access so the exchange also yields a refresh token.
const oboScope = "https://graph.microsoft.com/Files.ReadWrite offline_access"

// The fixed OAuth2 JWT-bearer on-behalf-of flow parameters (RFC 8693 /
// Microsoft identity platform OBO flow).
const (
	oboGrantType = "urn:ietf:params:oauth:grant-type:jwt-bearer"
	oboTokenUse  = "on_behalf_of"
)

// aadstsDetailCap bounds how much of a raw Entra error_description is echoed
// back to an admin — long enough to be actionable, short enough to never leak
// an oversized upstream payload into a gRPC status message.
const aadstsDetailCap = 300

// OBOTokenResult is the token-endpoint response the exchange needs.
type OBOTokenResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int
}

// ExchangeOBO performs the on-behalf-of token exchange: the caller's own
// Entra bearer (assertion) is exchanged for a token scoped to Graph delegated
// Files.ReadWrite + offline_access, acting as the tenant's M365 app. On
// failure, returns a classified, actionable error (see classifyAADSTSError)
// rather than the raw Entra error body.
func ExchangeOBO(ctx context.Context, httpc *http.Client, app M365App, assertion string) (*OBOTokenResult, error) {
	if httpc == nil {
		httpc = http.DefaultClient
	}
	form := url.Values{
		"grant_type":          {oboGrantType},
		"assertion":           {assertion},
		"requested_token_use": {oboTokenUse},
		"client_id":           {app.ClientID},
		"client_secret":       {app.ClientSecret},
		"scope":               {oboScope},
	}
	endpoint := fmt.Sprintf("%s/%s/oauth2/v2.0/token", strings.TrimRight(microsoftLoginBase, "/"), app.TenantID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("connector: build obo request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connector: obo exchange request failed: %w", err)
	}
	defer resp.Body.Close()

	var body struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int    `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("connector: decode obo response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, classifyAADSTSError(body.ErrorDescription)
	}
	if body.AccessToken == "" {
		return nil, fmt.Errorf("connector: obo exchange returned no access token")
	}
	return &OBOTokenResult{
		AccessToken:  body.AccessToken,
		RefreshToken: body.RefreshToken,
		ExpiresIn:    body.ExpiresIn,
	}, nil
}

// classifyAADSTSError maps a raw Entra error_description into an actionable,
// bounded detail string. The three codes the admin can actually act on get a
// specific, plain-language message; anything else passes the (bounded) raw
// description through so an unanticipated failure is still visible rather
// than swallowed.
func classifyAADSTSError(desc string) error {
	switch {
	case desc == "":
		return fmt.Errorf("obo exchange failed with no error description from Entra")
	case strings.Contains(desc, "AADSTS65001"):
		return fmt.Errorf("admin consent missing for the Graph Files.ReadWrite scope — grant admin consent for the app registration: %s", boundedDetail(desc))
	case strings.Contains(desc, "AADSTS7000215"):
		return fmt.Errorf("invalid client secret — the configured client secret was rejected: %s", boundedDetail(desc))
	case strings.Contains(desc, "AADSTS500011"):
		return fmt.Errorf("audience/app mismatch — the Microsoft Graph resource principal was not found for this app registration: %s", boundedDetail(desc))
	default:
		return fmt.Errorf("obo exchange failed: %s", boundedDetail(desc))
	}
}

func boundedDetail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > aadstsDetailCap {
		return s[:aadstsDetailCap]
	}
	return s
}

// ── OBOBroker: bootstrap + refresh caching layer (CP2) ──────────────────────

// oboRefreshSkew is how far ahead of expiry FreshAccessToken proactively
// refreshes. The OBO refresh is a slower, less frequent operation than the
// per-user oauth2.TokenSource path (token.go), so a wide skew keeps a call
// from racing expiry mid-request.
const oboRefreshSkew = 5 * time.Minute

// OBOScopes is the display-facing scope list stored alongside the OBO token
// — the short Graph permission names, matching what the legacy per-user
// OneDrive connector already stores (oneDrivePlugin.DefaultScopes in
// plugin.go), not the full scope string sent on the wire (oboScope). Exported
// so broker/internal/broker's m365_obo.go (oboConnectorScopes) doesn't carry
// its own duplicate literal — the connectorstore entry must look identical
// regardless of which path connected it (F-2).
var OBOScopes = []string{"Files.ReadWrite", "offline_access"}

// ErrOBOReconnectRequired signals a dead OBO refresh token (Entra
// invalid_grant on the refresh_token grant) — distinguishable via errors.Is so
// a caller can map it to reconnect_needed status. Self-heals on the next
// Bootstrap call carrying a fresh Entra login bearer.
var ErrOBOReconnectRequired = errors.New("connector: obo refresh token invalid — reconnect required")

// OBOBroker bootstraps and refreshes the tenant-wide OneDrive OBO connection
// for a user, caching the result at the existing per-user connector Vault path
// (secrets.ConnectorPath(tenant, user, "onedrive")) so every downstream token
// consumer is unchanged.
type OBOBroker struct {
	// Apps resolves the tenant's configured M365 app registration.
	Apps TenantAppSource
	// Secrets stores the exchanged/refreshed token pair. Required.
	Secrets secrets.Provider
	// Status reflects bootstrap/refresh outcomes into the connectorstore entry
	// (StatusConnected / StatusReconnectNeeded). Optional — nil disables status
	// tracking without changing the exchange/refresh flow itself.
	Status StatusUpdater
	// HTTP is the client used for the Entra token endpoint. Nil uses
	// http.DefaultClient.
	HTTP *http.Client
	// Logger is optional; used to report a failed token write-back (which can
	// silently brick a connection on refresh-token rotation) without failing
	// the call that triggered it.
	Logger *zap.Logger
}

// Configured reports whether the tenant has a usable M365 connection — the
// gate ensureOneDriveOBO and the connector RPCs use to decide whether
// OneDrive is tenant-managed at all.
func (b *OBOBroker) Configured(ctx context.Context, tenant string) bool {
	if b.Apps == nil {
		return false
	}
	_, ok, err := b.Apps.M365App(ctx, tenant)
	return err == nil && ok
}

// Bootstrap exchanges the caller's own Entra bearer (assertion) for a Graph
// OBO token and writes it to Vault at the existing connector path, reflecting
// the outcome via Status. A failure still records reconnect_needed so a
// stale "connected" status never survives a broken exchange.
func (b *OBOBroker) Bootstrap(ctx context.Context, tenant, user, assertion string) error {
	connectorID := ProviderOneDrive.ConnectorID()
	if b.Apps == nil {
		return fmt.Errorf("connector: obo bootstrap: no tenant app source configured")
	}
	app, ok, err := b.Apps.M365App(ctx, tenant)
	if err != nil {
		return fmt.Errorf("connector: obo bootstrap: resolve app: %w", err)
	}
	if !ok {
		return fmt.Errorf("connector: obo bootstrap: tenant %q has no M365 connection configured", tenant)
	}
	result, err := ExchangeOBO(ctx, b.httpClient(), app, assertion)
	if err != nil {
		b.recordStatus(tenant, user, connectorID, StatusReconnectNeeded)
		return err
	}
	if b.Secrets == nil {
		return fmt.Errorf("connector: obo bootstrap: no secrets provider configured")
	}
	if werr := b.Secrets.WriteConnector(ctx, tenant, user, connectorID, &secrets.ConnectorToken{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(result.ExpiresIn) * time.Second),
		Scopes:       OBOScopes,
	}); werr != nil {
		b.recordStatus(tenant, user, connectorID, StatusReconnectNeeded)
		return fmt.Errorf("connector: obo bootstrap: write token: %w", werr)
	}
	b.recordStatus(tenant, user, connectorID, StatusConnected)
	return nil
}

// FreshAccessToken returns a usable OneDrive Graph access token, refreshing it
// against Entra when expired/near-expiry and persisting the rotated refresh
// token back to Vault (Entra rotates refresh tokens on use — the same
// write-back rule as Registry.FreshAccessToken, token.go:41-63). A dead
// refresh token surfaces as ErrOBOReconnectRequired.
func (b *OBOBroker) FreshAccessToken(ctx context.Context, tenant, user string) (string, error) {
	if b.Secrets == nil {
		return "", fmt.Errorf("connector: obo: no secrets provider configured")
	}
	connectorID := ProviderOneDrive.ConnectorID()
	stored, err := b.Secrets.ReadConnector(ctx, tenant, user, connectorID)
	if err != nil {
		if errors.Is(err, secrets.ErrNotFound) {
			return "", fmt.Errorf("connector: obo: onedrive not bootstrapped for tenant %q user %q", tenant, user)
		}
		return "", err
	}
	if time.Until(stored.ExpiresAt) > oboRefreshSkew {
		return stored.AccessToken, nil
	}
	if b.Apps == nil {
		return "", fmt.Errorf("connector: obo: no tenant app source configured")
	}
	app, ok, aerr := b.Apps.M365App(ctx, tenant)
	if aerr != nil {
		return "", fmt.Errorf("connector: obo: resolve app: %w", aerr)
	}
	if !ok {
		return "", fmt.Errorf("connector: obo: tenant %q has no M365 connection configured", tenant)
	}
	result, rerr := refreshOBOToken(ctx, b.httpClient(), app, stored.RefreshToken)
	if rerr != nil {
		b.recordStatus(tenant, user, connectorID, StatusReconnectNeeded)
		return "", rerr
	}
	refresh := result.RefreshToken
	if refresh == "" {
		refresh = stored.RefreshToken
	}
	// The just-refreshed access token already serves this call, so a failed
	// write-back does not fail the request — but the next JIT fetch would
	// reload the stored (possibly already-rotated-out) token, bricking the
	// connection until the next successful refresh. Surface it instead of
	// dropping it silently (mirrors token.go:47-62).
	if werr := b.Secrets.WriteConnector(ctx, tenant, user, connectorID, &secrets.ConnectorToken{
		AccessToken:  result.AccessToken,
		RefreshToken: refresh,
		ExpiresAt:    time.Now().Add(time.Duration(result.ExpiresIn) * time.Second),
		Scopes:       stored.Scopes,
	}); werr != nil && b.Logger != nil {
		b.Logger.Warn("obo token write-back failed; connection may need re-link",
			zap.String("connector_id", connectorID),
			zap.Bool("refresh_token_rotated", refresh != stored.RefreshToken),
			zap.Error(werr))
	}
	b.recordStatus(tenant, user, connectorID, StatusConnected)
	return result.AccessToken, nil
}

// recordStatus is best-effort: a store failure is logged and never surfaces to
// the caller of Bootstrap/FreshAccessToken or alters its result.
func (b *OBOBroker) recordStatus(tenant, user, connectorID, status string) {
	if b.Status == nil {
		return
	}
	if err := b.Status.UpdateStatus(tenant, user, connectorID, status); err != nil && b.Logger != nil {
		b.Logger.Warn("obo connector status update failed",
			zap.String("connector_id", connectorID),
			zap.String("status", status),
			zap.Error(err))
	}
}

func (b *OBOBroker) httpClient() *http.Client {
	if b.HTTP != nil {
		return b.HTTP
	}
	return http.DefaultClient
}

// refreshOBOToken exchanges a stored refresh token for a new access/refresh
// token pair via the standard OAuth2 refresh_token grant against the same
// Entra token endpoint the OBO exchange uses. A distinguishable
// ErrOBOReconnectRequired signals invalid_grant (a dead refresh token).
func refreshOBOToken(ctx context.Context, httpc *http.Client, app M365App, refreshToken string) (*OBOTokenResult, error) {
	if httpc == nil {
		httpc = http.DefaultClient
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {app.ClientID},
		"client_secret": {app.ClientSecret},
		"scope":         {oboScope},
	}
	endpoint := fmt.Sprintf("%s/%s/oauth2/v2.0/token", strings.TrimRight(microsoftLoginBase, "/"), app.TenantID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("connector: build obo refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connector: obo refresh request failed: %w", err)
	}
	defer resp.Body.Close()

	var body struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int    `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("connector: decode obo refresh response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if body.Error == "invalid_grant" {
			return nil, fmt.Errorf("%w: %s", ErrOBOReconnectRequired, boundedDetail(body.ErrorDescription))
		}
		return nil, classifyAADSTSError(body.ErrorDescription)
	}
	if body.AccessToken == "" {
		return nil, fmt.Errorf("connector: obo refresh returned no access token")
	}
	return &OBOTokenResult{
		AccessToken:  body.AccessToken,
		RefreshToken: body.RefreshToken,
		ExpiresIn:    body.ExpiresIn,
	}, nil
}
