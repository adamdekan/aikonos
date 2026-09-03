package connector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/secrets"
)

// withFakeMicrosoftLogin points microsoftLoginBase at an httptest server for
// the duration of the test, restoring the real value on cleanup.
func withFakeMicrosoftLogin(t *testing.T, srv *httptest.Server) {
	t.Helper()
	prev := microsoftLoginBase
	microsoftLoginBase = srv.URL
	t.Cleanup(func() { microsoftLoginBase = prev })
}

func TestExchangeOBO_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tenant-a/oauth2/v2.0/token" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if got := r.PostForm.Get("grant_type"); got != oboGrantType {
			t.Errorf("grant_type = %q, want %q", got, oboGrantType)
		}
		if got := r.PostForm.Get("requested_token_use"); got != oboTokenUse {
			t.Errorf("requested_token_use = %q, want %q", got, oboTokenUse)
		}
		if got := r.PostForm.Get("assertion"); got != "caller-bearer" {
			t.Errorf("assertion = %q, want caller-bearer", got)
		}
		if got := r.PostForm.Get("scope"); got != oboScope {
			t.Errorf("scope = %q, want %q", got, oboScope)
		}
		if got := r.PostForm.Get("client_id"); got != "client-1" {
			t.Errorf("client_id = %q, want client-1", got)
		}
		if got := r.PostForm.Get("client_secret"); got != "secret-1" {
			t.Errorf("client_secret = %q, want secret-1", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "at-1",
			"refresh_token": "rt-1",
			"expires_in":    3600,
		})
	}))
	defer srv.Close()
	withFakeMicrosoftLogin(t, srv)

	app := M365App{TenantID: "tenant-a", ClientID: "client-1", ClientSecret: "secret-1"}
	res, err := ExchangeOBO(context.Background(), srv.Client(), app, "caller-bearer")
	if err != nil {
		t.Fatalf("ExchangeOBO: %v", err)
	}
	if res.AccessToken != "at-1" || res.RefreshToken != "rt-1" || res.ExpiresIn != 3600 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestExchangeOBO_ClassifiesAADSTSCodes(t *testing.T) {
	cases := []struct {
		name       string
		desc       string
		wantSubstr string
	}{
		{
			name:       "consent missing",
			desc:       "AADSTS65001: The user or administrator has not consented to use the application.",
			wantSubstr: "admin consent missing",
		},
		{
			name:       "invalid secret",
			desc:       "AADSTS7000215: Invalid client secret provided.",
			wantSubstr: "invalid client secret",
		},
		{
			name:       "audience mismatch",
			desc:       "AADSTS500011: The resource principal named https://graph.microsoft.com was not found.",
			wantSubstr: "audience/app mismatch",
		},
		{
			name:       "unrecognized code passes through",
			desc:       "AADSTS99999: something else entirely went wrong.",
			wantSubstr: "AADSTS99999",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error":             "invalid_grant",
					"error_description": tc.desc,
				})
			}))
			defer srv.Close()
			withFakeMicrosoftLogin(t, srv)

			app := M365App{TenantID: "tenant-a", ClientID: "client-1", ClientSecret: "secret-1"}
			_, err := ExchangeOBO(context.Background(), srv.Client(), app, "caller-bearer")
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

func TestExchangeOBO_NoAccessTokenIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer srv.Close()
	withFakeMicrosoftLogin(t, srv)

	app := M365App{TenantID: "tenant-a", ClientID: "client-1", ClientSecret: "secret-1"}
	if _, err := ExchangeOBO(context.Background(), srv.Client(), app, "caller-bearer"); err == nil {
		t.Fatal("expected an error for a 200 with no access_token")
	}
}

// ── OBOBroker (CP2) ──────────────────────────────────────────────────────────

type fakeTenantAppSource struct {
	app M365App
	ok  bool
	err error
}

func (f *fakeTenantAppSource) M365App(context.Context, string) (M365App, bool, error) {
	return f.app, f.ok, f.err
}

func TestOBOBroker_Configured(t *testing.T) {
	app := M365App{TenantID: "t", ClientID: "c", ClientSecret: "s"}
	b := &OBOBroker{Apps: &fakeTenantAppSource{app: app, ok: true}}
	if !b.Configured(context.Background(), "t") {
		t.Fatal("want configured=true when the app resolves ok")
	}

	unconfigured := &OBOBroker{Apps: &fakeTenantAppSource{ok: false}}
	if unconfigured.Configured(context.Background(), "t") {
		t.Fatal("want configured=false when the tenant has no M365 connection")
	}

	if (&OBOBroker{}).Configured(context.Background(), "t") {
		t.Fatal("want configured=false when Apps is nil")
	}
}

func TestOBOBroker_Bootstrap_Success_WritesTokenAndConnectedStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at-1", "refresh_token": "rt-1", "expires_in": 3600,
		})
	}))
	defer srv.Close()
	withFakeMicrosoftLogin(t, srv)

	store := &fakeSecrets{tokens: map[string]*secrets.ConnectorToken{}}
	fs := &fakeStatus{}
	b := &OBOBroker{
		Apps:    &fakeTenantAppSource{app: M365App{TenantID: "tenant-a", ClientID: "client-1", ClientSecret: "secret-1"}, ok: true},
		Secrets: store,
		Status:  fs,
		HTTP:    srv.Client(),
	}

	if err := b.Bootstrap(context.Background(), "tenant-a", "user-1", "caller-bearer"); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	stored, ok := store.tokens[key("tenant-a", "user-1", ProviderOneDrive.ConnectorID())]
	if !ok {
		t.Fatal("expected the token to be written at the onedrive connector path")
	}
	if stored.AccessToken != "at-1" || stored.RefreshToken != "rt-1" {
		t.Fatalf("unexpected stored token: %+v", stored)
	}
	if stored.ExpiresAt.Before(time.Now()) {
		t.Fatalf("expected a future expiry, got %v", stored.ExpiresAt)
	}
	if len(fs.calls) != 1 || fs.calls[0] != "tenant-a/user-1/"+ProviderOneDrive.ConnectorID()+"/"+StatusConnected {
		t.Fatalf("expected one connected status record, got %v", fs.calls)
	}
}

func TestOBOBroker_Bootstrap_ExchangeFailure_RecordsReconnectNeeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "invalid_grant", "error_description": "AADSTS65001: consent missing.",
		})
	}))
	defer srv.Close()
	withFakeMicrosoftLogin(t, srv)

	store := &fakeSecrets{tokens: map[string]*secrets.ConnectorToken{}}
	fs := &fakeStatus{}
	b := &OBOBroker{
		Apps:    &fakeTenantAppSource{app: M365App{TenantID: "tenant-a", ClientID: "client-1", ClientSecret: "secret-1"}, ok: true},
		Secrets: store,
		Status:  fs,
		HTTP:    srv.Client(),
	}

	if err := b.Bootstrap(context.Background(), "tenant-a", "user-1", "caller-bearer"); err == nil {
		t.Fatal("expected a bootstrap error on a failed exchange")
	}
	if store.writes != 0 {
		t.Fatalf("a failed exchange must not write a token, got %d writes", store.writes)
	}
	if len(fs.calls) != 1 || fs.calls[0] != "tenant-a/user-1/"+ProviderOneDrive.ConnectorID()+"/"+StatusReconnectNeeded {
		t.Fatalf("expected one reconnect_needed status record, got %v", fs.calls)
	}
}

func TestOBOBroker_Bootstrap_TenantUnconfigured(t *testing.T) {
	b := &OBOBroker{Apps: &fakeTenantAppSource{ok: false}, Secrets: &fakeSecrets{tokens: map[string]*secrets.ConnectorToken{}}}
	if err := b.Bootstrap(context.Background(), "tenant-a", "user-1", "bearer"); err == nil {
		t.Fatal("expected an error when the tenant has no M365 connection configured")
	}
}

func TestOBOBroker_FreshAccessToken_ValidTokenNoRefresh(t *testing.T) {
	connectorID := ProviderOneDrive.ConnectorID()
	store := &fakeSecrets{tokens: map[string]*secrets.ConnectorToken{
		key("tenant-a", "user-1", connectorID): {AccessToken: "still-good", RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Hour)},
	}}
	b := &OBOBroker{Secrets: store}

	at, err := b.FreshAccessToken(context.Background(), "tenant-a", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if at != "still-good" {
		t.Fatalf("got %q, want still-good", at)
	}
	if store.writes != 0 {
		t.Fatalf("no write-back expected for a still-valid token, got %d", store.writes)
	}
}

func TestOBOBroker_FreshAccessToken_RefreshesAndPersistsRotatedToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.PostForm.Get("grant_type"); got != "refresh_token" {
			t.Fatalf("grant_type = %q, want refresh_token", got)
		}
		if got := r.PostForm.Get("refresh_token"); got != "old-rt" {
			t.Fatalf("refresh_token = %q, want old-rt", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "refreshed-at", "refresh_token": "rotated-rt", "expires_in": 3600,
		})
	}))
	defer srv.Close()
	withFakeMicrosoftLogin(t, srv)

	connectorID := ProviderOneDrive.ConnectorID()
	store := &fakeSecrets{tokens: map[string]*secrets.ConnectorToken{
		key("tenant-a", "user-1", connectorID): {AccessToken: "expired-at", RefreshToken: "old-rt", ExpiresAt: time.Now().Add(-time.Hour), Scopes: []string{"Files.ReadWrite"}},
	}}
	fs := &fakeStatus{}
	b := &OBOBroker{
		Apps:    &fakeTenantAppSource{app: M365App{TenantID: "tenant-a", ClientID: "client-1", ClientSecret: "secret-1"}, ok: true},
		Secrets: store,
		Status:  fs,
		HTTP:    srv.Client(),
	}

	at, err := b.FreshAccessToken(context.Background(), "tenant-a", "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if at != "refreshed-at" {
		t.Fatalf("got %q, want refreshed-at", at)
	}
	stored := store.tokens[key("tenant-a", "user-1", connectorID)]
	if stored.RefreshToken != "rotated-rt" {
		t.Fatalf("rotated refresh token not persisted: %+v", stored)
	}
	if len(stored.Scopes) != 1 || stored.Scopes[0] != "Files.ReadWrite" {
		t.Fatalf("write-back must preserve the stored scopes, got %v", stored.Scopes)
	}
	if len(fs.calls) != 1 || fs.calls[0] != "tenant-a/user-1/"+connectorID+"/"+StatusConnected {
		t.Fatalf("expected one connected status record, got %v", fs.calls)
	}
}

func TestOBOBroker_FreshAccessToken_InvalidGrant_ReconnectRequired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "invalid_grant", "error_description": "AADSTS700084: refresh token expired.",
		})
	}))
	defer srv.Close()
	withFakeMicrosoftLogin(t, srv)

	connectorID := ProviderOneDrive.ConnectorID()
	store := &fakeSecrets{tokens: map[string]*secrets.ConnectorToken{
		key("tenant-a", "user-1", connectorID): {AccessToken: "expired-at", RefreshToken: "dead-rt", ExpiresAt: time.Now().Add(-time.Hour)},
	}}
	fs := &fakeStatus{}
	b := &OBOBroker{
		Apps:    &fakeTenantAppSource{app: M365App{TenantID: "tenant-a", ClientID: "client-1", ClientSecret: "secret-1"}, ok: true},
		Secrets: store,
		Status:  fs,
		HTTP:    srv.Client(),
	}

	_, err := b.FreshAccessToken(context.Background(), "tenant-a", "user-1")
	if !errors.Is(err, ErrOBOReconnectRequired) {
		t.Fatalf("want ErrOBOReconnectRequired, got %v", err)
	}
	if store.writes != 0 {
		t.Fatalf("a failed refresh must not write a token, got %d writes", store.writes)
	}
	if len(fs.calls) != 1 || fs.calls[0] != "tenant-a/user-1/"+connectorID+"/"+StatusReconnectNeeded {
		t.Fatalf("expected one reconnect_needed status record, got %v", fs.calls)
	}
}

func TestOBOBroker_FreshAccessToken_NotBootstrapped(t *testing.T) {
	b := &OBOBroker{Secrets: &fakeSecrets{missing: true}}
	if _, err := b.FreshAccessToken(context.Background(), "tenant-a", "user-1"); err == nil {
		t.Fatal("expected an error when onedrive has never been bootstrapped")
	}
}

func TestExchangeOBO_EndpointURLEncodesTenant(t *testing.T) {
	// Confirms the tenant id lands in the URL path exactly once, unescaped by
	// url.Values (the path segment, not the form body).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "/" + url.PathEscape("tenant-b") + "/oauth2/v2.0/token"
		if r.URL.Path != want {
			t.Errorf("path = %q, want %q", r.URL.Path, want)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at"})
	}))
	defer srv.Close()
	withFakeMicrosoftLogin(t, srv)

	app := M365App{TenantID: "tenant-b", ClientID: "c", ClientSecret: "s"}
	if _, err := ExchangeOBO(context.Background(), srv.Client(), app, "assertion"); err != nil {
		t.Fatalf("ExchangeOBO: %v", err)
	}
}
