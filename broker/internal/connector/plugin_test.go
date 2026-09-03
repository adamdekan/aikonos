package connector

import (
	"sync"
	"testing"

	"golang.org/x/oauth2"
)

func TestBuiltinPlugins_GoogleDrive(t *testing.T) {
	p, ok := pluginFor(ProviderGoogleDrive)
	if !ok {
		t.Fatal("googleDrivePlugin not registered")
	}
	if got := p.Provider(); got != ProviderGoogleDrive {
		t.Fatalf("Provider() = %q, want %q", got, ProviderGoogleDrive)
	}
	if got := p.ConnectorID(); got != "gdrive" {
		t.Fatalf("ConnectorID() = %q, want gdrive", got)
	}
	if got := p.DisplayName(); got != "Google Drive" {
		t.Fatalf("DisplayName() = %q, want \"Google Drive\"", got)
	}

	scopes := p.DefaultScopes()
	want := []string{
		"https://www.googleapis.com/auth/drive.readonly",
		"https://www.googleapis.com/auth/drive.file",
	}
	if len(scopes) != len(want) {
		t.Fatalf("DefaultScopes() len = %d, want %d: %v", len(scopes), len(want), scopes)
	}
	for i, s := range want {
		if scopes[i] != s {
			t.Fatalf("DefaultScopes()[%d] = %q, want %q", i, scopes[i], s)
		}
	}

	ep := p.Endpoint(EndpointOpts{})
	if ep.AuthURL != googleEndpoint.AuthURL || ep.TokenURL != googleEndpoint.TokenURL {
		t.Fatalf("Endpoint() = %+v, want %+v", ep, googleEndpoint)
	}

	// opts must be exactly {AccessTypeOffline, ApprovalForce} — test by round-tripping
	// through AuthCodeURL to confirm both params appear in the URL.
	opts := p.AuthCodeOptions()
	if len(opts) != 2 {
		t.Fatalf("AuthCodeOptions() len = %d, want 2", len(opts))
	}
	// Apply to a dummy config and verify access_type + prompt appear.
	cfg := &oauth2.Config{
		ClientID:    "x",
		RedirectURL: "http://localhost",
		Endpoint:    googleEndpoint,
	}
	u := cfg.AuthCodeURL("s", opts...)
	for _, sub := range []string{"access_type=offline", "prompt=consent"} {
		if !contains(u, sub) {
			t.Fatalf("AuthCodeOptions URL missing %q: %s", sub, u)
		}
	}

	if got := p.RevokeURL(); got != googleRevokeURL {
		t.Fatalf("RevokeURL() = %q, want %q", got, googleRevokeURL)
	}
}

func TestBuiltinPlugins_OneDrive(t *testing.T) {
	p, ok := pluginFor(ProviderOneDrive)
	if !ok {
		t.Fatal("oneDrivePlugin not registered")
	}
	if got := p.Provider(); got != ProviderOneDrive {
		t.Fatalf("Provider() = %q, want %q", got, ProviderOneDrive)
	}
	if got := p.ConnectorID(); got != "onedrive" {
		t.Fatalf("ConnectorID() = %q, want onedrive", got)
	}
	if got := p.DisplayName(); got != "OneDrive" {
		t.Fatalf("DisplayName() = %q, want OneDrive", got)
	}

	scopes := p.DefaultScopes()
	want := []string{"Files.ReadWrite", "offline_access"}
	if len(scopes) != len(want) {
		t.Fatalf("DefaultScopes() len = %d, want %d: %v", len(scopes), len(want), scopes)
	}
	for i, s := range want {
		if scopes[i] != s {
			t.Fatalf("DefaultScopes()[%d] = %q, want %q", i, scopes[i], s)
		}
	}

	// Without tenant — should fall back to "common".
	ep := p.Endpoint(EndpointOpts{})
	wantEP := microsoftEndpoint("common")
	if ep.AuthURL != wantEP.AuthURL || ep.TokenURL != wantEP.TokenURL {
		t.Fatalf("Endpoint({}) = %+v, want %+v", ep, wantEP)
	}

	// With explicit tenant.
	ep2 := p.Endpoint(EndpointOpts{MicrosoftTenant: "mytenant"})
	wantEP2 := microsoftEndpoint("mytenant")
	if ep2.AuthURL != wantEP2.AuthURL || ep2.TokenURL != wantEP2.TokenURL {
		t.Fatalf("Endpoint({MicrosoftTenant:mytenant}) = %+v, want %+v", ep2, wantEP2)
	}

	if opts := p.AuthCodeOptions(); opts != nil {
		t.Fatalf("AuthCodeOptions() = %v, want nil", opts)
	}
	if got := p.RevokeURL(); got != "" {
		t.Fatalf("RevokeURL() = %q, want empty", got)
	}
}

func TestRegister_PanicOnDuplicate(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Register did not panic on duplicate Provider")
		}
	}()
	// ProviderGoogleDrive is already registered by init().
	Register(&googleDrivePlugin{})
}

func TestPluginFor_UnknownProvider(t *testing.T) {
	_, ok := pluginFor(Provider("nope"))
	if ok {
		t.Fatal("pluginFor unknown provider should return ok=false")
	}
}

// ── CP2 extension-seam test ───────────────────────────────────────────────────

// dropboxProvider is a unique Provider value used only in this test; it must not
// collide with any built-in Provider constant.
const dropboxProvider Provider = "dropbox_test_cp2"

// fakeDropboxPlugin is a minimal Plugin wired for extension-seam testing.
type fakeDropboxPlugin struct{}

func (fakeDropboxPlugin) Provider() Provider                       { return dropboxProvider }
func (fakeDropboxPlugin) ConnectorID() string                      { return "dropbox-test" }
func (fakeDropboxPlugin) DisplayName() string                      { return "Dropbox Test" }
func (fakeDropboxPlugin) DefaultScopes() []string                  { return []string{"files.content.read"} }
func (fakeDropboxPlugin) AuthCodeOptions() []oauth2.AuthCodeOption { return nil }
func (fakeDropboxPlugin) RevokeURL() string                        { return "" }
func (fakeDropboxPlugin) ConfigKey() string                        { return "dropbox_test_cp2" }
func (fakeDropboxPlugin) Endpoint(_ EndpointOpts) oauth2.Endpoint {
	return oauth2.Endpoint{
		AuthURL:  "https://www.dropbox.com/oauth2/authorize",
		TokenURL: "https://api.dropboxapi.com/oauth2/token",
	}
}

// registerDropboxOnce guards against the duplicate-Register panic if this test
// is ever extracted into a sub-test or the file is copied.
var registerDropboxOnce sync.Once

func TestExtensionSeam_ThirdPartyPlugin(t *testing.T) {
	registerDropboxOnce.Do(func() { Register(fakeDropboxPlugin{}) })

	// ConnectorID dispatches through pluginFor — no oauth.go edit needed.
	if got := dropboxProvider.ConnectorID(); got != "dropbox-test" {
		t.Fatalf("ConnectorID() = %q, want dropbox-test", got)
	}
	if got := dropboxProvider.DisplayName(); got != "Dropbox Test" {
		t.Fatalf("DisplayName() = %q, want Dropbox Test", got)
	}

	// OAuthConfig routes through the plugin for endpoint + default scopes.
	r := &Registry{
		RedirectURI: "http://localhost/cb",
	}
	// credsFor returns zero AppCredentials for a provider with no Registry slot,
	// so Configured() is false — assert OAuthConfig takes the error path. The
	// dispatch still flows through the registered plugin (proven above by
	// ConnectorID/DisplayName), with no oauth.go edit.
	if _, err := r.OAuthConfig(dropboxProvider, nil); err == nil {
		t.Fatal("OAuthConfig for unconfigured third-party plugin must error")
	}

	// Verify the plugin is in the registry and has the right endpoint without
	// going through Registry (which needs creds).
	pl, ok := pluginFor(dropboxProvider)
	if !ok {
		t.Fatal("dropbox plugin not found in registry")
	}
	ep := pl.Endpoint(EndpointOpts{})
	if ep.AuthURL != "https://www.dropbox.com/oauth2/authorize" {
		t.Fatalf("Endpoint().AuthURL = %q", ep.AuthURL)
	}
	if scopes := pl.DefaultScopes(); len(scopes) != 1 || scopes[0] != "files.content.read" {
		t.Fatalf("DefaultScopes() = %v", scopes)
	}
}

// TestExtensionSeam_ThirdPartyPlugin_CredentialedViaMap proves the F60 seam: a
// third-party provider becomes Configured() and yields its own credentials
// purely by being present in Registry.Credentials — no oauth.go edit, and no
// hardcoded provider list on Registry. This was impossible before F60 (the
// two-arm credsFor switch had no slot for a non-Google/Microsoft provider).
func TestExtensionSeam_ThirdPartyPlugin_CredentialedViaMap(t *testing.T) {
	registerDropboxOnce.Do(func() { Register(fakeDropboxPlugin{}) })

	r := &Registry{
		RedirectURI: "http://localhost/cb",
		Credentials: map[Provider]AppCredentials{
			dropboxProvider: {ClientID: "dbx-id", ClientSecret: "dbx-secret"},
		},
	}
	if !r.Configured(dropboxProvider) {
		t.Fatal("dropboxProvider should be Configured() once credentialed via the map")
	}
	cfg, err := r.OAuthConfig(dropboxProvider, nil)
	if err != nil {
		t.Fatalf("OAuthConfig: %v", err)
	}
	if cfg.ClientID != "dbx-id" || cfg.ClientSecret != "dbx-secret" {
		t.Fatalf("OAuthConfig creds = %+v, want dbx-id/dbx-secret", cfg)
	}

	// A different registered provider's slot must stay unaffected.
	if r.Configured(ProviderGoogleDrive) {
		t.Fatal("ProviderGoogleDrive should remain unconfigured with no map entry")
	}
}

// TestConfigKeyFor_PreservesExistingDeployedKeys pins the existing
// "connectors.google.*" / "connectors.microsoft.*" viper key segments so main.go's
// map-population loop can never silently rename deployed config.
func TestConfigKeyFor_PreservesExistingDeployedKeys(t *testing.T) {
	if got, ok := ConfigKeyFor(ProviderGoogleDrive); !ok || got != "google" {
		t.Fatalf("ConfigKeyFor(ProviderGoogleDrive) = (%q, %v), want (\"google\", true)", got, ok)
	}
	if got, ok := ConfigKeyFor(ProviderOneDrive); !ok || got != "microsoft" {
		t.Fatalf("ConfigKeyFor(ProviderOneDrive) = (%q, %v), want (\"microsoft\", true)", got, ok)
	}
	if _, ok := ConfigKeyFor(Provider("unregistered")); ok {
		t.Fatal("ConfigKeyFor unregistered provider should return ok=false")
	}
}

// contains is a simple substring check used in URL assertions.
func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
