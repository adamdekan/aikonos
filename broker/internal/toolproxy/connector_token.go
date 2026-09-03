// broker/internal/toolproxy/connector_token.go
//
// Shared plumbing for the cloud-storage connector tools (gdrive.*, onedrive.*).
// A connector handler resolves the calling user's OAuth token just-in-time from
// Vault, refreshes it if expired (persisting the rotated token back), and makes
// the provider API call with the fresh access token. Tokens never touch a
// sandbox filesystem and are never logged.
package toolproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/connector"
	"github.com/adamdekan/aikonos/broker/internal/secrets"
)

// oboTokenSource is the narrow interface freshAccessToken needs from the
// tenant-wide OneDrive on-behalf-of broker,
// satisfied by *connector.OBOBroker. Config.OBO nil, or Configured()==false
// for the caller's tenant, means OBO-first routing is disabled entirely —
// every onedrive.* call falls back to the legacy per-user Registry path
// exactly as before this checkpoint. gdrive.* never consults this.
type oboTokenSource interface {
	Configured(ctx context.Context, tenant string) bool
	FreshAccessToken(ctx context.Context, tenant, user string) (string, error)
}

// connectorDeps is what the connector handlers need beyond the workspace: the
// per-user secret store and the OAuth app registry (for refresh), plus the
// optional tenant-wide OneDrive OBO broker (onedrive only — see
// freshAccessToken). client is injectable for tests; nil uses the shared
// default.
type connectorDeps struct {
	secrets secrets.Provider
	oauth   *connector.Registry
	obo     oboTokenSource
	client  *http.Client
}

var defaultConnectorClient = &http.Client{Timeout: 30 * time.Second}

func (d connectorDeps) httpClient() *http.Client {
	if d.client != nil {
		return d.client
	}
	return defaultConnectorClient
}

// connectorIdentity validates the per-user context a connector call needs. Both
// segments are required: the token lives at (tenant, user) and there is no
// connector token for the user-less server-side executor path.
func connectorIdentity(req Request) (tenant, user string, err error) {
	if req.TenantID == "" {
		return "", "", fmt.Errorf("connector tools require a tenant context")
	}
	if req.UserID == "" {
		return "", "", fmt.Errorf("connector tools require a user context")
	}
	return req.TenantID, req.UserID, nil
}

// freshAccessToken resolves the user's access token for a provider API call.
// For onedrive, when the tenant has a configured tenant-wide OBO connection
//, the OBO broker is preferred over the
// legacy per-user Registry path — and an OBO error propagates as-is, with no
// silent fallback to the legacy per-user token (the Invariants' "falls back
// to legacy" applies only when the tenant isn't OBO-configured at all, not
// when a configured OBO exchange itself fails). gdrive.* is entirely
// unaffected: d.obo is only ever consulted for ProviderOneDrive.
func (d connectorDeps) freshAccessToken(ctx context.Context, p connector.Provider, tenant, user string) (string, error) {
	if p == connector.ProviderOneDrive && d.obo != nil && d.obo.Configured(ctx, tenant) {
		return d.obo.FreshAccessToken(ctx, tenant, user)
	}
	return d.oauth.FreshAccessToken(ctx, d.secrets, p, tenant, user)
}

// doRequest performs an authorized request and returns the response body bytes.
// A non-2xx status returns an error carrying a body snippet.
func (d connectorDeps) doRequest(ctx context.Context, method, url, accessToken, contentType string, body []byte) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+accessToken)
	if contentType != "" {
		httpReq.Header.Set("Content-Type", contentType)
	}
	resp, err := d.httpClient().Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, defaultMaxBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := respBody
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return nil, fmt.Errorf("provider API status %d: %s", resp.StatusCode, strings.TrimSpace(decodeToUTF8(snippet)))
	}
	return respBody, nil
}

// doRequestBounded performs an authorized GET bounded by maxBytes with loud
// overflow detection, unlike doRequest's silent defaultMaxBytes clip. Content
// echoed straight into the (already-bounded) LLM context tolerates silent
// truncation; a staged (save_to) download does not — a truncated docx/xlsx is
// a corrupt file, so this variant errors naming size and cap instead of
// writing a partial workspace copy.
func (d connectorDeps) doRequestBounded(ctx context.Context, url, accessToken string, maxBytes int64) ([]byte, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := d.httpClient().Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("provider API status %d: %s", resp.StatusCode, strings.TrimSpace(decodeToUTF8(snippet)))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		size := resp.ContentLength
		if size <= 0 {
			size = int64(len(data)) // Content-Length absent: report the lower bound we actually read.
		}
		return nil, fmt.Errorf("staged download of %d bytes exceeds workspace cap of %d bytes", size, maxBytes)
	}
	return data, nil
}

// fetchContent downloads a GET url's body, choosing the silent-clip default
// (maxBytes<=0) or the loud-overflow staged bound (maxBytes>0, see
// doRequestBounded) — the single dispatch point so readOne implementations
// don't need to know which cap semantics apply.
func (d connectorDeps) fetchContent(ctx context.Context, url, accessToken string, maxBytes int64) ([]byte, error) {
	if maxBytes > 0 {
		return d.doRequestBounded(ctx, url, accessToken, maxBytes)
	}
	return d.doRequest(ctx, "GET", url, accessToken, "", nil)
}

// doJSON performs an authorized request and decodes a JSON response into out.
func (d connectorDeps) doJSON(ctx context.Context, method, url, accessToken, contentType string, body []byte, out any) error {
	b, err := d.doRequest(ctx, method, url, accessToken, contentType, body)
	if err != nil {
		return err
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}
