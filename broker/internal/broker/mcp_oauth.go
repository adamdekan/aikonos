package broker

// AG-09: per-user OAuth to MCP servers. The admin side (configureMcpOAuth)
// discovers the server's authorization server and settles the client when a
// connection is saved; the user side rides the connector RPCs —
// BeginConnectorAuth with mcp_connection_id, the shared /connect/callback →
// CompleteConnectorAuth, ListConnectorProviders / ListConnectors /
// RevokeConnector — so the token lands in the same per-user Vault slot and
// connectorstore entry as a cloud connector.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/auth"
	"github.com/adamdekan/aikonos/broker/internal/connector"
	"github.com/adamdekan/aikonos/broker/internal/connectorstore"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/secrets"
	"github.com/adamdekan/aikonos/broker/internal/toolproxy"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

const mcpOAuthTimeout = 30 * time.Second

// mcpHTTPClient is the SSRF-guarded client for discovery, registration and
// token calls: the endpoints come from metadata the MCP server controls.
func (d *Deps) mcpHTTPClient() *http.Client {
	return toolproxy.NewHardenedClient(mcpOAuthTimeout, d.Mcp.AllowPrivate)
}

// configureMcpOAuth fills rec's oauth columns from discovery and settles the
// client: an admin-supplied client id (secret optional, write-only, kept when
// the id is unchanged), else the aikonOS client metadata document when the
// server supports it, else dynamic registration. Fails closed: a server we
// cannot discover or register with is not saved as oauth.
func (s *BrokerService) configureMcpOAuth(ctx context.Context, tenant string, rec *db.McpConnection, in *brokerv1.McpConnection, secret string, prev *db.McpConnection) error {
	if s.deps.OAuth == nil || s.deps.Secrets == nil || s.deps.Mcp.Bearer == nil {
		return status.Error(codes.FailedPrecondition, "oauth MCP connections need Vault and connectors configured")
	}
	if s.deps.OAuth.RedirectURI == "" {
		return status.Error(codes.FailedPrecondition, "oauth MCP connections need connectors.redirect_uri")
	}
	hc := s.deps.mcpHTTPClient()
	meta, err := connector.DiscoverMcpAuth(ctx, hc, rec.URL, !s.deps.Mcp.AllowPrivate)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "oauth discovery failed: %v", err)
	}
	rec.OAuthIssuer = meta.Issuer
	rec.OAuthAuthURL = meta.AuthURL
	rec.OAuthTokenURL = meta.TokenURL
	rec.OAuthResource = meta.Resource
	rec.OAuthScopes = nil
	for _, sc := range in.OauthScopes {
		if sc = strings.TrimSpace(sc); sc != "" {
			rec.OAuthScopes = append(rec.OAuthScopes, sc)
		}
	}
	if len(rec.OAuthScopes) == 0 {
		rec.OAuthScopes = meta.Scopes
	}

	connID := rec.ID.String()
	path, err := secrets.McpBearerPath(tenant, connector.McpClientSecretID(connID))
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid vault path: %v", err)
	}
	keepSecret := false
	rec.OAuthClientID = strings.TrimSpace(in.OauthClientId)
	switch {
	case rec.OAuthClientID != "":
		keepSecret = secret == "" && prev != nil && prev.AuthType == "oauth" && prev.OAuthClientID == rec.OAuthClientID
	case meta.CIMD && s.deps.Mcp.ClientMetadataURL != "":
		rec.OAuthClientID = s.deps.Mcp.ClientMetadataURL
		secret = ""
	case meta.RegistrationURL != "":
		id, sec, rerr := connector.RegisterMcpClient(ctx, hc, meta.RegistrationURL, s.deps.OAuth.RedirectURI)
		if rerr != nil {
			return status.Errorf(codes.FailedPrecondition, "oauth client registration failed: %v", rerr)
		}
		rec.OAuthClientID, secret = id, sec
	default:
		return status.Error(codes.FailedPrecondition,
			"the authorization server supports neither client metadata documents nor dynamic registration; enter a client id")
	}

	switch {
	case keepSecret:
		rec.AuthSecretRef = prev.AuthSecretRef
	case secret != "":
		if werr := s.deps.Mcp.Bearer.WriteMcpBearer(ctx, path, secret); werr != nil {
			s.deps.Logger.Error("mcp oauth: client secret vault write failed", zap.Error(werr))
			return status.Error(codes.Internal, "failed to store oauth client secret")
		}
		rec.AuthSecretRef = path
	default:
		// A public client: drop any secret a previous client left behind.
		s.deleteMcpClientSecret(ctx, tenant, connID)
		rec.AuthSecretRef = ""
	}
	return nil
}

// deleteMcpClientSecret best-effort removes an oauth connection's client secret.
func (s *BrokerService) deleteMcpClientSecret(ctx context.Context, tenant, connID string) {
	if s.deps.Mcp.Bearer == nil {
		return
	}
	path, err := secrets.McpBearerPath(tenant, connector.McpClientSecretID(connID))
	if err != nil {
		return
	}
	if derr := s.deps.Mcp.Bearer.DeleteMcpBearer(ctx, path); derr != nil {
		s.deps.Logger.Warn("mcp oauth: client secret vault delete failed (continuing)", zap.Error(derr))
	}
}

// mcpToken returns the Authorization bearer for a broker-side call to an MCP
// server: the shared token (bearer), the given user's own token (oauth), or
// none.
func (d *Deps) mcpToken(ctx context.Context, conn *db.McpConnection, tenant, user string) (string, error) {
	switch conn.AuthType {
	case "bearer":
		if d.Mcp.Bearer == nil {
			return "", nil
		}
		b, err := d.Mcp.Bearer.ReadMcpBearer(ctx, tenant, conn.ID.String())
		if err != nil && !errors.Is(err, secrets.ErrMcpBearerNotFound) {
			d.Logger.Error("mcp: read bearer failed", zap.Error(err))
			return "", status.Error(codes.Internal, "failed to read MCP bearer token")
		}
		return b, nil
	case "oauth":
		if d.OAuth == nil || d.Secrets == nil {
			return "", status.Error(codes.FailedPrecondition, "oauth MCP connections need Vault and connectors configured")
		}
		if user == "" {
			return "", status.Error(codes.FailedPrecondition, "oauth MCP connection needs a user")
		}
		m, err := connector.LoadMcpOAuth(ctx, conn, d.Mcp.Bearer)
		if err != nil {
			return "", status.Error(codes.FailedPrecondition, err.Error())
		}
		tok, err := d.OAuth.FreshMcpAccessToken(ctx, d.mcpHTTPClient(), d.Secrets, m, tenant, user)
		if err != nil {
			return "", status.Error(codes.FailedPrecondition, err.Error())
		}
		return tok, nil
	}
	return "", nil
}

func (s *BrokerService) loadMcpOAuth(ctx context.Context, tenant, connID string) (connector.McpOAuth, error) {
	if s.deps.Mcp.Connections == nil {
		return connector.McpOAuth{}, status.Error(codes.FailedPrecondition, "MCP connections not configured")
	}
	conn, err := s.deps.Mcp.Connections.Get(ctx, tenant, connID)
	if err != nil {
		return connector.McpOAuth{}, status.Errorf(codes.NotFound, "mcp connection %q not found", connID)
	}
	m, err := connector.LoadMcpOAuth(ctx, conn, s.deps.Mcp.Bearer)
	if err != nil {
		return connector.McpOAuth{}, status.Error(codes.FailedPrecondition, err.Error())
	}
	return m, nil
}

// beginMcpAuth starts a user's authorization code + PKCE flow for an oauth MCP
// connection. The pending state carries provider "mcp:<id>", which routes the
// shared callback to completeMcpAuth.
func (s *BrokerService) beginMcpAuth(ctx context.Context, traceID, tenant, user, connID string) (*brokerv1.BeginConnectorAuthResponse, error) {
	m, err := s.loadMcpOAuth(ctx, tenant, connID)
	if err != nil {
		return nil, err
	}
	// F140: only a user who may use the connection links an account to it
	// (the same can_access every tool call checks). An FGA error fails closed.
	if !s.mayUseMcpConnection(ctx, user, m.ConnectionID) {
		s.emitMcpAudit(ctx, traceID, tenant, user, "aikonos.broker.mcp.oauth.begin", m.ConnectionID, auditv1.PolicyDecision_DENY)
		return nil, status.Errorf(codes.PermissionDenied, "no access to mcp connection %q", connID)
	}
	state, err := connector.GenerateState()
	if err != nil {
		return nil, status.Error(codes.Internal, "state generation failed")
	}
	verifier := connector.GenerateVerifier()
	if err := s.deps.ConnAuth.Put(ctx, state, connector.PendingAuth{
		Provider: connector.McpProvider(m.ConnectionID),
		Tenant:   tenant,
		User:     user,
		Verifier: verifier,
		Scopes:   m.Scopes,
	}); err != nil {
		s.deps.Logger.Warn("mcp oauth begin: persist state failed", zap.Error(err))
		return nil, status.Error(codes.Internal, "could not persist auth state")
	}
	s.emitMcpAudit(ctx, traceID, tenant, user, "aikonos.broker.mcp.oauth.begin", m.ConnectionID, auditv1.PolicyDecision_ALLOW)
	return &brokerv1.BeginConnectorAuthResponse{AuthorizeUrl: s.deps.OAuth.McpAuthCodeURL(m, state, verifier), State: state}, nil
}

// completeMcpAuth exchanges the code (with PKCE verifier and resource) and
// stores the user's token for the connection.
func (s *BrokerService) completeMcpAuth(ctx context.Context, traceID, tenant, user, connID string, pending connector.PendingAuth, code string) (*brokerv1.CompleteConnectorAuthResponse, error) {
	m, err := s.loadMcpOAuth(ctx, tenant, connID)
	if err != nil {
		return nil, err
	}
	tok, err := s.deps.OAuth.McpExchange(ctx, s.deps.mcpHTTPClient(), m, code, pending.Verifier)
	if err != nil {
		s.deps.Logger.Warn("mcp oauth complete: token exchange failed", zap.Error(err))
		s.emitMcpAudit(ctx, traceID, tenant, user, "aikonos.broker.mcp.oauth.connected", connID, auditv1.PolicyDecision_DENY)
		return nil, status.Error(codes.PermissionDenied, "token exchange failed")
	}
	connectorID := connector.McpConnectorID(connID)
	if err := s.deps.Secrets.WriteConnector(ctx, tenant, user, connectorID, &secrets.ConnectorToken{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    tok.Expiry,
		Scopes:       pending.Scopes,
		TokenURL:     m.TokenURL,
	}); err != nil {
		s.deps.Logger.Error("mcp oauth complete: vault write failed", zap.Error(err))
		return nil, status.Error(codes.Internal, "failed to store connector token")
	}
	if err := s.deps.Connectors.Upsert(tenant, user, connectorstore.Entry{
		ConnectorID: connectorID,
		Provider:    string(pending.Provider),
		DisplayName: m.Name,
		Scopes:      pending.Scopes,
		VaultPath:   vaultPath(tenant, user, connectorID),
		Status:      connector.StatusConnected,
		ConnectedAt: time.Now().UTC(),
	}); err != nil {
		s.deps.Logger.Error("mcp oauth complete: reference write failed", zap.Error(err))
		return nil, status.Error(codes.Internal, "failed to record connector")
	}
	s.emitMcpAudit(ctx, traceID, tenant, user, "aikonos.broker.mcp.oauth.connected", connID, auditv1.PolicyDecision_ALLOW)
	return &brokerv1.CompleteConnectorAuthResponse{
		ConnectorId: connectorID,
		Status:      connector.StatusConnected,
		Scopes:      pending.Scopes,
	}, nil
}

// mayUseMcpConnection is the caller's can_access on the connection; an FGA
// error counts as no.
func (s *BrokerService) mayUseMcpConnection(ctx context.Context, user, connID string) bool {
	ok, err := s.deps.Policy.CheckFGA(ctx, "user:"+user, "can_access", "mcp_connector:"+connID)
	return err == nil && ok
}

// mcpConnectorProviders lists the oauth MCP connections the caller may use as
// connect targets (key "mcp:<id>"). Best-effort: a listing error hides them.
func (s *BrokerService) mcpConnectorProviders(ctx context.Context, tenant string) []*brokerv1.ConnectorProviderInfo {
	if tenant == "" || s.deps.Mcp.Connections == nil {
		return nil
	}
	rows, err := s.deps.Mcp.Connections.List(ctx, tenant)
	if err != nil {
		s.deps.Logger.Warn("ListConnectorProviders: list mcp connections failed", zap.Error(err))
		return nil
	}
	user := ""
	if id, ok := auth.IdentityFromContext(ctx); ok {
		user = id.PrincipalID()
	}
	var out []*brokerv1.ConnectorProviderInfo
	for _, r := range rows {
		id := r.ID.String()
		if r.AuthType != "oauth" || !s.mayUseMcpConnection(ctx, user, id) {
			continue
		}
		out = append(out, &brokerv1.ConnectorProviderInfo{
			Key:         string(connector.McpProvider(id)),
			DisplayName: r.Name,
			ConnectorId: connector.McpConnectorID(id),
		})
	}
	return out
}
