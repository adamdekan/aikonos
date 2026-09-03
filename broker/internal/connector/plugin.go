package connector

import (
	"sort"

	"golang.org/x/oauth2"
)

// EndpointOpts carries provider-specific parameters needed to build the OAuth
// endpoint. MicrosoftTenant selects the Azure AD tenant; empty → "common".
type EndpointOpts struct {
	MicrosoftTenant string
}

// Plugin describes one cloud-storage provider's OAuth behaviour. Implementations
// self-register via init() so adding a provider requires no edits to oauth.go.
type Plugin interface {
	Provider() Provider
	ConnectorID() string
	DisplayName() string
	DefaultScopes() []string
	Endpoint(EndpointOpts) oauth2.Endpoint
	// AuthCodeOptions returns extra oauth2.AuthCodeOption values appended after
	// the always-present PKCE S256 challenge. nil means no extras (e.g. OneDrive).
	AuthCodeOptions() []oauth2.AuthCodeOption
	// RevokeURL is the token-revocation endpoint. Empty string means the
	// provider has no simple per-token revoke (e.g. OneDrive).
	RevokeURL() string
	// ConfigKey is the "connectors.<key>.*" viper segment this provider's app
	// credentials are read from. Decoupled from Provider's own string value and
	// from ConnectorID so existing deployed config keys (e.g. "google",
	// "microsoft") never have to change when a Provider constant does.
	ConfigKey() string
}

var plugins = map[Provider]Plugin{}

// Register adds p to the package-level registry. Panics on a duplicate Provider
// because registration happens at init time and a collision is a programming error.
func Register(p Plugin) {
	if _, exists := plugins[p.Provider()]; exists {
		panic("connector: duplicate plugin registration for provider " + string(p.Provider()))
	}
	plugins[p.Provider()] = p
}

func pluginFor(p Provider) (Plugin, bool) {
	pl, ok := plugins[p]
	return pl, ok
}

// ConfigKeyFor returns the registered plugin's viper config-key segment (see
// Plugin.ConfigKey) for p, and whether p is a registered provider at all.
// main.go uses this to populate Registry.Credentials without hardcoding the
// built-in provider list.
func ConfigKeyFor(p Provider) (string, bool) {
	pl, ok := pluginFor(p)
	if !ok {
		return "", false
	}
	return pl.ConfigKey(), true
}

// RegisteredProviders returns every self-registered provider, sorted by string
// value for deterministic output. It is the single source of truth for which
// providers exist — callers that enumerate providers (e.g. the connector-provider
// listing RPC) iterate this rather than maintaining a parallel hardcoded list.
func RegisteredProviders() []Provider {
	out := make([]Provider, 0, len(plugins))
	for p := range plugins {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ── built-in: Google Drive ────────────────────────────────────────────────────

type googleDrivePlugin struct{}

func (googleDrivePlugin) Provider() Provider                      { return ProviderGoogleDrive }
func (googleDrivePlugin) ConnectorID() string                     { return "gdrive" }
func (googleDrivePlugin) DisplayName() string                     { return "Google Drive" }
func (googleDrivePlugin) RevokeURL() string                       { return googleRevokeURL }
func (googleDrivePlugin) ConfigKey() string                       { return "google" }
func (googleDrivePlugin) Endpoint(_ EndpointOpts) oauth2.Endpoint { return googleEndpoint }

func (googleDrivePlugin) DefaultScopes() []string {
	return []string{
		"https://www.googleapis.com/auth/drive.readonly",
		"https://www.googleapis.com/auth/drive.file",
	}
}

func (googleDrivePlugin) AuthCodeOptions() []oauth2.AuthCodeOption {
	return []oauth2.AuthCodeOption{oauth2.AccessTypeOffline, oauth2.ApprovalForce}
}

// ── built-in: OneDrive ────────────────────────────────────────────────────────

type oneDrivePlugin struct{}

func (oneDrivePlugin) Provider() Provider  { return ProviderOneDrive }
func (oneDrivePlugin) ConnectorID() string { return "onedrive" }
func (oneDrivePlugin) DisplayName() string { return "OneDrive" }
func (oneDrivePlugin) RevokeURL() string   { return "" }
func (oneDrivePlugin) ConfigKey() string   { return "microsoft" }

func (oneDrivePlugin) Endpoint(opts EndpointOpts) oauth2.Endpoint {
	return microsoftEndpoint(opts.MicrosoftTenant)
}

func (oneDrivePlugin) DefaultScopes() []string {
	return []string{"Files.ReadWrite", "offline_access"}
}

func (oneDrivePlugin) AuthCodeOptions() []oauth2.AuthCodeOption { return nil }

// ── self-registration ─────────────────────────────────────────────────────────

func init() {
	Register(googleDrivePlugin{})
	Register(oneDrivePlugin{})
}
