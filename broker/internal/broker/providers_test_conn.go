package broker

// providers_test_conn.go — north-bound TestLlmProvider RPC.
// Probes a provider's endpoint+key with ONE minimal chat request before the
// admin saves it, so misconfigured endpoints / wrong keys / bad api-version are
// caught in the UI instead of at first model turn. The probe runs server-side
// (the api-key never reaches the browser) through an SSRF-guarded HTTP client.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/toolproxy"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

const providerTestTimeout = 12 * time.Second

// TestLlmProvider sends one minimal request to the provider to verify the
// endpoint, api-version and key are usable. It never writes to the DB or Vault.
func (s *BrokerService) TestLlmProvider(ctx context.Context, req *brokerv1.TestLlmProviderRequest) (*brokerv1.TestLlmProviderResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.admin.llm.test")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, "", "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}

	p := req.GetProvider()
	if p == nil {
		return nil, status.Error(codes.InvalidArgument, "provider required")
	}
	if err := validateProvider(p); err != nil {
		return nil, err
	}

	// Resolve the key: prefer the (unsaved) key from the request; otherwise fall
	// back to the stored Vault key for an existing provider (blank-on-edit path).
	key := req.ApiKey
	if key == "" && s.deps.Secrets != nil {
		stored, ok, kerr := s.deps.Secrets.ReadProviderKey(ctx, tenant, p.Id)
		if kerr == nil && ok {
			key = stored
		}
	}
	if key == "" {
		return &brokerv1.TestLlmProviderResponse{
			Ok:    false,
			Error: "no api key supplied and none stored for this provider",
		}, nil
	}

	// Which modality to probe, and which of the provider's models can serve it.
	// An explicit mode with no matching model is a caller error, not a probe
	// failure — the admin picked a mode this provider does not declare.
	mode, model, merr := resolveProbeMode(p, req.Mode)
	if merr != nil {
		return nil, merr
	}

	spec, perr := buildProviderProbe(p, key, mode, model)
	if perr != nil {
		return &brokerv1.TestLlmProviderResponse{Ok: false, Error: perr.Error()}, nil
	}

	allowPrivate := s.deps.AllowProviderTestPrivate
	if err := toolproxy.ValidatePublicURL(spec.url, allowPrivate); err != nil {
		return &brokerv1.TestLlmProviderResponse{Ok: false, Error: err.Error()}, nil
	}

	traceID := span.SpanContext().TraceID().String()

	// The probe spends real provider money and an admin can drive it in a loop,
	// so it passes the same rate-limit + spend-cap gate as any model turn. No
	// agent id — this is an admin acting as themselves.
	if allowed, limitType := s.deps.checkLlmBudget(ctx, tenant, user, "", p.Id); !allowed {
		go s.deps.emitCheckRateLimitAudit(context.Background(), tenant, "", p.Id, limitType)
		s.emitLlmProviderAudit(ctx, traceID, tenant, user, "llm.provider.test", p.Id)
		return &brokerv1.TestLlmProviderResponse{
			Ok:    false,
			Error: "blocked by " + limitType + " limit — the connection test was not sent",
		}, nil
	}

	resp, code, latency := s.runProviderProbe(ctx, spec, allowPrivate)

	// Book the probe as usage even when the upstream rejected it: the request was
	// sent, so the provider may well bill for it. One output token is the cap the
	// probe body asks for; input tokens are unknown and left at 0.
	s.deps.recordLlmUsage(ctx, &brokerv1.EmitLlmUsageRequest{
		TenantId: tenant, UserId: user, Provider: p.Id, Model: spec.model, TokensOut: 1,
		Source: "test",
	})

	s.emitLlmProviderAudit(ctx, traceID, tenant, user, "llm.provider.test", p.Id)
	resp.LatencyMs = latency
	resp.StatusCode = int32(code)
	return resp, nil
}

// runProviderProbe performs the HTTP request and maps the outcome to a response.
// A 2xx is success; any other completed status is "reachable but rejected" with
// a short snippet; a transport failure is a connection error.
func (s *BrokerService) runProviderProbe(
	ctx context.Context, spec probeSpec, allowPrivate bool,
) (*brokerv1.TestLlmProviderResponse, int, int64) {
	client := toolproxy.NewHardenedClient(providerTestTimeout, allowPrivate)

	reqCtx, cancel := context.WithTimeout(ctx, providerTestTimeout)
	defer cancel()

	// GET probes carry no body; a non-nil empty reader would still set
	// Content-Length: 0, which some gateways reject on GET.
	var rdr io.Reader
	if len(spec.body) > 0 {
		rdr = bytes.NewReader(spec.body)
	}
	httpReq, err := http.NewRequestWithContext(reqCtx, spec.method, spec.url, rdr)
	if err != nil {
		return &brokerv1.TestLlmProviderResponse{Ok: false, Error: "build request: " + err.Error()}, 0, 0
	}
	for k, v := range spec.headers {
		httpReq.Header.Set(k, v)
	}

	start := time.Now()
	httpResp, err := client.Do(httpReq)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		s.deps.Logger.Debug("TestLlmProvider: request failed", zap.Error(err))
		return &brokerv1.TestLlmProviderResponse{Ok: false, Error: "connection failed: " + err.Error()}, 0, latency
	}
	defer httpResp.Body.Close()

	snippet, _ := io.ReadAll(io.LimitReader(httpResp.Body, 512))
	if httpResp.StatusCode >= 200 && httpResp.StatusCode < 300 {
		return &brokerv1.TestLlmProviderResponse{Ok: true}, httpResp.StatusCode, latency
	}
	// snippet is raw upstream body; gRPC status strings must be valid UTF-8,
	// so drop any invalid bytes before embedding it in the error.
	msg := strings.ToValidUTF8(strings.TrimSpace(string(snippet)), "")
	if msg == "" {
		msg = http.StatusText(httpResp.StatusCode)
	}
	return &brokerv1.TestLlmProviderResponse{
		Ok:    false,
		Error: fmt.Sprintf("upstream returned %d: %s", httpResp.StatusCode, msg),
	}, httpResp.StatusCode, latency
}

// probeSpec is one mode-aware probe request: everything runProviderProbe needs
// plus the model it was built for (booked as usage).
type probeSpec struct {
	method  string
	url     string
	headers map[string]string
	body    []byte
	model   string
	// authOnly marks a reachability probe (a models/deployments list GET) used
	// for modes with no cheap single-unit call. A 2xx proves endpoint + key
	// only, NOT that the mode's own route works. The response has no field to
	// carry that nuance, so it stays a code-level fact: ok=true, error="".
	authOnly bool
}

// resolveProbeMode picks the modality to probe and the model that serves it.
// An explicit request mode wins; otherwise the first model's own mode is what
// the admin most likely means. A derived mode always matches its own model, so
// the not-found path only fires for an explicit mode the provider cannot serve.
func resolveProbeMode(p *brokerv1.LlmProvider, reqMode string) (string, string, error) {
	if len(p.Models) == 0 {
		return "", "", status.Error(codes.InvalidArgument, "provider has no models to probe")
	}
	mode := reqMode
	if mode == "" {
		mode = effectiveMode(p.Models[0].Mode)
	}
	for _, m := range p.Models {
		if effectiveMode(m.Mode) == mode {
			return mode, m.Id, nil
		}
	}
	return "", "", status.Errorf(codes.InvalidArgument, "provider %q has no model with mode %q", p.Id, mode)
}

// buildProviderProbe builds one probe request per family × mode. The three
// families that speak the OpenAI wire (openai-completions, google-gemini,
// aws-bedrock) share every route; azure prefixes the deployment route; anthropic
// has only a chat route at all, so every other mode is rejected without touching
// the network.
func buildProviderProbe(p *brokerv1.LlmProvider, key, mode, model string) (probeSpec, error) {
	base := strings.TrimRight(p.Endpoint, "/")

	switch p.Api {
	case apiAnthropicMessages:
		if mode != modeChat {
			return probeSpec{}, fmt.Errorf("mode %q is not supported by the %s family", mode, apiAnthropicMessages)
		}
		body, _ := json.Marshal(map[string]any{
			"model":      model,
			"max_tokens": 1,
			"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		})
		return probeSpec{
			method: http.MethodPost,
			url:    base + "/v1/messages",
			headers: map[string]string{
				"content-type":      "application/json",
				"x-api-key":         key,
				"anthropic-version": "2023-06-01",
			},
			body:  body,
			model: model,
		}, nil

	case apiAzureOpenAI:
		// Classic Azure deployment route: model id is the deployment name.
		headers := map[string]string{"content-type": "application/json", "api-key": key}
		route := func(suffix string) string {
			return fmt.Sprintf("%s/openai/deployments/%s/%s?api-version=%s", base, model, suffix, p.ApiVersion)
		}
		spec := probeSpec{method: http.MethodPost, headers: headers, model: model}
		switch mode {
		case modeChat:
			spec.url, spec.body = route("chat/completions"), openAIPingBody(model)
		case modeEmbedding:
			spec.url, spec.body = route("embeddings"), embeddingPingBody(model)
		case modeAudioSpeech:
			spec.url, spec.body = route("audio/speech"), speechPingBody(model)
		case modeImageGeneration:
			spec.url, spec.body = route("images/generations"), imagePingBody(model)
		default:
			spec.method, spec.authOnly = http.MethodGet, true
			spec.url = base + "/openai/deployments?api-version=" + p.ApiVersion
		}
		return spec, nil

	default: // openai-completions, google-gemini, aws-bedrock — OpenAI wire
		headers := map[string]string{"content-type": "application/json", "authorization": "Bearer " + key}
		spec := probeSpec{method: http.MethodPost, headers: headers, model: model}
		switch mode {
		case modeChat:
			spec.url, spec.body = base+"/chat/completions", openAIPingBody(model)
		case modeEmbedding:
			spec.url, spec.body = base+"/embeddings", embeddingPingBody(model)
		case modeAudioSpeech:
			spec.url, spec.body = base+"/audio/speech", speechPingBody(model)
		case modeImageGeneration:
			spec.url, spec.body = base+"/images/generations", imagePingBody(model)
		default:
			spec.method, spec.authOnly = http.MethodGet, true
			spec.url = base + "/models"
		}
		return spec, nil
	}
}

// openAIPingBody is a minimal, cheap chat-completions request (single token).
// max_completion_tokens: newer OpenAI-dialect models reject max_tokens.
func openAIPingBody(model string) []byte {
	body, _ := json.Marshal(map[string]any{
		"model":                 model,
		"max_completion_tokens": 1,
		"messages":              []map[string]string{{"role": "user", "content": "ping"}},
	})
	return body
}

// The three bodies below are each the smallest billable unit their route
// accepts: one short input, one voice, one image.
func embeddingPingBody(model string) []byte {
	body, _ := json.Marshal(map[string]any{"model": model, "input": "ping"})
	return body
}

func speechPingBody(model string) []byte {
	body, _ := json.Marshal(map[string]any{"model": model, "input": "p", "voice": "alloy"})
	return body
}

func imagePingBody(model string) []byte {
	body, _ := json.Marshal(map[string]any{"model": model, "prompt": "ping", "n": 1})
	return body
}
