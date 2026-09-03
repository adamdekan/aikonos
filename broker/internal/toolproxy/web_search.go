// broker/internal/toolproxy/web_search.go
//
// web.search — read_only admin-configured search-engine query
//. Composes with web.fetch: this tool returns
// ranked {title, url, snippet} links only, never fetched content — the model
// loads whichever result it chooses via web.fetch. Brave, Exa, and Tavily ship
//; the searchEngine interface makes a further
// engine additive.
package toolproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"

	"github.com/adamdekan/aikonos/broker/internal/netacl"
)

// searchResult is one ranked hit returned to the model.
type searchResult struct {
	Title   string
	URL     string
	Snippet string
}

// searchEngine queries one admin-configured search provider.
type searchEngine interface {
	Search(ctx context.Context, apiKey, query string, maxResults int) ([]searchResult, error)
}

// searchEngines maps a websearch_engine org_settings value to its
// implementation. An unknown/unset key fails closed at the handler — never a
// default engine.
var searchEngines = map[string]func(client *http.Client, baseURL string) searchEngine{
	"brave":  newBraveEngine,
	"exa":    newExaEngine,
	"tavily": newTavilyEngine,
}

// searchEngineProbeURLs is the fixed production endpoint each engine's
// TestWebSearchConfig admin probe validates + calls — the same host each engine's Search hits in production, just with a
// placeholder one-result query. Kept as a var (not const) so tests can point
// an engine at an httptest server.
var searchEngineProbeURLs = map[string]string{
	"brave":  braveSearchURL,
	"exa":    exaSearchURL,
	"tavily": tavilySearchURL,
}

// ProbeSearchEngine issues one minimal, SSRF-guarded query against engine's
// real endpoint using apiKey, reusing the exact searchEngine implementation
// web.search calls in production — the admin Test probe's only per-engine
// HTTP shape lives here, never duplicated in the broker package
//.
func ProbeSearchEngine(ctx context.Context, engine, apiKey string, allowPrivate bool, timeout time.Duration) error {
	newEngine, ok := searchEngines[engine]
	if !ok {
		return fmt.Errorf("engine %q is not supported", engine)
	}
	probeURL := searchEngineProbeURLs[engine]
	if err := ValidatePublicURL(probeURL, allowPrivate); err != nil {
		return err
	}
	client := NewHardenedClient(timeout, allowPrivate)
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err := newEngine(client, probeURL).Search(reqCtx, apiKey, "test", 1)
	return err
}

// webSearchKeyReader is the minimal read-side Vault seam this checkpoint
// needs.
// Satisfied by *secrets.VaultClient's new ReadWebSearchApp method via a type
// assertion on Config.Secrets rather than an addition to the shared
// secrets.Provider interface — CP2 owns the full read/write/delete + admin
// RPC surface and can promote this onto Provider then; doing it here would
// otherwise force every secrets.Provider test fake in the repo (broker/,
// connector/, llmseed/ packages) to grow a stub method for a feature they
// don't touch.
type webSearchKeyReader interface {
	ReadWebSearchApp(ctx context.Context, tenant string) (string, bool, error)
}

// WebSearchSettings resolves a tenant's configured engine + max_results.
// CP1 has no org_settings columns yet (CP2 adds
// WebsearchEngine/WebsearchMaxResults) — this is the seam main.go wires onto
// db.OrgSettingsRepo then. A nil Settings on WebSearchConfig resolves every
// tenant as unconfigured, the safe default until wired.
type WebSearchSettings interface {
	// Resolve reports the tenant's engine + max_results. configured=false
	// means the tenant has not set up web search (no/unset engine).
	Resolve(ctx context.Context, tenant string) (engine string, maxResults int, configured bool, err error)
}

// WebSearchConfig configures the web.search handler.
type WebSearchConfig struct {
	Settings WebSearchSettings
	// ACL gates result hosts, mirroring WebFetchConfig.ACL. Nil → no filtering.
	ACL NetACL
	// BraveBaseURL/ExaBaseURL/TavilyBaseURL override that engine's API
	// endpoint; empty uses the real one. Tests point these at an httptest
	// server.
	BraveBaseURL  string
	ExaBaseURL    string
	TavilyBaseURL string
	Timeout       time.Duration
	// HTTPClient is injectable for tests; nil builds a default client.
	HTTPClient *http.Client
}

const (
	defaultSearchTimeout = 15 * time.Second
	defaultMaxResults    = 10
	braveSearchURL       = "https://api.search.brave.com/res/v1/web/search"
	exaSearchURL         = "https://api.exa.ai/search"
	tavilySearchURL      = "https://api.tavily.com/search"
)

// webSearchPlugin self-registers web.search, available whenever the proxy
// has a secrets provider (mirrors the connector plugins' Available check).
type webSearchPlugin struct{}

func (webSearchPlugin) ToolID() string            { return "web.search" }
func (webSearchPlugin) Available(cfg Config) bool { return cfg.Secrets != nil }
func (webSearchPlugin) Build(cfg Config) Handler {
	var keys webSearchKeyReader
	if kr, ok := cfg.Secrets.(webSearchKeyReader); ok {
		keys = kr
	}
	return newWebSearchHandler(cfg.WebSearch, keys)
}
func (webSearchPlugin) Scope() string                   { return "web:search" }
func (webSearchPlugin) EffectClass() planv1.EffectClass { return planv1.EffectClass_READ_ONLY }

func init() { RegisterPlugin(webSearchPlugin{}) }

// errWebSearchNotConfigured is the in-band tool error surfaced when the
// tenant hasn't set up web search — never a broker-level failure.
var errWebSearchNotConfigured = fmt.Errorf("web.search: web search is not configured for this tenant — ask an admin")

func newWebSearchHandler(cfg WebSearchConfig, keys webSearchKeyReader) Handler {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultSearchTimeout
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}

	return func(ctx context.Context, req Request) (map[string]any, int64, error) {
		query, _ := req.Args["query"].(string)
		if query == "" {
			return nil, 0, fmt.Errorf("web.search: missing required arg %q", "query")
		}

		if cfg.Settings == nil {
			return nil, 0, errWebSearchNotConfigured
		}
		engineName, maxResults, configured, err := cfg.Settings.Resolve(ctx, req.TenantID)
		if err != nil {
			return nil, 0, fmt.Errorf("web.search: resolving tenant config: %w", err)
		}
		if !configured {
			return nil, 0, errWebSearchNotConfigured
		}
		newEngine, ok := searchEngines[engineName]
		if !ok {
			return nil, 0, fmt.Errorf("web.search: engine %q is not supported", engineName)
		}
		if maxResults <= 0 {
			maxResults = defaultMaxResults
		}

		if keys == nil {
			return nil, 0, errWebSearchNotConfigured
		}
		apiKey, ok, err := keys.ReadWebSearchApp(ctx, req.TenantID)
		if err != nil {
			return nil, 0, fmt.Errorf("web.search: reading api key: %w", err)
		}
		if !ok || apiKey == "" {
			return nil, 0, errWebSearchNotConfigured
		}

		baseURL := cfg.BraveBaseURL
		switch engineName {
		case "exa":
			baseURL = cfg.ExaBaseURL
		case "tavily":
			baseURL = cfg.TavilyBaseURL
		}
		results, err := newEngine(client, baseURL).Search(ctx, apiKey, query, maxResults)
		if err != nil {
			return nil, 1, fmt.Errorf("web.search: %w", err)
		}

		// []any, not []map[string]any: structpb only walks []any when converting
		// a list, so a typed slice fails ResultStruct and the whole result is
		// dropped on the way back to the model.
		out := make([]any, 0, len(results))
		dropped := 0
		for _, r := range results {
			if cfg.ACL != nil && cfg.ACL.Enabled() {
				if cfg.ACL.Decide(ctx, req.TenantID, req.UserID, hostOf(r.URL)) == netacl.Deny {
					dropped++
					continue
				}
			}
			out = append(out, map[string]any{
				"title":   r.Title,
				"url":     r.URL,
				"snippet": r.Snippet,
			})
		}

		return map[string]any{
			"query":         query,
			"results":       out,
			"dropped_count": dropped,
		}, int64(len(out)) + 1, nil
	}
}

// braveEngine calls the Brave Web Search API.
type braveEngine struct {
	client  *http.Client
	baseURL string
}

func newBraveEngine(client *http.Client, baseURL string) searchEngine {
	if baseURL == "" {
		baseURL = braveSearchURL
	}
	return &braveEngine{client: client, baseURL: baseURL}
}

type braveResponse struct {
	Web struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"results"`
	} `json:"web"`
}

func (e *braveEngine) Search(ctx context.Context, apiKey, query string, maxResults int) ([]searchResult, error) {
	u := fmt.Sprintf("%s?q=%s&count=%d", e.baseURL, url.QueryEscape(query), maxResults)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("bad request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("X-Subscription-Token", apiKey)

	resp, err := e.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("brave search status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var br braveResponse
	if err := json.Unmarshal(body, &br); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	out := make([]searchResult, 0, len(br.Web.Results))
	for _, r := range br.Web.Results {
		out = append(out, searchResult{Title: r.Title, URL: r.URL, Snippet: r.Description})
	}
	return out, nil
}

// exaEngine calls the Exa Search API.
type exaEngine struct {
	client  *http.Client
	baseURL string
}

func newExaEngine(client *http.Client, baseURL string) searchEngine {
	if baseURL == "" {
		baseURL = exaSearchURL
	}
	return &exaEngine{client: client, baseURL: baseURL}
}

type exaResponse struct {
	Results []struct {
		Title      string   `json:"title"`
		URL        string   `json:"url"`
		Text       string   `json:"text"`
		Highlights []string `json:"highlights"`
		Summary    string   `json:"summary"`
	} `json:"results"`
}

func (e *exaEngine) Search(ctx context.Context, apiKey, query string, maxResults int) ([]searchResult, error) {
	reqBody, err := json.Marshal(map[string]any{"query": query, "numResults": maxResults})
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("bad request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("x-api-key", apiKey)

	resp, err := e.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("exa search status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var er exaResponse
	if err := json.Unmarshal(body, &er); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	out := make([]searchResult, 0, len(er.Results))
	for _, r := range er.Results {
		// Snippet fallback chain: first
		// non-empty of text / first highlights entry / summary, else "".
		snippet := r.Text
		if snippet == "" && len(r.Highlights) > 0 {
			snippet = r.Highlights[0]
		}
		if snippet == "" {
			snippet = r.Summary
		}
		out = append(out, searchResult{Title: r.Title, URL: r.URL, Snippet: snippet})
	}
	return out, nil
}

// tavilyEngine calls the Tavily Search API.
type tavilyEngine struct {
	client  *http.Client
	baseURL string
}

func newTavilyEngine(client *http.Client, baseURL string) searchEngine {
	if baseURL == "" {
		baseURL = tavilySearchURL
	}
	return &tavilyEngine{client: client, baseURL: baseURL}
}

type tavilyResponse struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

func (e *tavilyEngine) Search(ctx context.Context, apiKey, query string, maxResults int) ([]searchResult, error) {
	reqBody, err := json.Marshal(map[string]any{"query": query, "max_results": maxResults})
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("bad request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := e.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tavily search status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tr tavilyResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	out := make([]searchResult, 0, len(tr.Results))
	for _, r := range tr.Results {
		out = append(out, searchResult{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return out, nil
}
