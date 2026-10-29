package connector

// AG-09: per-user OAuth to MCP servers, following the MCP authorization spec.
// Discovery (RFC 9728 protected-resource metadata, then RFC 8414 / OIDC
// authorization-server metadata) runs once when an admin saves the
// connection; the result is pinned on the connection row. A user's connect is
// authorization code + PKCE (S256) with the RFC 8707 resource indicator on
// every authorize and token request, and the token lands in Vault through the
// same per-user connector slot, state store and refresh write-back the cloud
// connectors use.
//
// Every HTTP call here goes through the caller's *http.Client, which the broker
// builds with the SSRF-guarded dialer: the metadata documents name further
// URLs (authorization servers, token endpoints) that an MCP server controls.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/oauth2"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/secrets"
)

// McpOAuth is one oauth MCP connection's client and pinned endpoints.
type McpOAuth struct {
	ConnectionID string
	Name         string
	ClientID     string
	ClientSecret string
	AuthURL      string
	TokenURL     string
	Resource     string
	Scopes       []string
}

const mcpProviderPrefix = "mcp:"

// McpProvider is the PendingAuth / connectorstore provider value of an MCP
// connection, so the shared OAuth callback can tell the flows apart.
func McpProvider(connID string) Provider { return Provider(mcpProviderPrefix + connID) }

// McpConnectionID reports the MCP connection a provider value names.
func McpConnectionID(p Provider) (string, bool) {
	return strings.CutPrefix(string(p), mcpProviderPrefix)
}

// McpConnectorID is the per-user Vault and connectorstore id of a user's token
// for an MCP connection.
func McpConnectorID(connID string) string { return "mcp-" + connID }

// McpClientSecretID is the McpBearerStore id holding an MCP connection's OAuth
// client secret. It is a sibling of the bearer slot, never the slot itself, so
// switching a connection to bearer cannot send the client secret to the MCP
// server as a bearer token.
func McpClientSecretID(connID string) string { return connID + "-oauth-client" }

// LoadMcpOAuth builds the client for an oauth connection row, reading the client
// secret from Vault (absent → a public client).
func LoadMcpOAuth(ctx context.Context, conn *db.McpConnection, store secrets.McpBearerStore) (McpOAuth, error) {
	if conn.AuthType != "oauth" {
		return McpOAuth{}, fmt.Errorf("mcp connection %q does not use oauth", conn.Name)
	}
	if conn.OAuthClientID == "" || conn.OAuthAuthURL == "" || conn.OAuthTokenURL == "" {
		return McpOAuth{}, fmt.Errorf("mcp connection %q has no oauth client; an admin must save it again", conn.Name)
	}
	m := McpOAuth{
		ConnectionID: conn.ID.String(),
		Name:         conn.Name,
		ClientID:     conn.OAuthClientID,
		AuthURL:      conn.OAuthAuthURL,
		TokenURL:     conn.OAuthTokenURL,
		Resource:     conn.OAuthResource,
		Scopes:       conn.OAuthScopes,
	}
	if store != nil {
		secret, err := store.ReadMcpBearer(ctx, conn.TenantID.String(), McpClientSecretID(m.ConnectionID))
		if err != nil && !errors.Is(err, secrets.ErrMcpBearerNotFound) {
			return McpOAuth{}, fmt.Errorf("read mcp oauth client secret: %w", err)
		}
		m.ClientSecret = secret
	}
	return m, nil
}

func (r *Registry) mcpConfig(m McpOAuth) *oauth2.Config {
	cfg := &oauth2.Config{
		ClientID:     m.ClientID,
		ClientSecret: m.ClientSecret,
		RedirectURL:  r.RedirectURI,
		Scopes:       m.Scopes,
		Endpoint:     oauth2.Endpoint{AuthURL: m.AuthURL, TokenURL: m.TokenURL},
	}
	if m.ClientSecret == "" {
		// A public client (CIMD, or a pre-registered id without a secret)
		// identifies itself in the form body.
		cfg.Endpoint.AuthStyle = oauth2.AuthStyleInParams
	}
	return cfg
}

// McpAuthCodeURL builds the consent URL: PKCE S256 plus the resource indicator.
func (r *Registry) McpAuthCodeURL(m McpOAuth, state, verifier string) string {
	return r.mcpConfig(m).AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("resource", m.Resource))
}

// McpExchange swaps the code for tokens at the connection's token endpoint.
func (r *Registry) McpExchange(ctx context.Context, hc *http.Client, m McpOAuth, code, verifier string) (*oauth2.Token, error) {
	return r.mcpConfig(m).Exchange(mcpTokenContext(ctx, hc, m.Resource), code, oauth2.VerifierOption(verifier))
}

// FreshMcpAccessToken returns the user's access token for an MCP connection,
// refreshing (with the resource indicator) and writing the rotated token back
// when it has expired. Not linked → "not connected"; a failed refresh marks
// the connection reconnect_needed.
func (r *Registry) FreshMcpAccessToken(ctx context.Context, hc *http.Client, store secrets.Provider, m McpOAuth, tenant, user string) (string, error) {
	connectorID := McpConnectorID(m.ConnectionID)
	defer LockToken(tenant, user, connectorID)()
	stored, err := readStored(ctx, store, tenant, user, connectorID, m.Name)
	if err != nil {
		return "", err
	}
	return r.refresh(mcpTokenContext(ctx, hc, m.Resource), store, r.mcpConfig(m), stored,
		tenant, user, connectorID, m.Name, string(McpProvider(m.ConnectionID)))
}

// mcpTokenContext routes oauth2's token requests through hc and adds the
// resource indicator to each one: oauth2's refresh path has no hook for extra
// parameters, and RFC 8707 / the MCP spec want it on every token request.
func mcpTokenContext(ctx context.Context, hc *http.Client, resource string) context.Context {
	c := *hc
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.Transport = resourceTransport{base: base, resource: resource}
	return context.WithValue(ctx, oauth2.HTTPClient, &c)
}

type resourceTransport struct {
	base     http.RoundTripper
	resource string
}

func (t resourceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.resource == "" || req.Method != http.MethodPost || req.Body == nil ||
		!strings.HasPrefix(req.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		return t.base.RoundTrip(req)
	}
	b, err := io.ReadAll(req.Body)
	req.Body.Close()
	if err != nil {
		return nil, err
	}
	v, err := url.ParseQuery(string(b))
	if err != nil {
		return nil, err
	}
	if v.Get("resource") == "" {
		v.Set("resource", t.resource)
	}
	body := []byte(v.Encode())
	out := req.Clone(req.Context())
	out.Body = io.NopCloser(bytes.NewReader(body))
	out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	out.ContentLength = int64(len(body))
	return t.base.RoundTrip(out)
}

// McpAuthMeta is what discovery found for an MCP server.
type McpAuthMeta struct {
	Issuer          string
	AuthURL         string
	TokenURL        string
	RegistrationURL string
	Resource        string
	Scopes          []string
	// CIMD reports client_id_metadata_document_supported: the server accepts
	// an https URL of a client metadata document as client_id.
	CIMD bool
}

var resourceMetadataRe = regexp.MustCompile(`resource_metadata="([^"]+)"`)

// DiscoverMcpAuth finds the authorization server for an MCP server: the
// protected-resource metadata named by a 401's WWW-Authenticate header, else
// the RFC 9728 well-known URIs (path-inserted, then root), then that server's
// RFC 8414 / OIDC metadata. It refuses a server without PKCE S256, an issuer
// that does not match its metadata, or a resource that is not this server.
// requireHTTPS is false only where the SSRF guard is also off (tests).
func DiscoverMcpAuth(ctx context.Context, hc *http.Client, serverURL string, requireHTTPS bool) (*McpAuthMeta, error) {
	su, err := url.Parse(serverURL)
	if err != nil || su.Host == "" {
		return nil, fmt.Errorf("invalid server url %q", serverURL)
	}
	origin := su.Scheme + "://" + su.Host
	path := strings.TrimSuffix(su.Path, "/")

	var candidates []string
	if u := probeResourceMetadata(ctx, hc, serverURL); u != "" {
		candidates = append(candidates, u)
	}
	if path != "" {
		candidates = append(candidates, origin+"/.well-known/oauth-protected-resource"+path)
	}
	candidates = append(candidates, origin+"/.well-known/oauth-protected-resource")

	type resourceMeta struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
		ScopesSupported      []string `json:"scopes_supported"`
	}
	var prm resourceMeta
	found, reached := false, false
	for _, c := range candidates {
		prm = resourceMeta{}
		err := fetchJSON(ctx, hc, c, &prm)
		// A *url.Error is a transport failure (DNS, connect, TLS, timeout);
		// anything else means the server answered.
		var ue *url.Error
		if !errors.As(err, &ue) {
			reached = true
		}
		if err == nil && len(prm.AuthorizationServers) > 0 {
			found = true
			break
		}
	}
	if !found && !reached {
		return nil, fmt.Errorf("could not reach %s (check the URL and that the server is up)", su.Host)
	}
	if !found {
		return nil, errors.New("no protected-resource metadata (RFC 9728) with an authorization server")
	}

	resource := prm.Resource
	if resource == "" {
		resource = serverURL
	}
	if !resourceCovers(resource, su) {
		return nil, fmt.Errorf("protected-resource metadata names resource %q, not this server", resource)
	}

	issuer := strings.TrimSuffix(prm.AuthorizationServers[0], "/")
	iu, err := url.Parse(issuer)
	if err != nil || iu.Host == "" {
		return nil, fmt.Errorf("invalid authorization server %q", issuer)
	}
	iorigin := iu.Scheme + "://" + iu.Host
	ipath := strings.TrimSuffix(iu.Path, "/")
	asCandidates := []string{
		iorigin + "/.well-known/oauth-authorization-server" + ipath,
		iorigin + "/.well-known/openid-configuration" + ipath,
	}
	if ipath != "" {
		asCandidates = append(asCandidates, issuer+"/.well-known/openid-configuration")
	}
	type serverMeta struct {
		Issuer                  string   `json:"issuer"`
		AuthorizationEndpoint   string   `json:"authorization_endpoint"`
		TokenEndpoint           string   `json:"token_endpoint"`
		RegistrationEndpoint    string   `json:"registration_endpoint"`
		CodeChallengeMethods    []string `json:"code_challenge_methods_supported"`
		ScopesSupported         []string `json:"scopes_supported"`
		ClientIDMetadataDocSupp bool     `json:"client_id_metadata_document_supported"`
	}
	var as serverMeta
	found = false
	for _, c := range asCandidates {
		as = serverMeta{}
		if fetchJSON(ctx, hc, c, &as) == nil && as.AuthorizationEndpoint != "" && as.TokenEndpoint != "" {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("no authorization-server metadata for %q", issuer)
	}
	if strings.TrimSuffix(as.Issuer, "/") != issuer {
		return nil, fmt.Errorf("authorization-server metadata issuer %q does not match %q", as.Issuer, issuer)
	}
	if !slices.Contains(as.CodeChallengeMethods, "S256") {
		return nil, errors.New("authorization server does not advertise PKCE S256")
	}
	if requireHTTPS {
		for _, u := range []string{issuer, as.AuthorizationEndpoint, as.TokenEndpoint, as.RegistrationEndpoint} {
			if u != "" && !strings.HasPrefix(u, "https://") {
				return nil, fmt.Errorf("authorization server endpoint %q is not https", u)
			}
		}
	}
	scopes := prm.ScopesSupported
	if len(scopes) == 0 {
		scopes = as.ScopesSupported
	}
	return &McpAuthMeta{
		Issuer:          issuer,
		AuthURL:         as.AuthorizationEndpoint,
		TokenURL:        as.TokenEndpoint,
		RegistrationURL: as.RegistrationEndpoint,
		Resource:        resource,
		Scopes:          scopes,
		CIMD:            as.ClientIDMetadataDocSupp,
	}, nil
}

// resourceCovers reports whether the metadata's resource identifies the MCP
// server at su: same origin, and su's path at or under the resource's path.
func resourceCovers(resource string, su *url.URL) bool {
	ru, err := url.Parse(resource)
	if err != nil || ru.Scheme != su.Scheme || ru.Host != su.Host {
		return false
	}
	rp := strings.TrimSuffix(ru.Path, "/")
	sp := strings.TrimSuffix(su.Path, "/")
	return rp == "" || sp == rp || strings.HasPrefix(sp, rp+"/")
}

// probeResourceMetadata makes one unauthenticated request and returns the
// resource_metadata URL from a 401's WWW-Authenticate header, or "".
func probeResourceMetadata(ctx context.Context, hc *http.Client, serverURL string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serverURL, nil)
	if err != nil {
		return ""
	}
	resp, err := hc.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		return ""
	}
	if m := resourceMetadataRe.FindStringSubmatch(resp.Header.Get("WWW-Authenticate")); m != nil {
		return m[1]
	}
	return ""
}

func fetchJSON(ctx context.Context, hc *http.Client, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: status %d", u, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// RegisterMcpClient registers aikonOS with an authorization server by RFC 7591
// dynamic client registration and returns the issued client id and secret
// (empty secret → the server issued a public client).
func RegisterMcpClient(ctx context.Context, hc *http.Client, registrationURL, redirectURI string) (clientID, clientSecret string, err error) {
	body, _ := json.Marshal(map[string]any{
		"client_name":                "aikonOS",
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "client_secret_basic",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, registrationURL, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return "", "", fmt.Errorf("client registration status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	var out struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", "", fmt.Errorf("decode client registration: %w", err)
	}
	if out.ClientID == "" {
		return "", "", errors.New("client registration returned no client_id")
	}
	return out.ClientID, out.ClientSecret, nil
}
