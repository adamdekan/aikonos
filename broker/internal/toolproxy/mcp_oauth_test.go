package toolproxy

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/connector"
	"github.com/adamdekan/aikonos/broker/internal/connector/mcpfake"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/secrets"
)

// userTokens is a per-user connector token store (the rest of secrets.Provider
// is unused here).
type userTokens struct {
	secrets.Provider
	m map[string]*secrets.ConnectorToken
}

func (u *userTokens) ReadConnector(_ context.Context, t, usr, c string) (*secrets.ConnectorToken, error) {
	if tok, ok := u.m[t+"/"+usr+"/"+c]; ok {
		return tok, nil
	}
	return nil, secrets.ErrNotFound
}

func (u *userTokens) WriteConnector(_ context.Context, t, usr, c string, tok *secrets.ConnectorToken) error {
	u.m[t+"/"+usr+"/"+c] = tok
	return nil
}

// AG-09: an oauth MCP call carries the task subject's own token, and a
// service run or an unlinked user is refused.
func TestInvokeMCP_OAuthUsesSubjectToken(t *testing.T) {
	fake := mcpfake.New()
	defer fake.Close()
	ctx := context.Background()
	tenant := uuid.New()
	connID := uuid.New()
	conn := &db.McpConnection{ID: connID, TenantID: tenant, Name: "Fake", URL: fake.MCPURL(), AuthType: "oauth",
		OAuthClientID: "c", OAuthAuthURL: fake.URL + "/authorize", OAuthTokenURL: fake.URL + "/token",
		OAuthResource: fake.MCPURL()}
	reg := &connector.Registry{RedirectURI: "https://app/connect/callback"}

	// alice links her account.
	m, err := connector.LoadMcpOAuth(ctx, conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	verifier := connector.GenerateVerifier()
	code, _, err := fake.Authorize(reg.McpAuthCodeURL(m, "s", verifier), "alice")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := reg.McpExchange(ctx, http.DefaultClient, m, code, verifier)
	if err != nil {
		t.Fatal(err)
	}
	store := &userTokens{m: map[string]*secrets.ConnectorToken{
		tenant.String() + "/alice/" + connector.McpConnectorID(connID.String()): {
			AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken, ExpiresAt: time.Now().Add(time.Hour)},
	}}

	proxy := New(Config{
		McpConnections:  &fakeConnRepo{conns: map[string]*db.McpConnection{connID.String(): conn}},
		Secrets:         store,
		OAuth:           reg,
		AllowMcpPrivate: true,
	}, zap.NewNop())
	call := func(user string, obo db.OnBehalfOf) *Result {
		res, err := proxy.Invoke(ctx, Request{ToolID: "mcp:" + connID.String() + ":whoami",
			TenantID: tenant.String(), UserID: user, OBO: obo})
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		return res
	}

	if res := call("alice", db.OnBehalfOf{Subject: "alice"}); !res.Success || res.Output["content"] != "user:alice" {
		t.Fatalf("alice: %+v", res)
	}
	if res := call("bob", db.OnBehalfOf{Subject: "bob"}); res.Success || !strings.Contains(res.Error, "not connected") {
		t.Errorf("bob (not linked): %+v", res)
	}
	if res := call("svc-agent", db.OnBehalfOf{ServiceRun: true}); res.Success || !strings.Contains(res.Error, "human subject") {
		t.Errorf("service run: %+v", res)
	}
}
