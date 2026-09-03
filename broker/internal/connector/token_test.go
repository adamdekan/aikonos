package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/connectorstore"
	"github.com/adamdekan/aikonos/broker/internal/secrets"
)

type fakeSecrets struct {
	tokens  map[string]*secrets.ConnectorToken
	writes  int
	missing bool
}

func key(t, u, c string) string { return t + "/" + u + "/" + c }

func (f *fakeSecrets) ReadConnector(_ context.Context, t, u, c string) (*secrets.ConnectorToken, error) {
	if f.missing {
		return nil, secrets.ErrNotFound
	}
	tok, ok := f.tokens[key(t, u, c)]
	if !ok {
		return nil, secrets.ErrNotFound
	}
	return tok, nil
}
func (f *fakeSecrets) WriteConnector(_ context.Context, t, u, c string, tok *secrets.ConnectorToken) error {
	f.writes++
	f.tokens[key(t, u, c)] = tok
	return nil
}
func (f *fakeSecrets) DeleteConnector(_ context.Context, t, u, c string) error {
	delete(f.tokens, key(t, u, c))
	return nil
}
func (f *fakeSecrets) ProviderKeyPath(tenant, providerID string) (string, error) {
	return secrets.ProviderKeyPath(tenant, providerID)
}
func (f *fakeSecrets) ReadProviderKey(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}
func (f *fakeSecrets) WriteProviderKey(context.Context, string, string, string) error { return nil }
func (f *fakeSecrets) DeleteProviderKey(context.Context, string, string) error        { return nil }
func (f *fakeSecrets) ReadM365App(context.Context, string) (string, bool, error) {
	return "", false, nil
}
func (f *fakeSecrets) WriteM365App(context.Context, string, string) error { return nil }
func (f *fakeSecrets) DeleteM365App(context.Context, string) error        { return nil }

// fakeStatus records UpdateStatus calls in order; failNext makes the next N
// calls return an error without recording (to test the best-effort contract).
type fakeStatus struct {
	calls    []string // "tenant/user/connectorID/status"
	failNext int
}

func (f *fakeStatus) UpdateStatus(tenant, user, connectorID, status string) error {
	if f.failNext > 0 {
		f.failNext--
		return fmt.Errorf("store write failed")
	}
	f.calls = append(f.calls, tenant+"/"+user+"/"+connectorID+"/"+status)
	return nil
}

func TestFreshAccessToken_RefreshFailureRecordsReconnectNeeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	orig := googleEndpoint
	googleEndpoint.TokenURL = srv.URL
	defer func() { googleEndpoint = orig }()

	r := testRegistry()
	fs := &fakeStatus{}
	r.Status = fs
	store := &fakeSecrets{tokens: map[string]*secrets.ConnectorToken{
		key("t", "u", "gdrive"): {AccessToken: "expired-at", RefreshToken: "old-rt", ExpiresAt: time.Now().Add(-time.Hour)},
	}}

	if _, err := r.FreshAccessToken(context.Background(), store, ProviderGoogleDrive, "t", "u"); err == nil {
		t.Fatal("expected refresh failure error")
	}
	if len(fs.calls) != 1 || fs.calls[0] != "t/u/gdrive/reconnect_needed" {
		t.Fatalf("expected one reconnect_needed record, got %v", fs.calls)
	}
}

func TestFreshAccessToken_RefreshSuccessResetsConnected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "refreshed-at", "refresh_token": "rotated-rt",
			"token_type": "Bearer", "expires_in": 3600,
		})
	}))
	defer srv.Close()

	orig := googleEndpoint
	googleEndpoint.TokenURL = srv.URL
	defer func() { googleEndpoint = orig }()

	r := testRegistry()
	fs := &fakeStatus{}
	r.Status = fs
	store := &fakeSecrets{tokens: map[string]*secrets.ConnectorToken{
		key("t", "u", "gdrive"): {AccessToken: "expired-at", RefreshToken: "old-rt", ExpiresAt: time.Now().Add(-time.Hour)},
	}}

	if _, err := r.FreshAccessToken(context.Background(), store, ProviderGoogleDrive, "t", "u"); err != nil {
		t.Fatal(err)
	}
	if len(fs.calls) != 1 || fs.calls[0] != "t/u/gdrive/connected" {
		t.Fatalf("expected one connected record on refresh success, got %v", fs.calls)
	}
}

func TestFreshAccessToken_StatusWriteFailureDoesNotAffectFailureResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	orig := googleEndpoint
	googleEndpoint.TokenURL = srv.URL
	defer func() { googleEndpoint = orig }()

	r := testRegistry()
	r.Status = &fakeStatus{failNext: 1}
	store := &fakeSecrets{tokens: map[string]*secrets.ConnectorToken{
		key("t", "u", "gdrive"): {AccessToken: "expired-at", RefreshToken: "old-rt", ExpiresAt: time.Now().Add(-time.Hour)},
	}}

	// A broken status store must not change the refresh outcome: still an error.
	if _, err := r.FreshAccessToken(context.Background(), store, ProviderGoogleDrive, "t", "u"); err == nil {
		t.Fatal("expected refresh failure error despite status write failure")
	}
}

func TestFreshAccessToken_StatusWriteFailureDoesNotAffectSuccessResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "refreshed-at", "refresh_token": "rotated-rt",
			"token_type": "Bearer", "expires_in": 3600,
		})
	}))
	defer srv.Close()

	orig := googleEndpoint
	googleEndpoint.TokenURL = srv.URL
	defer func() { googleEndpoint = orig }()

	r := testRegistry()
	r.Status = &fakeStatus{failNext: 1}
	store := &fakeSecrets{tokens: map[string]*secrets.ConnectorToken{
		key("t", "u", "gdrive"): {AccessToken: "expired-at", RefreshToken: "old-rt", ExpiresAt: time.Now().Add(-time.Hour)},
	}}

	// A broken status store must not change the refresh outcome: still succeeds
	// with the refreshed token.
	at, err := r.FreshAccessToken(context.Background(), store, ProviderGoogleDrive, "t", "u")
	if err != nil {
		t.Fatal(err)
	}
	if at != "refreshed-at" {
		t.Fatalf("got %q, want refreshed-at", at)
	}
}

// TestFreshAccessToken_RealStoreReflectsRefreshOutcome is an integration check
// against the real connectorstore.Store — the thing ListConnectors reads —
// rather than a fake, proving the entry the UI would see actually flips.
func TestFreshAccessToken_RealStoreReflectsRefreshOutcome(t *testing.T) {
	cs := connectorstore.New(t.TempDir())
	if err := cs.Upsert("t", "u", connectorstore.Entry{ConnectorID: "gdrive", Status: "connected"}); err != nil {
		t.Fatal(err)
	}

	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer failSrv.Close()

	orig := googleEndpoint
	googleEndpoint.TokenURL = failSrv.URL
	defer func() { googleEndpoint = orig }()

	r := testRegistry()
	r.Status = cs
	store := &fakeSecrets{tokens: map[string]*secrets.ConnectorToken{
		key("t", "u", "gdrive"): {AccessToken: "expired-at", RefreshToken: "old-rt", ExpiresAt: time.Now().Add(-time.Hour)},
	}}

	if _, err := r.FreshAccessToken(context.Background(), store, ProviderGoogleDrive, "t", "u"); err == nil {
		t.Fatal("expected refresh failure")
	}
	entries, err := cs.List("t", "u")
	if err != nil || len(entries) != 1 {
		t.Fatalf("list: %v %v", entries, err)
	}
	if entries[0].Status != "reconnect_needed" {
		t.Fatalf("ListConnectors would echo %q, want reconnect_needed", entries[0].Status)
	}
}

func TestFreshAccessToken_NotConnected(t *testing.T) {
	r := testRegistry()
	store := &fakeSecrets{missing: true}
	if _, err := r.FreshAccessToken(context.Background(), store, ProviderGoogleDrive, "t", "u"); err == nil {
		t.Fatal("expected not-connected error")
	}
}

func TestFreshAccessToken_ValidTokenNoRefresh(t *testing.T) {
	r := testRegistry()
	fs := &fakeStatus{}
	r.Status = fs
	store := &fakeSecrets{tokens: map[string]*secrets.ConnectorToken{
		key("t", "u", "gdrive"): {AccessToken: "still-good", RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Hour)},
	}}
	at, err := r.FreshAccessToken(context.Background(), store, ProviderGoogleDrive, "t", "u")
	if err != nil {
		t.Fatal(err)
	}
	if at != "still-good" {
		t.Fatalf("got %q, want still-good", at)
	}
	if store.writes != 0 {
		t.Fatalf("no write-back expected for a valid token, got %d", store.writes)
	}
	if len(fs.calls) != 0 {
		t.Fatalf("cached-valid-token path must not touch the status store, got %v", fs.calls)
	}
}

func TestFreshAccessToken_RefreshesAndWritesBack(t *testing.T) {
	// Fake Google token endpoint returns a rotated access+refresh token.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != "refresh_token" {
			t.Fatalf("unexpected grant_type %q", r.Form.Get("grant_type"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "refreshed-at", "refresh_token": "rotated-rt",
			"token_type": "Bearer", "expires_in": 3600,
		})
	}))
	defer srv.Close()

	orig := googleEndpoint
	googleEndpoint.TokenURL = srv.URL
	defer func() { googleEndpoint = orig }()

	r := testRegistry()
	store := &fakeSecrets{tokens: map[string]*secrets.ConnectorToken{
		key("t", "u", "gdrive"): {AccessToken: "expired-at", RefreshToken: "old-rt", ExpiresAt: time.Now().Add(-time.Hour)},
	}}
	at, err := r.FreshAccessToken(context.Background(), store, ProviderGoogleDrive, "t", "u")
	if err != nil {
		t.Fatal(err)
	}
	if at != "refreshed-at" {
		t.Fatalf("got %q, want refreshed-at", at)
	}
	if store.writes != 1 {
		t.Fatalf("expected one write-back, got %d", store.writes)
	}
	stored := store.tokens[key("t", "u", "gdrive")]
	if stored.AccessToken != "refreshed-at" || stored.RefreshToken != "rotated-rt" {
		t.Fatalf("write-back did not persist rotated token: %+v", stored)
	}
}
