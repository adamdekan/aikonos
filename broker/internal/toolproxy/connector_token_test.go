// broker/internal/toolproxy/connector_token_test.go
//
// CP7: freshAccessToken prefers the
// tenant-wide OneDrive OBO broker over the legacy per-user Registry path when
// the tenant is OBO-configured, propagates an OBO error as-is (no silent
// fallback), and never consults OBO at all for gdrive.
package toolproxy

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"unicode/utf8"

	"github.com/adamdekan/aikonos/broker/internal/connector"
	"github.com/adamdekan/aikonos/broker/internal/secrets"
)

// TestConnectorErrorSnippet_Latin1BodyIsValidUTF8 pins the encoding fix on both
// snippet-building paths: a provider returning a non-2xx with a Latin-1 body
// (0xE1 = á, invalid UTF-8) must not leak raw bytes into the error string — it
// flows into InvokeToolResponse.Error (a proto3 string) which proto.Marshal
// rejects on invalid UTF-8, and the service_invoke backstop only guards the
// result Struct, not the Error field.
func TestConnectorErrorSnippet_Latin1BodyIsValidUTF8(t *testing.T) {
	latin1 := []byte("provider blew up: M\xe1jer not found")
	srv := newTestServer(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write(latin1)
	})
	defer srv.Close()
	d := connectorDeps{client: srv.Client()}

	if _, err := d.doRequest(context.Background(), "GET", srv.URL, "tok", "", nil); err == nil {
		t.Fatal("doRequest: expected an error on 500")
	} else if !utf8.ValidString(err.Error()) {
		t.Errorf("doRequest error is not valid UTF-8: %q", err.Error())
	}

	if _, err := d.doRequestBounded(context.Background(), srv.URL, "tok", 1<<20); err == nil {
		t.Fatal("doRequestBounded: expected an error on 500")
	} else if !utf8.ValidString(err.Error()) {
		t.Errorf("doRequestBounded error is not valid UTF-8: %q", err.Error())
	}
}

// fakeOBO is a fake oboTokenSource for connectorDeps.obo.
type fakeOBO struct {
	configured      bool
	token           string
	err             error
	configuredCalls int
	tokenCalls      int
}

func (f *fakeOBO) Configured(ctx context.Context, tenant string) bool {
	f.configuredCalls++
	return f.configured
}

func (f *fakeOBO) FreshAccessToken(ctx context.Context, tenant, user string) (string, error) {
	f.tokenCalls++
	return f.token, f.err
}

// countingSecrets wraps a secrets.Provider, counting ReadConnector calls —
// the legacy connector.Registry.FreshAccessToken path always calls
// ReadConnector first, so a zero count proves the legacy path was never
// consulted at all.
type countingSecrets struct {
	secrets.Provider
	reads int
}

func (c *countingSecrets) ReadConnector(ctx context.Context, tenant, user, connectorID string) (*secrets.ConnectorToken, error) {
	c.reads++
	return c.Provider.ReadConnector(ctx, tenant, user, connectorID)
}

func oneDriveConfiguredDeps(secretsStore secrets.Provider, obo oboTokenSource) connectorDeps {
	return connectorDeps{
		secrets: secretsStore,
		oauth: &connector.Registry{
			Credentials: map[connector.Provider]connector.AppCredentials{
				connector.ProviderOneDrive: {ClientID: "m-id", ClientSecret: "m-secret"},
			},
		},
		obo: obo,
	}
}

func TestFreshAccessToken_OneDrive_OBOConfigured_PrefersOBO(t *testing.T) {
	obo := &fakeOBO{configured: true, token: "obo-token"}
	cs := &countingSecrets{Provider: fakeStore{at: "legacy-at"}}
	d := oneDriveConfiguredDeps(cs, obo)

	token, err := d.freshAccessToken(context.Background(), connector.ProviderOneDrive, "tenant1", "user1")
	if err != nil {
		t.Fatalf("freshAccessToken: %v", err)
	}
	if token != "obo-token" {
		t.Errorf("token = %q, want %q", token, "obo-token")
	}
	if cs.reads != 0 {
		t.Errorf("expected the legacy Registry path to never be consulted, ReadConnector called %d times", cs.reads)
	}
	if obo.tokenCalls != 1 {
		t.Errorf("expected FreshAccessToken called once on the OBO broker, got %d", obo.tokenCalls)
	}
}

func TestFreshAccessToken_OneDrive_NilOBO_FallsBackToLegacy(t *testing.T) {
	cs := &countingSecrets{Provider: fakeStore{at: "legacy-at"}}
	d := oneDriveConfiguredDeps(cs, nil) // obo nil

	_, _ = d.freshAccessToken(context.Background(), connector.ProviderOneDrive, "tenant1", "user1")
	if cs.reads == 0 {
		t.Errorf("expected the legacy Registry path to be consulted when obo is nil, ReadConnector never called")
	}
}

func TestFreshAccessToken_OneDrive_OBONotConfigured_FallsBackToLegacy(t *testing.T) {
	obo := &fakeOBO{configured: false}
	cs := &countingSecrets{Provider: fakeStore{at: "legacy-at"}}
	d := oneDriveConfiguredDeps(cs, obo)

	_, _ = d.freshAccessToken(context.Background(), connector.ProviderOneDrive, "tenant1", "user1")
	if obo.tokenCalls != 0 {
		t.Errorf("expected obo.FreshAccessToken to never be called when the tenant isn't OBO-configured, got %d calls", obo.tokenCalls)
	}
	if cs.reads == 0 {
		t.Errorf("expected the legacy Registry path to be consulted when the tenant isn't OBO-configured")
	}
}

func TestFreshAccessToken_OneDrive_OBOConfiguredButErrors_PropagatesWithoutFallback(t *testing.T) {
	wantErr := errors.New("obo: refresh token invalid")
	obo := &fakeOBO{configured: true, err: wantErr}
	cs := &countingSecrets{Provider: fakeStore{at: "legacy-at"}}
	d := oneDriveConfiguredDeps(cs, obo)

	_, err := d.freshAccessToken(context.Background(), connector.ProviderOneDrive, "tenant1", "user1")
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected the OBO error to propagate unwrapped, got %v", err)
	}
	if cs.reads != 0 {
		t.Errorf("expected the legacy Registry path to never be consulted on an OBO error, ReadConnector called %d times", cs.reads)
	}
}

func TestFreshAccessToken_GDrive_NeverConsultsOBO(t *testing.T) {
	obo := &fakeOBO{configured: true, token: "obo-token"}
	cs := &countingSecrets{Provider: fakeStore{at: "legacy-at"}}
	d := connectorDeps{
		secrets: cs,
		oauth: &connector.Registry{
			Credentials: map[connector.Provider]connector.AppCredentials{
				connector.ProviderGoogleDrive: {ClientID: "g-id", ClientSecret: "g-secret"},
			},
		},
		obo: obo,
	}

	_, _ = d.freshAccessToken(context.Background(), connector.ProviderGoogleDrive, "tenant1", "user1")
	if obo.configuredCalls != 0 {
		t.Errorf("expected gdrive to never even call obo.Configured, got %d calls", obo.configuredCalls)
	}
	if cs.reads == 0 {
		t.Errorf("expected the legacy Registry path to be consulted for gdrive")
	}
}
