package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// defaultJWTPath is where Kubernetes projects the pod ServiceAccount token the
// broker presents to Vault's kubernetes auth backend.
const defaultJWTPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// Auth methods the client can use to obtain a Vault token.
const (
	// AuthKubernetes exchanges a projected ServiceAccount JWT for a token. Only
	// works in a Kubernetes pod (the JWT file must exist). Default.
	AuthKubernetes = "kubernetes"
	// AuthAppRole exchanges a role_id + secret_id for a token. The deployment
	// method under docker compose, where there is no SA token. The role is bound
	// to a least-privilege policy scoped to exactly the broker's KV paths — the
	// broker never holds the Vault root token.
	AuthAppRole = "approle"
)

// tokenRenewSkew re-logs in this long before the Vault client token's lease
// expires, so an in-flight request never races a 403.
const tokenRenewSkew = 60 * time.Second

// Config configures the Vault client. Addr empty means Vault is disabled and
// NewVaultClient returns (nil, nil) so the caller treats connectors as off.
type Config struct {
	Addr       string // e.g. http://vault:8200
	AuthMethod string // "kubernetes" (default) or "approle"
	Role       string // kubernetes auth role (aikonos-broker); required for kubernetes auth
	RoleID     string // approle role_id; required for approle auth
	SecretID   string // approle secret_id; required for approle auth
	KVMount    string // KV-v2 mount; defaults to "secret"
	JWTPath    string // SA token path; defaults to defaultJWTPath (kubernetes auth only)
}

// VaultClient is a minimal KV-v2 + kubernetes-auth client. It is deliberately
// hand-rolled (no hashicorp/vault/api dependency): the surface is one login
// call plus KV read/write/delete.
type VaultClient struct {
	addr       string
	authMethod string
	role       string
	roleID     string
	secretID   string
	kvMount    string
	jwtPath    string
	httpc      *http.Client

	now func() time.Time

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
}

// NewVaultClient builds a client, or returns (nil, nil) when Addr is empty
// (Vault disabled). It does not log in eagerly — the first secret access does.
func NewVaultClient(cfg Config) (*VaultClient, error) {
	if cfg.Addr == "" {
		return nil, nil
	}
	method := cfg.AuthMethod
	if method == "" {
		method = AuthKubernetes
	}
	switch method {
	case AuthKubernetes:
		if cfg.Role == "" {
			return nil, fmt.Errorf("secrets: vault role required for kubernetes auth")
		}
	case AuthAppRole:
		if cfg.RoleID == "" || cfg.SecretID == "" {
			return nil, fmt.Errorf("secrets: vault role_id and secret_id required for approle auth")
		}
	default:
		return nil, fmt.Errorf("secrets: unknown vault auth method %q", method)
	}
	mount := cfg.KVMount
	if mount == "" {
		mount = "secret"
	}
	jwtPath := cfg.JWTPath
	if jwtPath == "" {
		jwtPath = defaultJWTPath
	}
	return &VaultClient{
		addr:       strings.TrimRight(cfg.Addr, "/"),
		authMethod: method,
		role:       cfg.Role,
		roleID:     cfg.RoleID,
		secretID:   cfg.SecretID,
		kvMount:    mount,
		jwtPath:    jwtPath,
		httpc:      &http.Client{Timeout: 10 * time.Second},
		now:        time.Now,
	}, nil
}

type loginResponse struct {
	Auth struct {
		ClientToken   string `json:"client_token"`
		LeaseDuration int    `json:"lease_duration"`
	} `json:"auth"`
}

// loginRequest builds the auth-backend URL and request body for the configured
// auth method. The secret material (SA JWT or secret_id) is read here, never
// logged.
func (c *VaultClient) loginRequest() (string, []byte, error) {
	switch c.authMethod {
	case AuthAppRole:
		body, _ := json.Marshal(map[string]string{
			"role_id":   c.roleID,
			"secret_id": c.secretID,
		})
		return c.addr + "/v1/auth/approle/login", body, nil
	default: // AuthKubernetes
		jwt, err := os.ReadFile(c.jwtPath)
		if err != nil {
			return "", nil, fmt.Errorf("secrets: read SA token: %w", err)
		}
		body, _ := json.Marshal(map[string]string{
			"role": c.role,
			"jwt":  strings.TrimSpace(string(jwt)),
		})
		return c.addr + "/v1/auth/kubernetes/login", body, nil
	}
}

// login exchanges the configured credential for a Vault client token. The auth
// method (kubernetes SA-JWT or approle role_id/secret_id) is chosen at
// construction. Caller holds c.mu.
func (c *VaultClient) login(ctx context.Context) error {
	url, body, err := c.loginRequest()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("secrets: vault login: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("secrets: vault login status %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	var lr loginResponse
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return fmt.Errorf("secrets: decode login: %w", err)
	}
	if lr.Auth.ClientToken == "" {
		return fmt.Errorf("secrets: vault login returned no token")
	}
	c.token = lr.Auth.ClientToken
	ttl := time.Duration(lr.Auth.LeaseDuration) * time.Second
	c.tokenExpiry = c.now().Add(ttl)
	return nil
}

// ensureToken logs in if there is no token yet or the current one is within the
// renew skew of expiry. Caller holds c.mu.
func (c *VaultClient) ensureToken(ctx context.Context) error {
	if c.token != "" && c.now().Before(c.tokenExpiry.Add(-tokenRenewSkew)) {
		return nil
	}
	return c.login(ctx)
}

// do performs an authenticated Vault request, logging in as needed.
func (c *VaultClient) do(ctx context.Context, method, path string, payload any) (*http.Response, error) {
	c.mu.Lock()
	if err := c.ensureToken(ctx); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	token := c.token
	c.mu.Unlock()

	var rdr io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.addr+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Vault-Token", token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.httpc.Do(req)
}

// dataPath is the KV-v2 data path for a connector secret. KV-v2 inserts the
// "data" segment between the mount and the logical path.
func (c *VaultClient) dataPath(tenant, user, connectorID string) (string, error) {
	seg, err := connectorSegments(tenant, user, connectorID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("/v1/%s/data/%s", c.kvMount, seg), nil
}

// metaPath is the KV-v2 metadata path; deleting it removes all versions.
func (c *VaultClient) metaPath(tenant, user, connectorID string) (string, error) {
	seg, err := connectorSegments(tenant, user, connectorID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("/v1/%s/metadata/%s", c.kvMount, seg), nil
}

// validSegment reports whether s is safe to interpolate into both a Vault KV
// path and a filesystem path: a strict charset (alnum plus . _ - @) that admits
// emails and UUIDs but no '/', '..', whitespace, or URL-significant character
// (?, #, %).
func validSegment(s string) bool {
	if s == "" || len(s) > 256 || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-' || r == '@':
		default:
			return false
		}
	}
	return true
}

// ConnectorPath returns the logical KV path (without the KV-v2 "data/" segment)
// for a user's connector secret, validating every segment against validSegment.
// It is the single source of truth for the path so the stored Vault location and
// the workspace reference can never drift.
func ConnectorPath(tenant, user, connectorID string) (string, error) {
	for _, s := range []string{tenant, user, connectorID} {
		if !validSegment(s) {
			return "", fmt.Errorf("secrets: invalid path segment %q", s)
		}
	}
	return fmt.Sprintf("workspaces/%s/%s/connectors/%s", tenant, user, connectorID), nil
}

func connectorSegments(tenant, user, connectorID string) (string, error) {
	return ConnectorPath(tenant, user, connectorID)
}

type kvReadResponse struct {
	Data struct {
		Data ConnectorToken `json:"data"`
	} `json:"data"`
}

// ReadConnector fetches a connector token. A 404 maps to ErrNotFound.
func (c *VaultClient) ReadConnector(ctx context.Context, tenant, user, connectorID string) (*ConnectorToken, error) {
	path, err := c.dataPath(tenant, user, connectorID)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("secrets: read status %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	var kr kvReadResponse
	if err := json.NewDecoder(resp.Body).Decode(&kr); err != nil {
		return nil, fmt.Errorf("secrets: decode read: %w", err)
	}
	return &kr.Data.Data, nil
}

// WriteConnector stores (creates or overwrites) a connector token.
func (c *VaultClient) WriteConnector(ctx context.Context, tenant, user, connectorID string, t *ConnectorToken) error {
	if t == nil {
		return fmt.Errorf("secrets: nil token")
	}
	path, err := c.dataPath(tenant, user, connectorID)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPost, path, map[string]any{"data": t})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("secrets: write status %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	return nil
}

// DeleteConnector removes all versions of a connector secret (used on revoke).
// A missing secret is not an error.
func (c *VaultClient) DeleteConnector(ctx context.Context, tenant, user, connectorID string) error {
	path, err := c.metaPath(tenant, user, connectorID)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("secrets: delete status %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	return nil
}

// capabilityKeyLogical is the fixed KV-v2 logical path for the broker's Biscuit
// capability root key. One key per cluster, shared by every broker replica.
const capabilityKeyLogical = "broker/capability"

// gatewayGrantKeyLogical is the fixed KV-v2 logical path for the 32-byte HMAC
// key used to sign owner-grant tokens. Mirrored from capabilityKeyLogical —
// same durability semantics (cas=0, shared by every replica).
const gatewayGrantKeyLogical = "broker/gateway-grant"

func (c *VaultClient) capabilityDataPath() string {
	return fmt.Sprintf("/v1/%s/data/%s", c.kvMount, capabilityKeyLogical)
}

type kvCapabilityResponse struct {
	Data struct {
		Data struct {
			RootKey string `json:"root_key"`
		} `json:"data"`
	} `json:"data"`
}

// ReadCapabilityKey returns the base64 ed25519 root key, or ErrNotFound if it
// has not been provisioned yet.
func (c *VaultClient) ReadCapabilityKey(ctx context.Context) (string, error) {
	resp, err := c.do(ctx, http.MethodGet, c.capabilityDataPath(), nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("secrets: read capability status %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	var kr kvCapabilityResponse
	if err := json.NewDecoder(resp.Body).Decode(&kr); err != nil {
		return "", fmt.Errorf("secrets: decode capability: %w", err)
	}
	if kr.Data.Data.RootKey == "" {
		return "", ErrNotFound
	}
	return kr.Data.Data.RootKey, nil
}

// WriteCapabilityKey stores the root key with check-and-set cas=0, so only the
// first writer in a concurrent first-boot wins; the rest get ErrCASConflict and
// re-read.
func (c *VaultClient) WriteCapabilityKey(ctx context.Context, b64 string) error {
	payload := map[string]any{
		"options": map[string]any{"cas": 0},
		"data":    map[string]string{"root_key": b64},
	}
	resp, err := c.do(ctx, http.MethodPost, c.capabilityDataPath(), payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusBadRequest {
		// KV-v2 returns 400 on a cas mismatch (key already exists).
		return ErrCASConflict
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("secrets: write capability status %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	return nil
}

// ── Gateway-grant key ─────────────────────────────────────────────────────────
// The gateway-grant key is a 32-byte random value used as the HMAC-SHA256 key
// for owner-grant tokens. Mirrors the capability key: cas=0 on first boot,
// shared across replicas, durable across broker restarts (Vault KV-v2).

func (c *VaultClient) gatewayGrantDataPath() string {
	return fmt.Sprintf("/v1/%s/data/%s", c.kvMount, gatewayGrantKeyLogical)
}

type kvGatewayGrantResponse struct {
	Data struct {
		Data struct {
			GrantKey string `json:"grant_key"`
		} `json:"data"`
	} `json:"data"`
}

// ReadGatewayGrantKey returns the base64-encoded 32-byte grant key, or
// ErrNotFound if it has not been provisioned yet.
func (c *VaultClient) ReadGatewayGrantKey(ctx context.Context) (string, error) {
	resp, err := c.do(ctx, http.MethodGet, c.gatewayGrantDataPath(), nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("secrets: read gateway-grant status %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	var gr kvGatewayGrantResponse
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return "", fmt.Errorf("secrets: decode gateway-grant: %w", err)
	}
	if gr.Data.Data.GrantKey == "" {
		return "", ErrNotFound
	}
	return gr.Data.Data.GrantKey, nil
}

// WriteGatewayGrantKey stores the grant key with check-and-set cas=0, so only
// the first writer in a concurrent first-boot wins; the rest get ErrCASConflict
// and re-read.
func (c *VaultClient) WriteGatewayGrantKey(ctx context.Context, b64 string) error {
	payload := map[string]any{
		"options": map[string]any{"cas": 0},
		"data":    map[string]string{"grant_key": b64},
	}
	resp, err := c.do(ctx, http.MethodPost, c.gatewayGrantDataPath(), payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusBadRequest {
		// KV-v2 returns 400 on a cas mismatch (key already exists).
		return ErrCASConflict
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("secrets: write gateway-grant status %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	return nil
}

// ── Audit signing key ──────────────────────────────────────────────────────
// The audit signing key is a 32-byte random value used as the HMAC-SHA256 key
// for audit-event signatures. Mirrors the capability/gateway-grant keys:
// cas=0 on first boot, shared across replicas, durable across broker
// restarts (Vault KV-v2) — versioned, so a rotation (new KV write) never
// invalidates events signed under the prior version; VerifyChain resolves
// each event's key by its recorded signing_key_version instead.
const auditSigningKeyLogical = "broker/audit-signing-key"

func (c *VaultClient) auditSigningKeyDataPath() string {
	return fmt.Sprintf("/v1/%s/data/%s", c.kvMount, auditSigningKeyLogical)
}

func (c *VaultClient) auditSigningKeyDataPathVersion(version int32) string {
	return fmt.Sprintf("/v1/%s/data/%s?version=%d", c.kvMount, auditSigningKeyLogical, version)
}

type kvAuditSigningKeyResponse struct {
	Data struct {
		Data struct {
			Key string `json:"key"`
		} `json:"data"`
		Metadata struct {
			Version int32 `json:"version"`
		} `json:"metadata"`
	} `json:"data"`
}

// ReadAuditSigningKey returns the base64-encoded 32-byte signing key and its
// KV-v2 version, or ErrNotFound if it has not been provisioned yet.
func (c *VaultClient) ReadAuditSigningKey(ctx context.Context) (string, int32, error) {
	resp, err := c.do(ctx, http.MethodGet, c.auditSigningKeyDataPath(), nil)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", 0, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("secrets: read audit-signing-key status %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	var kr kvAuditSigningKeyResponse
	if err := json.NewDecoder(resp.Body).Decode(&kr); err != nil {
		return "", 0, fmt.Errorf("secrets: decode audit-signing-key: %w", err)
	}
	if kr.Data.Data.Key == "" {
		return "", 0, ErrNotFound
	}
	return kr.Data.Data.Key, kr.Data.Metadata.Version, nil
}

// ReadAuditSigningKeyVersion reads a specific historic KV-v2 version of the
// audit signing key (used by VerifyChain to resolve events signed before a
// rotation). ErrNotFound if that version doesn't exist or was destroyed.
func (c *VaultClient) ReadAuditSigningKeyVersion(ctx context.Context, version int32) (string, error) {
	resp, err := c.do(ctx, http.MethodGet, c.auditSigningKeyDataPathVersion(version), nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("secrets: read audit-signing-key version %d status %d: %s", version, resp.StatusCode, readSnippet(resp.Body))
	}
	var kr kvAuditSigningKeyResponse
	if err := json.NewDecoder(resp.Body).Decode(&kr); err != nil {
		return "", fmt.Errorf("secrets: decode audit-signing-key version %d: %w", version, err)
	}
	if kr.Data.Data.Key == "" {
		return "", ErrNotFound
	}
	return kr.Data.Data.Key, nil
}

// WriteAuditSigningKey stores the signing key with check-and-set cas=0, so
// only the first writer in a concurrent first-boot wins; the rest get
// ErrCASConflict and re-read. Subsequent rotations are a manual operator
// action (docs/OPS-RUNBOOK.md) that writes a new version directly, bypassing
// this cas=0 guard.
func (c *VaultClient) WriteAuditSigningKey(ctx context.Context, b64 string) error {
	payload := map[string]any{
		"options": map[string]any{"cas": 0},
		"data":    map[string]string{"key": b64},
	}
	resp, err := c.do(ctx, http.MethodPost, c.auditSigningKeyDataPath(), payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusBadRequest {
		// KV-v2 returns 400 on a cas mismatch (key already exists).
		return ErrCASConflict
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("secrets: write audit-signing-key status %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	return nil
}

// ── Workspace session integrity key (CP4.2) ─────────────────────────────────
// The workspace session key is a 32-byte random value used as the HMAC-SHA256
// key for session-file (.agent/Sessions/*) integrity sidecars. Mirrors the
// gateway-grant key: cas=0 on first boot, shared across replicas, unversioned
// (no rotation-spanning verification is needed for CP4.2 — see
//  CP4.2).
const workspaceSessionKeyLogical = "broker/workspace-session-key"

func (c *VaultClient) workspaceSessionKeyDataPath() string {
	return fmt.Sprintf("/v1/%s/data/%s", c.kvMount, workspaceSessionKeyLogical)
}

type kvWorkspaceSessionKeyResponse struct {
	Data struct {
		Data struct {
			Key string `json:"key"`
		} `json:"data"`
	} `json:"data"`
}

// ReadWorkspaceSessionKey returns the base64-encoded 32-byte signing key, or
// ErrNotFound if it has not been provisioned yet.
func (c *VaultClient) ReadWorkspaceSessionKey(ctx context.Context) (string, error) {
	resp, err := c.do(ctx, http.MethodGet, c.workspaceSessionKeyDataPath(), nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("secrets: read workspace-session-key status %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	var kr kvWorkspaceSessionKeyResponse
	if err := json.NewDecoder(resp.Body).Decode(&kr); err != nil {
		return "", fmt.Errorf("secrets: decode workspace-session-key: %w", err)
	}
	if kr.Data.Data.Key == "" {
		return "", ErrNotFound
	}
	return kr.Data.Data.Key, nil
}

// WriteWorkspaceSessionKey stores the signing key with check-and-set cas=0, so
// only the first writer in a concurrent first-boot wins; the rest get
// ErrCASConflict and re-read.
func (c *VaultClient) WriteWorkspaceSessionKey(ctx context.Context, b64 string) error {
	payload := map[string]any{
		"options": map[string]any{"cas": 0},
		"data":    map[string]string{"key": b64},
	}
	resp, err := c.do(ctx, http.MethodPost, c.workspaceSessionKeyDataPath(), payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusBadRequest {
		// KV-v2 returns 400 on a cas mismatch (key already exists).
		return ErrCASConflict
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("secrets: write workspace-session-key status %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	return nil
}

// ProviderKeyPath returns the logical KV-v2 path for an LLM provider's API key:
// providers/<tenant>/<providerID>. Validates both segments against validSegment.
func ProviderKeyPath(tenant, providerID string) (string, error) {
	for _, s := range []string{tenant, providerID} {
		if !validSegment(s) {
			return "", fmt.Errorf("secrets: invalid path segment %q", s)
		}
	}
	return fmt.Sprintf("providers/%s/%s", tenant, providerID), nil
}

// ProviderKeyPath satisfies the Provider interface, delegating to the package-
// level function so the path logic has a single source of truth.
func (c *VaultClient) ProviderKeyPath(tenant, providerID string) (string, error) {
	return ProviderKeyPath(tenant, providerID)
}

type kvProviderKeyResponse struct {
	Data struct {
		Data struct {
			APIKey string `json:"api_key"`
		} `json:"data"`
	} `json:"data"`
}

// ReadProviderKey fetches the api_key for a provider. Returns ("", false, nil)
// when no secret exists at the path.
func (c *VaultClient) ReadProviderKey(ctx context.Context, tenant, providerID string) (string, bool, error) {
	logical, err := ProviderKeyPath(tenant, providerID)
	if err != nil {
		return "", false, err
	}
	kvPath := fmt.Sprintf("/v1/%s/data/%s", c.kvMount, logical)
	resp, err := c.do(ctx, http.MethodGet, kvPath, nil)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("secrets: read provider key status %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	var kr kvProviderKeyResponse
	if err := json.NewDecoder(resp.Body).Decode(&kr); err != nil {
		return "", false, fmt.Errorf("secrets: decode provider key: %w", err)
	}
	if kr.Data.Data.APIKey == "" {
		return "", false, nil
	}
	return kr.Data.Data.APIKey, true, nil
}

// WriteProviderKey stores the api_key for a provider. Creates or overwrites.
func (c *VaultClient) WriteProviderKey(ctx context.Context, tenant, providerID, key string) error {
	logical, err := ProviderKeyPath(tenant, providerID)
	if err != nil {
		return err
	}
	kvPath := fmt.Sprintf("/v1/%s/data/%s", c.kvMount, logical)
	resp, err := c.do(ctx, http.MethodPost, kvPath, map[string]any{
		"data": map[string]string{"api_key": key},
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("secrets: write provider key status %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	return nil
}

// DeleteProviderKey removes all versions of the provider key secret.
// A missing secret is not an error.
func (c *VaultClient) DeleteProviderKey(ctx context.Context, tenant, providerID string) error {
	logical, err := ProviderKeyPath(tenant, providerID)
	if err != nil {
		return err
	}
	metaPath := fmt.Sprintf("/v1/%s/metadata/%s", c.kvMount, logical)
	resp, err := c.do(ctx, http.MethodDelete, metaPath, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		return nil
	}
	return fmt.Errorf("secrets: delete provider key status %d: %s", resp.StatusCode, readSnippet(resp.Body))
}

// M365AppPath returns the logical KV-v2 path for a tenant's M365 (Entra OBO)
// app secret: m365/<tenant>/app. Validates the tenant segment. Unlike
// ProviderKeyPath this is not part of the Provider interface — it is a plain
// package function, mirroring the ConnectorPath/McpBearerPath precedent.
func M365AppPath(tenant string) (string, error) {
	if !validSegment(tenant) {
		return "", fmt.Errorf("secrets: invalid path segment %q", tenant)
	}
	return fmt.Sprintf("m365/%s/app", tenant), nil
}

type kvM365AppResponse struct {
	Data struct {
		Data struct {
			ClientSecret string `json:"client_secret"`
		} `json:"data"`
	} `json:"data"`
}

// ReadM365App fetches the client_secret for a tenant's M365 connection.
// Returns ("", false, nil) when unconfigured.
func (c *VaultClient) ReadM365App(ctx context.Context, tenant string) (string, bool, error) {
	logical, err := M365AppPath(tenant)
	if err != nil {
		return "", false, err
	}
	kvPath := fmt.Sprintf("/v1/%s/data/%s", c.kvMount, logical)
	resp, err := c.do(ctx, http.MethodGet, kvPath, nil)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("secrets: read m365 app status %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	var kr kvM365AppResponse
	if err := json.NewDecoder(resp.Body).Decode(&kr); err != nil {
		return "", false, fmt.Errorf("secrets: decode m365 app: %w", err)
	}
	if kr.Data.Data.ClientSecret == "" {
		return "", false, nil
	}
	return kr.Data.Data.ClientSecret, true, nil
}

// WriteM365App stores the client_secret for a tenant's M365 connection.
// Creates or overwrites.
func (c *VaultClient) WriteM365App(ctx context.Context, tenant, clientSecret string) error {
	logical, err := M365AppPath(tenant)
	if err != nil {
		return err
	}
	kvPath := fmt.Sprintf("/v1/%s/data/%s", c.kvMount, logical)
	resp, err := c.do(ctx, http.MethodPost, kvPath, map[string]any{
		"data": map[string]string{"client_secret": clientSecret},
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("secrets: write m365 app status %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	return nil
}

// DeleteM365App removes all versions of the tenant's M365 app secret. A
// missing secret is not an error.
func (c *VaultClient) DeleteM365App(ctx context.Context, tenant string) error {
	logical, err := M365AppPath(tenant)
	if err != nil {
		return err
	}
	metaPath := fmt.Sprintf("/v1/%s/metadata/%s", c.kvMount, logical)
	resp, err := c.do(ctx, http.MethodDelete, metaPath, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		return nil
	}
	return fmt.Errorf("secrets: delete m365 app status %d: %s", resp.StatusCode, readSnippet(resp.Body))
}

// WebSearchAppPath returns the logical KV-v2 path for a tenant's web.search
// engine API key: websearch/<tenant>/app. Validates
// the tenant segment. Deliberately kept off the Provider interface (CP1
// decision, unchanged by CP2): toolproxy's web.search plugin and
// broker/internal/broker's websearch_admin.go both type-assert *VaultClient
// for this surface directly, so adding it doesn't force every other
// secrets.Provider fake in the repo to grow three stub methods for a feature
// they don't touch.
func WebSearchAppPath(tenant string) (string, error) {
	if !validSegment(tenant) {
		return "", fmt.Errorf("secrets: invalid path segment %q", tenant)
	}
	return fmt.Sprintf("websearch/%s/app", tenant), nil
}

type kvWebSearchAppResponse struct {
	Data struct {
		Data struct {
			APIKey string `json:"api_key"`
		} `json:"data"`
	} `json:"data"`
}

// ReadWebSearchApp fetches the api_key for a tenant's web.search engine.
// Returns ("", false, nil) when unconfigured.
func (c *VaultClient) ReadWebSearchApp(ctx context.Context, tenant string) (string, bool, error) {
	logical, err := WebSearchAppPath(tenant)
	if err != nil {
		return "", false, err
	}
	kvPath := fmt.Sprintf("/v1/%s/data/%s", c.kvMount, logical)
	resp, err := c.do(ctx, http.MethodGet, kvPath, nil)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("secrets: read websearch app status %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	var kr kvWebSearchAppResponse
	if err := json.NewDecoder(resp.Body).Decode(&kr); err != nil {
		return "", false, fmt.Errorf("secrets: decode websearch app: %w", err)
	}
	if kr.Data.Data.APIKey == "" {
		return "", false, nil
	}
	return kr.Data.Data.APIKey, true, nil
}

// WriteWebSearchApp stores the api_key for a tenant's web.search engine.
// Creates or overwrites.
func (c *VaultClient) WriteWebSearchApp(ctx context.Context, tenant, apiKey string) error {
	logical, err := WebSearchAppPath(tenant)
	if err != nil {
		return err
	}
	kvPath := fmt.Sprintf("/v1/%s/data/%s", c.kvMount, logical)
	resp, err := c.do(ctx, http.MethodPost, kvPath, map[string]any{
		"data": map[string]string{"api_key": apiKey},
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("secrets: write websearch app status %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	return nil
}

// DeleteWebSearchApp removes all versions of the tenant's web.search api key.
// A missing key is not an error.
func (c *VaultClient) DeleteWebSearchApp(ctx context.Context, tenant string) error {
	logical, err := WebSearchAppPath(tenant)
	if err != nil {
		return err
	}
	metaPath := fmt.Sprintf("/v1/%s/metadata/%s", c.kvMount, logical)
	resp, err := c.do(ctx, http.MethodDelete, metaPath, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		return nil
	}
	return fmt.Errorf("secrets: delete websearch app status %d: %s", resp.StatusCode, readSnippet(resp.Body))
}

// McpBearerPath returns the logical KV-v2 path for an MCP connection's bearer
// token: mcp/<tenant>/<id>. Validates each segment.
func McpBearerPath(tenant, id string) (string, error) {
	for _, s := range []string{tenant, id} {
		if !validSegment(s) {
			return "", fmt.Errorf("secrets: invalid path segment %q", s)
		}
	}
	return fmt.Sprintf("mcp/%s/%s", tenant, id), nil
}

// WriteMcpBearer stores a bearer token at the given logical KV-v2 path (as
// returned by McpBearerPath). Creates or overwrites.
func (c *VaultClient) WriteMcpBearer(ctx context.Context, path, bearer string) error {
	kvPath := fmt.Sprintf("/v1/%s/data/%s", c.kvMount, path)
	resp, err := c.do(ctx, http.MethodPost, kvPath, map[string]any{
		"data": map[string]string{"bearer_token": bearer},
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("secrets: write mcp bearer status %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	return nil
}

// ReadMcpBearer fetches the bearer token stored at McpBearerPath(tenant, id).
// A 404 maps to ErrMcpBearerNotFound so the Tool Proxy can skip the
// Authorization header rather than failing hard.
func (c *VaultClient) ReadMcpBearer(ctx context.Context, tenant, id string) (string, error) {
	logical, err := McpBearerPath(tenant, id)
	if err != nil {
		return "", err
	}
	kvPath := fmt.Sprintf("/v1/%s/data/%s", c.kvMount, logical)
	resp, err := c.do(ctx, http.MethodGet, kvPath, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", ErrMcpBearerNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("secrets: read mcp bearer status %d: %s", resp.StatusCode, readSnippet(resp.Body))
	}
	var kr struct {
		Data struct {
			Data struct {
				BearerToken string `json:"bearer_token"`
			} `json:"data"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&kr); err != nil {
		return "", fmt.Errorf("secrets: decode mcp bearer: %w", err)
	}
	if kr.Data.Data.BearerToken == "" {
		return "", ErrMcpBearerNotFound
	}
	return kr.Data.Data.BearerToken, nil
}

// DeleteMcpBearer removes all versions of the MCP bearer token secret.
// A 404 (missing) is treated as success so Delete is idempotent.
func (c *VaultClient) DeleteMcpBearer(ctx context.Context, path string) error {
	metaPath := fmt.Sprintf("/v1/%s/metadata/%s", c.kvMount, path)
	resp, err := c.do(ctx, http.MethodDelete, metaPath, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		return nil
	}
	return fmt.Errorf("secrets: delete mcp bearer status %d: %s", resp.StatusCode, readSnippet(resp.Body))
}

func readSnippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 512))
	return strings.TrimSpace(string(b))
}
