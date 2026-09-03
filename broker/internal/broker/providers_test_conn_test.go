package broker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/db"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// azureProvider returns a valid classic-route Azure provider for tests.
func azureProvider() *brokerv1.LlmProvider {
	return &brokerv1.LlmProvider{
		Id:         "azure-foundry",
		Name:       "Azure Foundry",
		Endpoint:   "https://my-resource.openai.azure.com",
		Api:        "azure-openai",
		ApiVersion: "2024-08-01-preview",
		Enabled:    true,
		Models: []*brokerv1.LlmModel{
			{Id: "gpt-4o-deployment", PriceIn: 0.000005, PriceOut: 0.000015},
		},
	}
}

// ── buildProviderProbe: per-dialect URL + auth header shape ────────────────────

func TestBuildProviderProbe_Azure(t *testing.T) {
	spec, err := buildProviderProbe(azureProvider(), "sk-azure", modeChat, "gpt-4o-deployment")
	if err != nil {
		t.Fatalf("buildProviderProbe: %v", err)
	}
	want := "https://my-resource.openai.azure.com/openai/deployments/gpt-4o-deployment/chat/completions?api-version=2024-08-01-preview"
	if spec.url != want {
		t.Errorf("azure url:\n  got  %s\n  want %s", spec.url, want)
	}
	if spec.headers["api-key"] != "sk-azure" {
		t.Errorf("azure must use api-key header, got %+v", spec.headers)
	}
	if _, ok := spec.headers["authorization"]; ok {
		t.Errorf("azure must NOT send Authorization/Bearer, got %+v", spec.headers)
	}
	if !strings.Contains(string(spec.body), "gpt-4o-deployment") {
		t.Errorf("body should reference the deployment model, got %s", spec.body)
	}
}

func TestBuildProviderProbe_OpenAI(t *testing.T) {
	spec, err := buildProviderProbe(validProvider(), "sk-or", modeChat, "anthropic/claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("buildProviderProbe: %v", err)
	}
	if !strings.HasSuffix(spec.url, "/chat/completions") {
		t.Errorf("openai url should end with /chat/completions, got %s", spec.url)
	}
	if spec.headers["authorization"] != "Bearer sk-or" {
		t.Errorf("openai must use Bearer auth, got %+v", spec.headers)
	}
	if _, ok := spec.headers["api-key"]; ok {
		t.Errorf("openai must NOT send api-key header, got %+v", spec.headers)
	}
}

// ── buildProviderProbe: family × mode routing ─────────────────────────────────

// TestBuildProviderProbe_FamilyModeMatrix pins the route, verb and body per
// family × mode. The three OpenAI-wire families share every route, so the matrix
// covers one of them plus azure's deployment prefix; anthropic is verified
// separately because its non-chat answer is a refusal, not a request.
func TestBuildProviderProbe_FamilyModeMatrix(t *testing.T) {
	compat := func(api string) *brokerv1.LlmProvider {
		p := validProvider()
		p.Api = api
		return p
	}
	for _, c := range []struct {
		name       string
		provider   *brokerv1.LlmProvider
		mode       string
		wantMethod string
		wantURL    string
		wantBody   []string
		wantAuth   bool // true = auth-only reachability probe
	}{
		{"openai chat", compat(apiOpenAICompletions), modeChat, http.MethodPost,
			"https://openrouter.ai/api/v1/chat/completions", []string{`"messages"`}, false},
		{"gemini chat rides the openai wire", compat(apiGoogleGemini), modeChat, http.MethodPost,
			"https://openrouter.ai/api/v1/chat/completions", []string{`"messages"`}, false},
		{"bedrock chat rides the openai wire", compat(apiAWSBedrock), modeChat, http.MethodPost,
			"https://openrouter.ai/api/v1/chat/completions", []string{`"messages"`}, false},
		{"openai embedding", compat(apiOpenAICompletions), modeEmbedding, http.MethodPost,
			"https://openrouter.ai/api/v1/embeddings", []string{`"input":"ping"`}, false},
		{"openai audio_speech", compat(apiOpenAICompletions), modeAudioSpeech, http.MethodPost,
			"https://openrouter.ai/api/v1/audio/speech", []string{`"voice":"alloy"`, `"input":"p"`}, false},
		{"openai image_generation", compat(apiOpenAICompletions), modeImageGeneration, http.MethodPost,
			"https://openrouter.ai/api/v1/images/generations", []string{`"prompt":"ping"`, `"n":1`}, false},
		{"openai audio_transcription is auth-only", compat(apiOpenAICompletions), modeAudioTranscription,
			http.MethodGet, "https://openrouter.ai/api/v1/models", nil, true},
		{"openai rerank is auth-only", compat(apiOpenAICompletions), modeRerank,
			http.MethodGet, "https://openrouter.ai/api/v1/models", nil, true},
		{"openai ocr is auth-only", compat(apiOpenAICompletions), modeOCR,
			http.MethodGet, "https://openrouter.ai/api/v1/models", nil, true},
		{"openai moderation is auth-only", compat(apiOpenAICompletions), modeModeration,
			http.MethodGet, "https://openrouter.ai/api/v1/models", nil, true},
		{"azure embedding uses the deployment route", azureProvider(), modeEmbedding, http.MethodPost,
			"https://my-resource.openai.azure.com/openai/deployments/m/embeddings?api-version=2024-08-01-preview",
			[]string{`"input":"ping"`}, false},
		{"azure audio_speech uses the deployment route", azureProvider(), modeAudioSpeech, http.MethodPost,
			"https://my-resource.openai.azure.com/openai/deployments/m/audio/speech?api-version=2024-08-01-preview",
			[]string{`"voice":"alloy"`}, false},
		{"azure image_generation uses the deployment route", azureProvider(), modeImageGeneration, http.MethodPost,
			"https://my-resource.openai.azure.com/openai/deployments/m/images/generations?api-version=2024-08-01-preview",
			[]string{`"prompt":"ping"`}, false},
		{"azure ocr is an auth-only deployments list", azureProvider(), modeOCR, http.MethodGet,
			"https://my-resource.openai.azure.com/openai/deployments?api-version=2024-08-01-preview", nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			spec, err := buildProviderProbe(c.provider, "sk-test", c.mode, "m")
			if err != nil {
				t.Fatalf("buildProviderProbe: %v", err)
			}
			if spec.method != c.wantMethod {
				t.Errorf("method: got %s, want %s", spec.method, c.wantMethod)
			}
			if spec.url != c.wantURL {
				t.Errorf("url:\n  got  %s\n  want %s", spec.url, c.wantURL)
			}
			if spec.authOnly != c.wantAuth {
				t.Errorf("authOnly: got %v, want %v", spec.authOnly, c.wantAuth)
			}
			if c.wantAuth && len(spec.body) != 0 {
				t.Errorf("an auth-only GET probe must carry no body, got %s", spec.body)
			}
			for _, want := range c.wantBody {
				if !strings.Contains(string(spec.body), want) {
					t.Errorf("body missing %s, got %s", want, spec.body)
				}
			}
		})
	}
}

// Anthropic has one route (/v1/messages) and no embeddings/TTS/image/list
// endpoint at all, so every non-chat mode must be refused before any request
// goes out — a 404 from a guessed URL would read as "endpoint misconfigured".
func TestBuildProviderProbe_AnthropicRejectsNonChatModes(t *testing.T) {
	p := validProvider()
	p.Api = apiAnthropicMessages

	if _, err := buildProviderProbe(p, "sk-ant", modeChat, "m"); err != nil {
		t.Fatalf("anthropic chat must still build a probe: %v", err)
	}
	for _, mode := range []string{modeEmbedding, modeAudioSpeech, modeImageGeneration,
		modeAudioTranscription, modeRerank, modeOCR, modeModeration} {
		_, err := buildProviderProbe(p, "sk-ant", mode, "m")
		if err == nil {
			t.Fatalf("mode %q must be refused for anthropic-messages", mode)
		}
		want := "mode \"" + mode + "\" is not supported by the anthropic-messages family"
		if err.Error() != want {
			t.Errorf("mode %q error:\n  got  %s\n  want %s", mode, err, want)
		}
	}
}

// ── resolveProbeMode ──────────────────────────────────────────────────────────

func TestResolveProbeMode(t *testing.T) {
	mixed := func() *brokerv1.LlmProvider {
		p := validProvider()
		p.Models = []*brokerv1.LlmModel{
			{Id: "embed-1", Mode: modeEmbedding},
			{Id: "chat-1", Mode: modeChat},
		}
		return p
	}
	for _, c := range []struct {
		name      string
		provider  *brokerv1.LlmProvider
		reqMode   string
		wantMode  string
		wantModel string
	}{
		// An absent mode on a legacy row means chat, and the first model serves it.
		{"empty request mode falls back to the first model's mode", validProvider(), "",
			modeChat, "anthropic/claude-sonnet-4-6"},
		{"derived mode follows the first model", mixed(), "", modeEmbedding, "embed-1"},
		{"explicit mode picks its own model", mixed(), modeChat, modeChat, "chat-1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			mode, model, err := resolveProbeMode(c.provider, c.reqMode)
			if err != nil {
				t.Fatalf("resolveProbeMode: %v", err)
			}
			if mode != c.wantMode || model != c.wantModel {
				t.Errorf("got (%q, %q), want (%q, %q)", mode, model, c.wantMode, c.wantModel)
			}
		})
	}
}

func TestResolveProbeMode_UnservedModeRejected(t *testing.T) {
	_, _, err := resolveProbeMode(validProvider(), modeEmbedding)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("mode no model declares: want InvalidArgument, got %v", err)
	}
}

// ── azure-openai upsert validation ────────────────────────────────────────────

func TestUpsertLlmProvider_AzureRequiresApiVersion(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	svc := NewBrokerService(testProviderDeps(t, srv.URL, nil))

	p := azureProvider()
	p.ApiVersion = ""
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.UpsertLlmProvider(ctx, &brokerv1.UpsertLlmProviderRequest{Provider: p})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("azure without api_version: want InvalidArgument, got %v", err)
	}
}

func TestUpsertLlmProvider_AzureAccepted(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	sec := newCapturingSecrets()
	deps := testProviderDeps(t, srv.URL, sec)
	deps.Providers = newFakeLlmProviderStore()
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.UpsertLlmProvider(ctx, &brokerv1.UpsertLlmProviderRequest{
		Provider: azureProvider(),
		ApiKey:   "sk-azure",
	})
	if err != nil {
		t.Fatalf("azure upsert: %v", err)
	}
	store := deps.Providers.(*fakeLlmProviderStore)
	if len(store.upserts) != 1 {
		t.Fatalf("want 1 upsert, got %d", len(store.upserts))
	}
	if got := store.upserts[0].APIVersion; got != "2024-08-01-preview" {
		t.Errorf("api_version round-trip: got %q, want 2024-08-01-preview", got)
	}
	if got := store.upserts[0].API; got != "azure-openai" {
		t.Errorf("api: got %q, want azure-openai", got)
	}
}

// ── TestLlmProvider RPC: probe outcomes ───────────────────────────────────────

// storedKeySecrets returns a fixed stored key for ReadProviderKey.
type storedKeySecrets struct {
	capturingSecrets
	stored string
}

func (s *storedKeySecrets) ReadProviderKey(_ context.Context, _, _ string) (string, bool, error) {
	return s.stored, s.stored != "", nil
}

func TestTestLlmProvider_OK(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("authorization") != "Bearer sk-or" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	deps := testProviderDeps(t, srv.URL, nil)
	deps.AllowProviderTestPrivate = true // upstream is loopback (httptest)
	svc := NewBrokerService(deps)

	p := validProvider()
	p.Endpoint = upstream.URL
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.TestLlmProvider(ctx, &brokerv1.TestLlmProviderRequest{Provider: p, ApiKey: "sk-or"})
	if err != nil {
		t.Fatalf("TestLlmProvider: %v", err)
	}
	if !resp.Ok {
		t.Errorf("want ok, got %+v", resp)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d, want 200", resp.StatusCode)
	}
}

func TestTestLlmProvider_UpstreamRejected(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer upstream.Close()

	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	deps := testProviderDeps(t, srv.URL, nil)
	deps.AllowProviderTestPrivate = true
	svc := NewBrokerService(deps)

	p := validProvider()
	p.Endpoint = upstream.URL
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.TestLlmProvider(ctx, &brokerv1.TestLlmProviderRequest{Provider: p, ApiKey: "wrong"})
	if err != nil {
		t.Fatalf("TestLlmProvider: %v", err)
	}
	if resp.Ok {
		t.Errorf("want not-ok for 401, got %+v", resp)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", resp.StatusCode)
	}
}

func TestTestLlmProvider_StoredKeyFallback(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	sec := &storedKeySecrets{stored: "sk-stored"}
	deps := testProviderDeps(t, srv.URL, sec)
	deps.AllowProviderTestPrivate = true
	svc := NewBrokerService(deps)

	p := validProvider()
	p.Endpoint = upstream.URL
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	// Blank ApiKey → handler must fall back to the stored Vault key.
	resp, err := svc.TestLlmProvider(ctx, &brokerv1.TestLlmProviderRequest{Provider: p, ApiKey: ""})
	if err != nil {
		t.Fatalf("TestLlmProvider: %v", err)
	}
	if !resp.Ok {
		t.Errorf("want ok using stored key, got %+v", resp)
	}
	if gotAuth != "Bearer sk-stored" {
		t.Errorf("upstream should see stored key, got auth %q", gotAuth)
	}
}

// ── TestLlmProvider metering + budget gate ────────────────────────────────────

// probeSvc wires a north service whose provider-test probe hits upstream, with
// spend caps and counters attached so the budget gate and usage accounting are
// both live.
func probeSvc(t *testing.T, upstreamURL string, caps *fakeSpendCapStore, counters *fakeSpendCounterStore) (*BrokerService, *brokerv1.LlmProvider) {
	t.Helper()
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	t.Cleanup(srv.Close)

	deps := testProviderDeps(t, srv.URL, nil)
	deps.AllowProviderTestPrivate = true // upstream is loopback (httptest)
	deps.SpendCaps = caps
	deps.SpendCounters = counters
	deps.Providers = newFakeLlmProviderStore()

	p := validProvider()
	p.Endpoint = upstreamURL
	return NewBrokerService(deps), p
}

// The probe spends real provider money and an admin can drive it in a loop, so
// a breached spend cap must stop it BEFORE the upstream request goes out. The
// RPC contract is preserved: ok=false with a reason, never a gRPC error.
func TestTestLlmProvider_DeniedByBreachedSpendCap(t *testing.T) {
	var upstreamHits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	counters := newFakeSpendCounterStore()
	counters.orgTotal = 1_000_000
	caps := newFakeSpendCapStore()
	_, _ = caps.Upsert(context.Background(), testTenantUUID, db.SpendCap{
		Scope: db.SpendCapScopeOrg, CapMicros: 1_000_000,
	})
	svc, p := probeSvc(t, upstream.URL, caps, counters)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.TestLlmProvider(ctx, &brokerv1.TestLlmProviderRequest{Provider: p, ApiKey: "sk-or"})
	if err != nil {
		t.Fatalf("a denial must stay a 200-class ok=false response, got gRPC error %v", err)
	}
	if resp.Ok {
		t.Fatal("want ok=false when the org spend cap is breached")
	}
	if !strings.Contains(resp.Error, "spend_org") {
		t.Errorf("denial must name the limit that stopped it, got %q", resp.Error)
	}
	if upstreamHits != 0 {
		t.Errorf("upstream must not be contacted after a budget denial, got %d hits", upstreamHits)
	}
}

// A completed probe is billable, so it must land in spend accounting whether the
// upstream accepted it or rejected it — the request was sent either way.
func TestTestLlmProvider_EmitsUsage(t *testing.T) {
	for _, c := range []struct {
		name       string
		statusCode int
	}{
		{"successful probe", http.StatusOK},
		{"upstream-rejected probe still billed", http.StatusUnauthorized},
	} {
		t.Run(c.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.statusCode)
			}))
			defer upstream.Close()

			counters := newFakeSpendCounterStore()
			svc, p := probeSvc(t, upstream.URL, newFakeSpendCapStore(), counters)

			ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
			if _, err := svc.TestLlmProvider(ctx, &brokerv1.TestLlmProviderRequest{Provider: p, ApiKey: "sk-or"}); err != nil {
				t.Fatalf("TestLlmProvider: %v", err)
			}

			counters.mu.Lock()
			calls := append([]fakeAccumulateCall(nil), counters.accumulateCalls...)
			counters.mu.Unlock()
			if len(calls) != 1 {
				t.Fatalf("probe must record exactly one usage accumulation, got %d", len(calls))
			}
			if calls[0].tokensOut != 1 {
				t.Errorf("probe asks upstream for 1 token, so tokensOut = %d, want 1", calls[0].tokensOut)
			}
			if calls[0].userID != "admin@example.com" {
				t.Errorf("probe usage must be attributed to the calling admin, got %q", calls[0].userID)
			}
			if calls[0].agentID != "" {
				t.Errorf("probe has no agent, got agentID %q", calls[0].agentID)
			}
		})
	}
}

// An auth-only mode must reach the upstream as a bodiless GET on the
// models-list route and still report ok=true on a 2xx.
func TestTestLlmProvider_AuthOnlyModeSendsGet(t *testing.T) {
	var gotMethod, gotPath string
	var gotLen int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotLen = r.Method, r.URL.Path, r.ContentLength
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()

	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	deps := testProviderDeps(t, srv.URL, nil)
	deps.AllowProviderTestPrivate = true
	svc := NewBrokerService(deps)

	p := validProvider()
	p.Endpoint = upstream.URL
	p.Models = []*brokerv1.LlmModel{{Id: "mistral-ocr", Mode: modeOCR}}

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.TestLlmProvider(ctx, &brokerv1.TestLlmProviderRequest{
		Provider: p, ApiKey: "sk-or", Mode: modeOCR,
	})
	if err != nil {
		t.Fatalf("TestLlmProvider: %v", err)
	}
	if !resp.Ok {
		t.Errorf("auth-only probe on a 200: want ok, got %+v", resp)
	}
	if resp.Error != "" {
		t.Errorf("a successful probe must carry no error text, got %q", resp.Error)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method: got %s, want GET", gotMethod)
	}
	if gotPath != "/models" {
		t.Errorf("path: got %s, want /models", gotPath)
	}
	if gotLen > 0 {
		t.Errorf("GET probe must send no body, got Content-Length %d", gotLen)
	}
}

// A mode the provider declares no model for is the admin's mistake, so it must
// be a gRPC InvalidArgument — not an ok=false probe result implying the endpoint
// was contacted and refused.
func TestTestLlmProvider_UnservedModeIsInvalidArgument(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	deps := testProviderDeps(t, srv.URL, nil)
	deps.AllowProviderTestPrivate = true
	svc := NewBrokerService(deps)

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	_, err := svc.TestLlmProvider(ctx, &brokerv1.TestLlmProviderRequest{
		Provider: validProvider(), ApiKey: "sk-or", Mode: modeEmbedding,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unserved mode: want InvalidArgument, got %v", err)
	}
}

// Anthropic × a non-chat mode must be an ok=false answer with no request sent —
// the family has no such route to probe.
func TestTestLlmProvider_AnthropicNonChatModeNotSupported(t *testing.T) {
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	deps := testProviderDeps(t, srv.URL, nil)
	deps.AllowProviderTestPrivate = true
	svc := NewBrokerService(deps)

	p := validProvider()
	p.Api = apiAnthropicMessages
	p.Endpoint = upstream.URL
	p.Models = []*brokerv1.LlmModel{{Id: "voyage-3", Mode: modeEmbedding}}

	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.TestLlmProvider(ctx, &brokerv1.TestLlmProviderRequest{Provider: p, ApiKey: "sk-ant"})
	if err != nil {
		t.Fatalf("an unsupported family/mode pair must stay an ok=false response, got %v", err)
	}
	if resp.Ok {
		t.Fatalf("want ok=false, got %+v", resp)
	}
	if resp.Error != `mode "embedding" is not supported by the anthropic-messages family` {
		t.Errorf("error text: got %q", resp.Error)
	}
	if hits != 0 {
		t.Errorf("no request may go out for an unsupported mode, got %d hits", hits)
	}
}

func TestTestLlmProvider_SSRFRejected(t *testing.T) {
	f := &fakeFGA{admins: map[string]bool{"user:admin@example.com": true}}
	srv := f.server(t)
	defer srv.Close()
	deps := testProviderDeps(t, srv.URL, nil)
	deps.AllowProviderTestPrivate = false // enforce the SSRF guard
	svc := NewBrokerService(deps)

	p := validProvider()
	p.Endpoint = "http://169.254.169.254/latest/meta-data" // cloud metadata
	ctx := ctxWithIdentity(testTenantUUID, "admin@example.com")
	resp, err := svc.TestLlmProvider(ctx, &brokerv1.TestLlmProviderRequest{Provider: p, ApiKey: "sk"})
	if err != nil {
		t.Fatalf("TestLlmProvider: %v", err)
	}
	if resp.Ok {
		t.Errorf("SSRF target must be rejected, got %+v", resp)
	}
}
