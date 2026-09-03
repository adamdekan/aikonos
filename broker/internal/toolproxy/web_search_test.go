// broker/internal/toolproxy/web_search_test.go
package toolproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
)

// fakeWebSearchSettings is a stub WebSearchSettings resolver.
type fakeWebSearchSettings struct {
	engine     string
	maxResults int
	configured bool
	err        error
}

func (f fakeWebSearchSettings) Resolve(_ context.Context, _ string) (string, int, bool, error) {
	return f.engine, f.maxResults, f.configured, f.err
}

// fakeWebSearchKeys is a stub webSearchKeyReader.
type fakeWebSearchKeys struct {
	key string
	ok  bool
	err error
}

func (f fakeWebSearchKeys) ReadWebSearchApp(_ context.Context, _ string) (string, bool, error) {
	return f.key, f.ok, f.err
}

// searchResults returns out["results"] as a slice of result maps, after proving
// the whole output survives ResultStruct.
//
// The ResultStruct call is the point of this helper. Asserting the Go shape alone
// is what let a regression ship: `results` was built as []map[string]any, which
// reads fine from a test but is a type structpb will not walk, so every real
// web.search call lost its results on the way back to the model while these
// tests stayed green. Any future change to the result shape has to keep it
// encodable, not merely well-typed in Go.
func searchResults(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	if _, err := ResultStruct(out); err != nil {
		t.Fatalf("web.search output is not proto-encodable, so the model would receive nothing: %v\noutput: %#v", err, out)
	}
	raw, ok := out["results"].([]any)
	if !ok {
		t.Fatalf("results field wrong type/shape: %#v", out["results"])
	}
	results := make([]map[string]any, 0, len(raw))
	for i, item := range raw {
		m, mok := item.(map[string]any)
		if !mok {
			t.Fatalf("results[%d] is not a map: %#v", i, item)
		}
		results = append(results, m)
	}
	return results
}

func TestWebSearchPlugin_RegistersWithAuthoritativeEffectClass(t *testing.T) {
	reg := toolregistry.NewRegistry()
	if err := reg.LoadManifests("../../../skills"); err != nil {
		t.Fatalf("LoadManifests: %v", err)
	}
	reg.LoadFromPlugins(RegisteredPluginBaselines())

	found := false
	for _, id := range RegisteredToolIDs() {
		if id == "web.search" {
			found = true
		}
	}
	if !found {
		t.Fatal("web.search plugin did not self-register")
	}

	ec, ok := reg.AuthoritativeEffectClass("web.search")
	if !ok {
		t.Fatal("web.search has no authoritative effect class")
	}
	if ec.String() != "READ_ONLY" {
		t.Errorf("web.search effect class = %v, want READ_ONLY", ec)
	}

	scope, ok := reg.RequiredScope("web.search")
	if !ok || scope != "web:search" {
		t.Errorf("web.search scope = %q, ok=%v, want %q", scope, ok, "web:search")
	}
}

func TestWebSearchHandler_UnconfiguredTenant(t *testing.T) {
	h := newWebSearchHandler(WebSearchConfig{Settings: fakeWebSearchSettings{configured: false}}, fakeWebSearchKeys{ok: true, key: "k"})

	_, _, err := h(context.Background(), Request{Args: map[string]any{"query": "aikonos"}, TenantID: "t1"})
	if err == nil {
		t.Fatal("expected an in-band not-configured error")
	}
	if err != errWebSearchNotConfigured {
		t.Errorf("error = %v, want %v", err, errWebSearchNotConfigured)
	}
}

func TestWebSearchHandler_NilSettingsIsUnconfigured(t *testing.T) {
	h := newWebSearchHandler(WebSearchConfig{}, fakeWebSearchKeys{ok: true, key: "k"})

	_, _, err := h(context.Background(), Request{Args: map[string]any{"query": "aikonos"}, TenantID: "t1"})
	if err != errWebSearchNotConfigured {
		t.Errorf("error = %v, want %v", err, errWebSearchNotConfigured)
	}
}

func TestWebSearchHandler_UnknownEngineFailsClosed(t *testing.T) {
	h := newWebSearchHandler(WebSearchConfig{
		Settings: fakeWebSearchSettings{engine: "bing", maxResults: 5, configured: true},
	}, fakeWebSearchKeys{ok: true, key: "k"})

	_, _, err := h(context.Background(), Request{Args: map[string]any{"query": "aikonos"}, TenantID: "t1"})
	if err == nil {
		t.Fatal("expected unknown-engine to fail closed")
	}
}

func TestWebSearchHandler_ConfiguredReturnsRankedResults(t *testing.T) {
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Subscription-Token") != "brave-key" {
			t.Errorf("missing/wrong subscription token header: %q", r.Header.Get("X-Subscription-Token"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"web": map[string]any{
				"results": []map[string]any{
					{"title": "Allowed Result", "url": "https://allowed.example/a", "description": "an allowed snippet"},
					{"title": "Denied Result", "url": "https://denied.example/b", "description": "a denied snippet"},
				},
			},
		})
	})
	defer srv.Close()

	h := newWebSearchHandler(WebSearchConfig{
		Settings:     fakeWebSearchSettings{engine: "brave", maxResults: 5, configured: true},
		BraveBaseURL: srv.URL,
		ACL:          fakeACL{deny: map[string]bool{"denied.example": true}},
	}, fakeWebSearchKeys{ok: true, key: "brave-key"})

	out, _, err := h(context.Background(), Request{Args: map[string]any{"query": "aikonos"}, TenantID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	results := searchResults(t, out)
	if len(results) != 1 {
		t.Fatalf("expected 1 surviving result after NetACL filtering, got %d: %#v", len(results), results)
	}
	if results[0]["url"] != "https://allowed.example/a" {
		t.Errorf("wrong result survived: %#v", results[0])
	}
	if results[0]["title"] != "Allowed Result" || results[0]["snippet"] != "an allowed snippet" {
		t.Errorf("result fields not mapped correctly: %#v", results[0])
	}

	dropped, ok := out["dropped_count"].(int)
	if !ok || dropped != 1 {
		t.Errorf("dropped_count = %#v, want 1", out["dropped_count"])
	}
}

func TestWebSearchHandler_MissingQueryArg(t *testing.T) {
	h := newWebSearchHandler(WebSearchConfig{Settings: fakeWebSearchSettings{engine: "brave", configured: true}}, fakeWebSearchKeys{ok: true, key: "k"})

	_, _, err := h(context.Background(), Request{Args: map[string]any{}, TenantID: "t1"})
	if err == nil {
		t.Fatal("expected missing-query error")
	}
}

// ── Exa engine ─────────────────────────────────

func TestWebSearchHandler_ExaConfiguredMapsRequestAndSnippetFallback(t *testing.T) {
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.Header.Get("x-api-key") != "exa-key" {
			t.Errorf("missing/wrong x-api-key header: %q", r.Header.Get("x-api-key"))
		}
		var body map[string]any
		if derr := json.NewDecoder(r.Body).Decode(&body); derr != nil {
			t.Fatalf("decode request body: %v", derr)
		}
		if body["query"] != "aikonos" {
			t.Errorf("body.query = %v, want %q", body["query"], "aikonos")
		}
		if body["numResults"] != float64(5) {
			t.Errorf("body.numResults = %v, want 5", body["numResults"])
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"title": "Has text", "url": "https://a.example/1", "text": "text snippet", "highlights": []string{"hl snippet"}, "summary": "summary snippet"},
				{"title": "Falls back to highlights", "url": "https://a.example/2", "highlights": []string{"hl snippet"}, "summary": "summary snippet"},
				{"title": "Falls back to summary", "url": "https://a.example/3", "summary": "summary snippet"},
				{"title": "No snippet at all", "url": "https://a.example/4"},
			},
		})
	})
	defer srv.Close()

	h := newWebSearchHandler(WebSearchConfig{
		Settings:   fakeWebSearchSettings{engine: "exa", maxResults: 5, configured: true},
		ExaBaseURL: srv.URL,
	}, fakeWebSearchKeys{ok: true, key: "exa-key"})

	out, _, err := h(context.Background(), Request{Args: map[string]any{"query": "aikonos"}, TenantID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	results := searchResults(t, out)
	if len(results) != 4 {
		t.Fatalf("expected 4 results, got %d: %#v", len(results), results)
	}
	wantSnippets := []string{"text snippet", "hl snippet", "summary snippet", ""}
	for i, want := range wantSnippets {
		if results[i]["snippet"] != want {
			t.Errorf("result[%d] snippet = %q, want %q", i, results[i]["snippet"], want)
		}
	}
}

// ── Tavily engine ──────────────────────────────

func TestWebSearchHandler_TavilyConfiguredMapsRequestAndContent(t *testing.T) {
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tavily-key" {
			t.Errorf("missing/wrong Authorization header: %q", got)
		}
		var body map[string]any
		if derr := json.NewDecoder(r.Body).Decode(&body); derr != nil {
			t.Fatalf("decode request body: %v", derr)
		}
		if body["query"] != "aikonos" {
			t.Errorf("body.query = %v, want %q", body["query"], "aikonos")
		}
		if body["max_results"] != float64(5) {
			t.Errorf("body.max_results = %v, want 5", body["max_results"])
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"title": "Tavily result", "url": "https://a.example/1", "content": "tavily content"},
			},
		})
	})
	defer srv.Close()

	h := newWebSearchHandler(WebSearchConfig{
		Settings:      fakeWebSearchSettings{engine: "tavily", maxResults: 5, configured: true},
		TavilyBaseURL: srv.URL,
	}, fakeWebSearchKeys{ok: true, key: "tavily-key"})

	out, _, err := h(context.Background(), Request{Args: map[string]any{"query": "aikonos"}, TenantID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	results := searchResults(t, out)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d: %#v", len(results), results)
	}
	if results[0]["title"] != "Tavily result" || results[0]["url"] != "https://a.example/1" || results[0]["snippet"] != "tavily content" {
		t.Errorf("result not mapped correctly: %#v", results[0])
	}
}

// ── ProbeSearchEngine ( CP6: admin Test probe reuses
// the same engine impl web.search calls, no duplicated per-engine HTTP shape
// in the broker package) ─────────────────────────────────────────────────────

func withProbeURL(t *testing.T, engine, url string) {
	t.Helper()
	orig := searchEngineProbeURLs[engine]
	searchEngineProbeURLs[engine] = url
	t.Cleanup(func() { searchEngineProbeURLs[engine] = orig })
}

func TestProbeSearchEngine_UnknownEngineFailsClosed(t *testing.T) {
	if err := ProbeSearchEngine(context.Background(), "not-a-real-engine", "key", true, time.Second); err == nil {
		t.Fatal("expected an error for an unregistered engine")
	}
}

func TestProbeSearchEngine_ExaSuccessReusesEngineImpl(t *testing.T) {
	var gotAuth string
	srv := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{}})
	})
	defer srv.Close()
	withProbeURL(t, "exa", srv.URL)

	if err := ProbeSearchEngine(context.Background(), "exa", "exa-probe-key", true, 5*time.Second); err != nil {
		t.Fatalf("ProbeSearchEngine: %v", err)
	}
	if gotAuth != "exa-probe-key" {
		t.Errorf("probe did not reuse the exa engine's auth header, got %q", gotAuth)
	}
}

func TestProbeSearchEngine_TavilyUpstreamErrorSurfaces(t *testing.T) {
	srv := newTestServer(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("bad key"))
	})
	defer srv.Close()
	withProbeURL(t, "tavily", srv.URL)

	if err := ProbeSearchEngine(context.Background(), "tavily", "bad-key", true, 5*time.Second); err == nil {
		t.Fatal("expected upstream 401 to surface as an error")
	}
}
