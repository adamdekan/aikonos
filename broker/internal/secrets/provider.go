// Package secrets stores and retrieves per-user external-connector OAuth tokens
// in Vault. Tokens for a user's Google Drive / OneDrive connection live at
// secret/data/workspaces/<tenant>/<user>/connectors/<connector_id> (KV-v2) and
// are fetched just-in-time by the Tool Proxy — never written to a sandbox, never
// logged. The broker authenticates to Vault with one of two methods (Config.
// AuthMethod): "kubernetes" (projected ServiceAccount JWT, in a pod) or
// "approle" (role_id/secret_id, the docker-compose deployment). The AppRole is
// bound to a least-privilege policy scoped to exactly the broker's KV paths
// (deploy/compose/vault-broker-policy.hcl) — the broker never holds the Vault
// root token.
package secrets

import (
	"context"
	"time"
)

// ConnectorToken is the value persisted per connector. It is the minimal OAuth
// material needed to make and refresh provider API calls.
type ConnectorToken struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	Scopes       []string  `json:"scopes"`
}

// Provider is the connector-secret store the broker depends on. A nil Provider
// (Vault disabled) means connector tools are not registered and the connector
// RPCs fail closed — callers must nil-check.
type Provider interface {
	ReadConnector(ctx context.Context, tenant, user, connectorID string) (*ConnectorToken, error)
	WriteConnector(ctx context.Context, tenant, user, connectorID string, t *ConnectorToken) error
	DeleteConnector(ctx context.Context, tenant, user, connectorID string) error

	// ProviderKeyPath returns the logical KV-v2 path for an LLM provider's API
	// key: providers/<tenant>/<providerID>. Validates both segments.
	ProviderKeyPath(tenant, providerID string) (string, error)
	// ReadProviderKey fetches the api_key for a provider. Returns ("", false, nil)
	// when absent (the provider is configured but has no stored key).
	ReadProviderKey(ctx context.Context, tenant, providerID string) (string, bool, error)
	// WriteProviderKey stores the api_key for a provider.
	WriteProviderKey(ctx context.Context, tenant, providerID, key string) error
	// DeleteProviderKey removes the provider key. A missing secret is not an error.
	DeleteProviderKey(ctx context.Context, tenant, providerID string) error

	// ReadM365App fetches the client_secret for a tenant's M365 (Entra OBO)
	// connection, at secrets.M365AppPath(tenant). Returns ("", false, nil) when
	// the tenant has no M365 connection configured yet.
	ReadM365App(ctx context.Context, tenant string) (string, bool, error)
	// WriteM365App stores the client_secret for a tenant's M365 connection.
	WriteM365App(ctx context.Context, tenant, clientSecret string) error
	// DeleteM365App removes the stored client_secret. A missing secret is not
	// an error.
	DeleteM365App(ctx context.Context, tenant string) error
}

// notFoundError is the concrete type behind the ErrNotFound sentinel.
type notFoundError struct{ msg string }

func (e *notFoundError) Error() string { return e.msg }

// ErrNotFound is returned by ReadConnector when no secret exists at the path
// (the user has not connected that provider). Handlers map this to a
// "reconnect needed" tool error rather than a hard failure.
var ErrNotFound = &notFoundError{msg: "secrets: connector not found"}

// ErrCASConflict is returned by WriteCapabilityKey when a concurrent writer
// already created the key (KV-v2 check-and-set, cas=0). The caller should
// re-read and adopt the winner's key.
var ErrCASConflict = &notFoundError{msg: "secrets: cas conflict (key already exists)"}

// ErrMcpBearerNotFound is returned by ReadMcpBearer when no bearer token has
// been stored for the given MCP connection (server requires no auth, or the
// record was deleted). The Tool Proxy treats this as "no Authorization header".
var ErrMcpBearerNotFound = &notFoundError{msg: "secrets: mcp bearer not found"}

// McpBearerStore stores, retrieves, and removes MCP server bearer tokens in
// Vault at the path returned by McpBearerPath (secret/mcp/<tenant>/<id>). The
// broker layer calls Write on Add/Update when a bearer token is present,
// Read just-in-time in the Tool Proxy MCP client, and Delete (best-effort) on
// Remove.
type McpBearerStore interface {
	WriteMcpBearer(ctx context.Context, path, bearer string) error
	ReadMcpBearer(ctx context.Context, tenant, id string) (string, error)
	DeleteMcpBearer(ctx context.Context, path string) error
}

// McpBearerWriter is the write-only subset retained for callers that only need
// to provision tokens (the admin RPCs). New callers should prefer McpBearerStore.
type McpBearerWriter interface {
	WriteMcpBearer(ctx context.Context, path, bearer string) error
	DeleteMcpBearer(ctx context.Context, path string) error
}
