package broker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/auth"
	"github.com/adamdekan/aikonos/broker/internal/connector"
	"github.com/adamdekan/aikonos/broker/internal/connector/mcpfake"
	"github.com/adamdekan/aikonos/broker/internal/connectorstore"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/secrets"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// mapSecrets keeps per-user connector tokens in memory.
type mapSecrets struct {
	stubSecrets
	mu sync.Mutex
	m  map[string]*secrets.ConnectorToken
}

func (s *mapSecrets) ReadConnector(_ context.Context, t, u, c string) (*secrets.ConnectorToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tok, ok := s.m[t+"/"+u+"/"+c]; ok {
		return tok, nil
	}
	return nil, secrets.ErrNotFound
}
func (s *mapSecrets) WriteConnector(_ context.Context, t, u, c string, tok *secrets.ConnectorToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[t+"/"+u+"/"+c] = tok
	return nil
}
func (s *mapSecrets) DeleteConnector(_ context.Context, t, u, c string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, t+"/"+u+"/"+c)
	return nil
}

type mcpOAuthFixture struct {
	fga   *fakeFGAForMcp
	fake  *mcpfake.Server
	repo  *fakeMcpRepo
	vault *fakeMcpSecrets
	deps  Deps
	svc   *BrokerService
}

func newMcpOAuthFixture(t *testing.T) *mcpOAuthFixture {
	t.Helper()
	fga := &fakeFGAForMcp{allowed: map[string]bool{"user:admin@example.com||admin||tenant:aikonos-dev": true}}
	fgaSrv := fga.server(t)
	t.Cleanup(fgaSrv.Close)
	fake := mcpfake.New()
	t.Cleanup(fake.Close)
	f := &mcpOAuthFixture{fga: fga, fake: fake, repo: &fakeMcpRepo{}, vault: &fakeMcpSecrets{}}
	f.deps = testMcpDeps(t, fgaSrv.URL, f.repo, f.vault)
	f.deps.Mcp.AllowPrivate = true
	f.deps.Secrets = &mapSecrets{m: map[string]*secrets.ConnectorToken{}}
	f.deps.OAuth = &connector.Registry{RedirectURI: "http://localhost:3000/connect/callback"}
	f.deps.ConnAuth = connector.NewStateStore(connectorStateTTL)
	f.deps.Connectors = connectorstore.New(t.TempDir())
	f.svc = NewBrokerService(f.deps)
	return f
}

func (f *mcpOAuthFixture) add(t *testing.T, conn *brokerv1.McpConnection, secret string) (*brokerv1.McpConnection, error) {
	t.Helper()
	resp, err := f.svc.AddMcpConnection(ctxWithIdentity(testTenantUUID, "admin@example.com"), &brokerv1.AddMcpConnectionRequest{
		UserId: "admin@example.com", Connection: conn, OauthClientSecret: secret,
	})
	if err != nil {
		return nil, err
	}
	return resp.Connection, nil
}

func oauthConn(url string) *brokerv1.McpConnection {
	return &brokerv1.McpConnection{Name: "Fake MCP", Url: url, Transport: "streamable_http", AuthType: "oauth"}
}

// connect runs a user's whole browser round-trip and returns the connector id.
func (f *mcpOAuthFixture) connect(t *testing.T, connID, user string) string {
	t.Helper()
	ctx := ctxWithIdentity(testTenantUUID, user)
	begin, err := f.svc.BeginConnectorAuth(ctx, &brokerv1.BeginConnectorAuthRequest{UserId: user, McpConnectionId: connID})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	code, state, err := f.fake.Authorize(begin.AuthorizeUrl, user)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	done, err := f.svc.CompleteConnectorAuth(ctx, &brokerv1.CompleteConnectorAuthRequest{UserId: user, Code: code, State: state})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	return done.ConnectorId
}

// AG-09 end to end: admin adds an oauth connection (discovery + dynamic
// registration), a user connects, the server sees that user, and the user
// can disconnect.
func TestMcpOAuth_AddConnectListDisconnect(t *testing.T) {
	f := newMcpOAuthFixture(t)
	conn, err := f.add(t, oauthConn(f.fake.MCPURL()), "")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if conn.OauthClientId != "dcr-client" || conn.OauthIssuer != f.fake.URL || len(conn.OauthScopes) != 1 {
		t.Errorf("oauth fields = %q %q %v", conn.OauthClientId, conn.OauthIssuer, conn.OauthScopes)
	}
	secretPath, _ := secrets.McpBearerPath(testTenantUUID, connector.McpClientSecretID(conn.Id))
	if f.vault.written[secretPath] != "dcr-secret" || conn.AuthSecretRef != secretPath {
		t.Errorf("client secret not in its own Vault slot: ref=%q written=%v", conn.AuthSecretRef, f.vault.written)
	}

	alice := ctxWithIdentity(testTenantUUID, "alice@example.com")
	// F140: without can_access on the connection it is neither offered nor
	// can a link be started.
	provs, err := f.svc.ListConnectorProviders(alice, &brokerv1.ListConnectorProvidersRequest{})
	if err != nil || len(provs.Providers) != 0 {
		t.Fatalf("offered without can_access: %v %v", provs, err)
	}
	if _, err := f.svc.BeginConnectorAuth(alice, &brokerv1.BeginConnectorAuthRequest{UserId: "alice@example.com", McpConnectionId: conn.Id}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("begin without can_access: want PermissionDenied, got %v", err)
	}
	f.fga.allowed["user:alice@example.com||can_access||mcp_connector:"+conn.Id] = true
	provs, err = f.svc.ListConnectorProviders(alice, &brokerv1.ListConnectorProvidersRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var offered bool
	for _, p := range provs.Providers {
		offered = offered || (p.Key == "mcp:"+conn.Id && p.DisplayName == "Fake MCP")
	}
	if !offered {
		t.Fatalf("oauth MCP connection not offered: %v", provs.Providers)
	}

	connectorID := f.connect(t, conn.Id, "alice@example.com")
	if connectorID != connector.McpConnectorID(conn.Id) {
		t.Errorf("connector id = %q", connectorID)
	}
	list, err := f.svc.ListConnectors(alice, &brokerv1.ListConnectorsRequest{UserId: "alice@example.com"})
	if err != nil || len(list.Connectors) != 1 || list.Connectors[0].Status != "connected" || list.Connectors[0].DisplayName != "Fake MCP" {
		t.Fatalf("list = %v err=%v", list, err)
	}

	// Tool discovery for a session uses the session user's own token.
	// AG-08: the session's agent allowlists whoami; audit mode skips the
	// AG-02 user-bound filter so the oauth path alone decides.
	south := NewSandboxService(func() Deps {
		d := f.deps
		d.DevMode = true
		d.Config = userBoundCfg("audit")
		d.Agents = &fakeAgentStoreForSkill{agent: &db.Agent{
			ID: uuid.MustParse(mcpTestAgent), McpServers: []string{conn.Id}, McpTools: []string{"mcp:" + conn.Id + ":whoami"},
		}}
		return d
	}())
	peer := auth.WithIdentity(context.Background(), &auth.Identity{SpiffeID: "spiffe://test/gateway"})
	tools, err := south.ListMcpServerToolsSouth(peer, &brokerv1.ListMcpServerToolsRequest{
		TenantId: testTenantUUID, ConnectorId: conn.Id, AgentId: mcpTestAgent, UserId: "alice@example.com"})
	if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != "whoami" {
		t.Fatalf("south tools for alice = %v err=%v", tools, err)
	}
	if _, err := south.ListMcpServerToolsSouth(peer, &brokerv1.ListMcpServerToolsRequest{
		TenantId: testTenantUUID, ConnectorId: conn.Id, AgentId: mcpTestAgent, UserId: "bob@example.com"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("south tools for unlinked bob: want FailedPrecondition, got %v", err)
	}
	// The admin has not linked an account either.
	if _, err := f.svc.ListMcpServerTools(ctxWithIdentity(testTenantUUID, "admin@example.com"), &brokerv1.ListMcpServerToolsRequest{
		UserId: "admin@example.com", ConnectorId: conn.Id}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("admin tools without a linked account: want FailedPrecondition, got %v", err)
	}

	if _, err := f.svc.RevokeConnector(alice, &brokerv1.RevokeConnectorRequest{UserId: "alice@example.com", ConnectorId: connectorID}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	list, _ = f.svc.ListConnectors(alice, &brokerv1.ListConnectorsRequest{UserId: "alice@example.com"})
	if len(list.Connectors) != 0 {
		t.Errorf("after disconnect: %v", list.Connectors)
	}
	if _, err := south.ListMcpServerToolsSouth(peer, &brokerv1.ListMcpServerToolsRequest{
		TenantId: testTenantUUID, ConnectorId: conn.Id, AgentId: mcpTestAgent, UserId: "alice@example.com"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("after disconnect: want FailedPrecondition, got %v", err)
	}
}

// A configured client id skips registration; saving again with the same id
// keeps the stored secret and does not re-register.
func TestMcpOAuth_PreRegisteredClientKeptOnUpdate(t *testing.T) {
	f := newMcpOAuthFixture(t)
	in := oauthConn(f.fake.MCPURL())
	in.OauthClientId = "pre-registered"
	in.OauthScopes = []string{"custom.scope"}
	conn, err := f.add(t, in, "pre-secret")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if f.fake.Registered() != 0 || conn.OauthClientId != "pre-registered" || conn.OauthScopes[0] != "custom.scope" {
		t.Fatalf("pre-registered client: registered=%d conn=%v", f.fake.Registered(), conn)
	}
	in.Id = conn.Id
	upd, err := f.svc.UpdateMcpConnection(ctxWithIdentity(testTenantUUID, "admin@example.com"), &brokerv1.UpdateMcpConnectionRequest{
		UserId: "admin@example.com", Connection: in})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	secretPath, _ := secrets.McpBearerPath(testTenantUUID, connector.McpClientSecretID(conn.Id))
	if upd.Connection.AuthSecretRef != secretPath || f.vault.written[secretPath] != "pre-secret" || f.vault.deleted[secretPath] {
		t.Errorf("secret not kept: ref=%q", upd.Connection.AuthSecretRef)
	}
}

// Discovery that finds no authorization server refuses the save.
func TestMcpOAuth_AddFailsClosedWithoutMetadata(t *testing.T) {
	f := newMcpOAuthFixture(t)
	bare := httptest.NewServer(http.NotFoundHandler())
	defer bare.Close()
	if _, err := f.add(t, oauthConn(bare.URL+"/mcp"), ""); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v", err)
	}
	if len(f.repo.conns) != 0 {
		t.Errorf("connection saved despite failed discovery")
	}
}
