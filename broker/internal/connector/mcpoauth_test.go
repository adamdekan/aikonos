package connector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/connector/mcpfake"
	"github.com/adamdekan/aikonos/broker/internal/secrets"
)

func TestDiscoverMcpAuth_FollowsChallengeToServerMetadata(t *testing.T) {
	fake := mcpfake.New()
	defer fake.Close()

	meta, err := DiscoverMcpAuth(context.Background(), http.DefaultClient, fake.MCPURL(), false)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if meta.Issuer != fake.URL || meta.AuthURL != fake.URL+"/authorize" || meta.TokenURL != fake.URL+"/token" {
		t.Errorf("endpoints = %+v", meta)
	}
	if meta.Resource != fake.MCPURL() || meta.RegistrationURL != fake.URL+"/register" {
		t.Errorf("resource/registration = %q %q", meta.Resource, meta.RegistrationURL)
	}
	if len(meta.Scopes) != 1 || meta.Scopes[0] != "mcp.read" {
		t.Errorf("scopes = %v", meta.Scopes)
	}
}

func TestDiscoverMcpAuth_FailsClosed(t *testing.T) {
	fake := mcpfake.New()
	defer fake.Close()

	if _, err := DiscoverMcpAuth(context.Background(), http.DefaultClient, fake.MCPURL(), true); err == nil ||
		!strings.Contains(err.Error(), "not https") {
		t.Errorf("http endpoints with requireHTTPS: err = %v", err)
	}
	// The metadata names <srv>/mcp; a connection URL on another path is not it.
	if _, err := DiscoverMcpAuth(context.Background(), http.DefaultClient, fake.URL+"/other", false); err == nil {
		t.Error("expected discovery to refuse a URL the metadata does not cover")
	}
	fake.NoS256 = true
	if _, err := DiscoverMcpAuth(context.Background(), http.DefaultClient, fake.MCPURL(), false); err == nil ||
		!strings.Contains(err.Error(), "S256") {
		t.Errorf("no S256: err = %v", err)
	}
}

func TestRegisterMcpClient(t *testing.T) {
	fake := mcpfake.New()
	defer fake.Close()
	id, secret, err := RegisterMcpClient(context.Background(), http.DefaultClient, fake.URL+"/register", "https://app/connect/callback")
	if err != nil || id != "dcr-client" || secret != "dcr-secret" {
		t.Fatalf("register = %q %q %v", id, secret, err)
	}
}

// A user's flow end to end: authorize URL carries PKCE + resource, the code
// exchange and the later refresh both carry the resource, and the rotated
// token is written back to the user's own Vault slot.
func TestMcpOAuth_ExchangeAndRefreshCarryResource(t *testing.T) {
	fake := mcpfake.New()
	defer fake.Close()
	ctx := context.Background()
	reg := &Registry{RedirectURI: "https://app/connect/callback"}
	m := McpOAuth{ConnectionID: "c1", Name: "Fake", ClientID: "dcr-client", ClientSecret: "dcr-secret",
		AuthURL: fake.URL + "/authorize", TokenURL: fake.URL + "/token", Resource: fake.MCPURL(), Scopes: []string{"mcp.read"}}

	verifier := GenerateVerifier()
	code, state, err := fake.Authorize(reg.McpAuthCodeURL(m, "st", verifier), "alice")
	if err != nil || state != "st" {
		t.Fatalf("authorize: %v state=%q", err, state)
	}
	tok, err := reg.McpExchange(ctx, http.DefaultClient, m, code, verifier)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}

	store := &fakeSecrets{tokens: map[string]*secrets.ConnectorToken{}}
	cid := McpConnectorID("c1")
	_ = store.WriteConnector(ctx, "t1", "alice", cid, &secrets.ConnectorToken{
		AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken, ExpiresAt: time.Now().Add(-time.Minute)})

	got, err := reg.FreshMcpAccessToken(ctx, http.DefaultClient, store, m, "t1", "alice")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got == tok.AccessToken || store.tokens[key("t1", "alice", cid)].AccessToken != got {
		t.Errorf("refreshed token not written back: got %q", got)
	}
	forms := fake.TokenForms()
	if len(forms) != 2 || forms[1].Get("grant_type") != "refresh_token" {
		t.Fatalf("token requests = %v", forms)
	}
	for _, f := range forms {
		if f.Get("resource") != fake.MCPURL() {
			t.Errorf("%s request resource = %q", f.Get("grant_type"), f.Get("resource"))
		}
	}

	// Another user has no token: not connected, never alice's.
	if _, err := reg.FreshMcpAccessToken(ctx, http.DefaultClient, store, m, "t1", "bob"); err == nil ||
		!strings.Contains(err.Error(), "not connected") {
		t.Errorf("bob: err = %v", err)
	}
}

// An unreachable host says so, instead of the "no metadata" message a
// reachable non-OAuth server gets.
func TestDiscoverMcpAuth_UnreachableHostIsNamed(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close() // nothing listens there now

	_, err := DiscoverMcpAuth(context.Background(), http.DefaultClient, base+"/mcp", false)
	if err == nil || !strings.Contains(err.Error(), "could not reach") {
		t.Errorf("unreachable host: err = %v", err)
	}

	up := httptest.NewServer(http.NotFoundHandler())
	defer up.Close()
	_, err = DiscoverMcpAuth(context.Background(), http.DefaultClient, up.URL+"/mcp", false)
	if err == nil || !strings.Contains(err.Error(), "no protected-resource metadata") {
		t.Errorf("reachable non-OAuth server: err = %v", err)
	}
}
