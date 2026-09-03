package connector

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testRegistry() *Registry {
	return &Registry{
		RedirectURI: "http://localhost:3000/connect/callback",
		Credentials: map[Provider]AppCredentials{
			ProviderGoogleDrive: {ClientID: "g-id", ClientSecret: "g-secret"},
			ProviderOneDrive:    {ClientID: "m-id", ClientSecret: "m-secret"},
		},
		MicrosoftTenant: "common",
	}
}

func TestConfigured(t *testing.T) {
	r := &Registry{Credentials: map[Provider]AppCredentials{
		ProviderGoogleDrive: {ClientID: "x", ClientSecret: "y"},
	}}
	if !r.Configured(ProviderGoogleDrive) {
		t.Fatal("google should be configured")
	}
	if r.Configured(ProviderOneDrive) {
		t.Fatal("onedrive should be unconfigured")
	}
}

func TestAuthCodeURL_GoogleHasPKCEAndOffline(t *testing.T) {
	r := testRegistry()
	raw, err := r.AuthCodeURL(ProviderGoogleDrive, nil, "state-123", "verifier-abc")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if got := q.Get("code_challenge_method"); got != "S256" {
		t.Fatalf("code_challenge_method = %q, want S256", got)
	}
	if q.Get("code_challenge") == "" {
		t.Fatal("missing code_challenge")
	}
	if q.Get("state") != "state-123" {
		t.Fatalf("state = %q", q.Get("state"))
	}
	if q.Get("access_type") != "offline" {
		t.Fatalf("access_type = %q, want offline", q.Get("access_type"))
	}
	if q.Get("redirect_uri") != "http://localhost:3000/connect/callback" {
		t.Fatalf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if !strings.HasPrefix(raw, "https://accounts.google.com/") {
		t.Fatalf("unexpected auth host: %s", raw)
	}
	if scope := q.Get("scope"); !strings.Contains(scope, "drive.file") {
		t.Fatalf("scope missing drive.file: %q", scope)
	}
}

func TestAuthCodeURL_MicrosoftTenantAndScopes(t *testing.T) {
	r := testRegistry()
	raw, err := r.AuthCodeURL(ProviderOneDrive, nil, "s", "v")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, "https://login.microsoftonline.com/common/oauth2/v2.0/authorize") {
		t.Fatalf("unexpected MS auth url: %s", raw)
	}
	u, _ := url.Parse(raw)
	if scope := u.Query().Get("scope"); !strings.Contains(scope, "offline_access") {
		t.Fatalf("scope missing offline_access: %q", scope)
	}
}

func TestAuthCodeURL_UnconfiguredProviderErrors(t *testing.T) {
	r := &Registry{} // no creds
	if _, err := r.AuthCodeURL(ProviderGoogleDrive, nil, "s", "v"); err == nil {
		t.Fatal("expected error for unconfigured provider")
	}
}

func TestProviderConnectorID(t *testing.T) {
	if ProviderGoogleDrive.ConnectorID() != "gdrive" || ProviderOneDrive.ConnectorID() != "onedrive" {
		t.Fatal("unexpected connector ids")
	}
}

func TestStateStore_SingleUse(t *testing.T) {
	ctx := context.Background()
	s := NewStateStore(10 * time.Minute)
	_ = s.Put(ctx, "st", PendingAuth{Provider: ProviderGoogleDrive, Tenant: "t", User: "u", Verifier: "v"})
	got, ok, err := s.Take(ctx, "t", "st")
	if err != nil || !ok || got.Verifier != "v" {
		t.Fatalf("first take failed: %v %v %v", got, ok, err)
	}
	if _, ok, _ := s.Take(ctx, "t", "st"); ok {
		t.Fatal("state should be single-use")
	}
}

func TestStateStore_TenantMismatch(t *testing.T) {
	ctx := context.Background()
	s := NewStateStore(time.Minute)
	_ = s.Put(ctx, "st", PendingAuth{Tenant: "t1", Verifier: "v"})
	if _, ok, _ := s.Take(ctx, "t2", "st"); ok {
		t.Fatal("state minted for another tenant must not be returned")
	}
}

func TestStateStore_Expiry(t *testing.T) {
	ctx := context.Background()
	s := NewStateStore(5 * time.Minute)
	clock := time.Unix(1000, 0)
	s.now = func() time.Time { return clock }
	_ = s.Put(ctx, "st", PendingAuth{Tenant: "t", Verifier: "v"})
	clock = clock.Add(6 * time.Minute)
	if _, ok, _ := s.Take(ctx, "t", "st"); ok {
		t.Fatal("expired state should not be returned")
	}
}

func TestStateStore_UnknownState(t *testing.T) {
	ctx := context.Background()
	s := NewStateStore(time.Minute)
	if _, ok, _ := s.Take(ctx, "t", "nope"); ok {
		t.Fatal("unknown state should return false")
	}
}

func TestGenerateStateUnique(t *testing.T) {
	a, err := GenerateState()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := GenerateState()
	if a == "" || a == b {
		t.Fatalf("states not unique/non-empty: %q %q", a, b)
	}
}
