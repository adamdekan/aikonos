// CP4: vision-provider selection + the multimodal HTTP call itself.
//
// WHY this is still fail-closed: unlike chat model selection (pickModelId in
// pi/session-plan.ts) there is no agent-level vision override. Selection now
// considers the tenant-designated fallback after the default-vision provider,
// but ONLY when that fallback is itself vision_capable — a non-vision provider
// is never reached for, and a tenant with neither surfaces a clear error to the
// caller rather than silently reusing the chat provider or best-efforting a
// non-vision model. See 
// ("Why per-tenant only, not per-agent, for v1").
//
// This module never reaches the forked Pi child — it runs only in the trusted
// parent process, which is the only place the real provider API key may exist.

import {
  resolveDialectRequest,
  extractOpenAIText,
  extractAnthropicText,
  extractOpenAIUsage,
  extractAnthropicUsage,
} from "./provider-dialect.js";
// ── callVisionProvider ───────────────────────────────────────────────────────

export interface VisionCallProvider {
  id: string;
  endpoint: string;
  api: string;
  apiKey: string;
  // apiVersion is required when api is "azure-openai".
  apiVersion?: string;
}

export interface CallVisionProviderOptions {
  provider: VisionCallProvider;
  modelId: string;
  imageBase64: string;
  mimeType: string;
  prompt?: string;
  // Abort the upstream request after this many ms so a hung provider can't
  // block the calling run indefinitely. Reuses the gateway egress timeout
  // (Config.egressTimeoutMs) — no new env knob. Omitted = no timeout.
  timeoutMs?: number;
}

const DEFAULT_PROMPT = "Describe this image.";

// VisionProviderError is thrown on a non-2xx upstream response or a transport
// failure. The message includes the provider id + status but never the key.
//
// `status` carries the upstream HTTP status when the failure WAS a response, so
// a caller can classify a retry through provider-fallback's shouldFailover. It
// is undefined for a transport failure, a timeout, or a 200 whose body could not
// be read — all of which are "this provider is not working", i.e. transport-class.
export class VisionProviderError extends Error {
  constructor(message: string, readonly status?: number) {
    super(message);
  }
}

interface ProviderRequest {
  url: string;
  headers: Record<string, string>;
  body: string;
}

// buildProviderRequest builds the per-dialect URL/headers/body. Mirrors
// broker/internal/broker/providers_test_conn.go's buildProviderProbe endpoint
// and header conventions, extended with a multimodal image content block.
// URL/headers/auth resolution is shared with reason.ts and egress-proxy.ts
// via provider-dialect.ts; only the body shape (multimodal content block vs.
// plain text) is per-call-site here.
function buildProviderRequest(opts: CallVisionProviderOptions): ProviderRequest {
  const { provider, modelId, imageBase64, mimeType } = opts;
  const prompt = opts.prompt ?? DEFAULT_PROMPT;
  const base = provider.endpoint.replace(/\/+$/, "");
  const { path, headers: authHeaders } = resolveDialectRequest({
    api: provider.api,
    apiKey: provider.apiKey,
    apiVersion: provider.apiVersion,
    modelId,
  });
  const url = `${base}${path}`;
  const headers = { "content-type": "application/json", ...authHeaders };

  if (provider.api === "anthropic-messages") {
    return {
      url,
      headers,
      body: JSON.stringify({
        model: modelId,
        max_tokens: 1024,
        messages: [
          {
            role: "user",
            content: [
              { type: "image", source: { type: "base64", media_type: mimeType, data: imageBase64 } },
              { type: "text", text: prompt },
            ],
          },
        ],
      }),
    };
  }

  // azure-openai and openai-completions share the same body shape.
  return {
    url,
    headers,
    body: JSON.stringify({
      model: modelId,
      messages: [
        {
          role: "user",
          content: [
            { type: "text", text: prompt },
            { type: "image_url", image_url: { url: `data:${mimeType};base64,${imageBase64}` } },
          ],
        },
      ],
    }),
  };
}

export interface VisionCallResult {
  text: string;
  tokensIn: number;
  tokensOut: number;
}

// callVisionProvider sends one multimodal chat-completion request to the
// resolved vision provider and returns the extracted text plus token counts
// (for spend-cap metering — the caller emits EmitLlmUsage with these, cost 0,
// so the broker computes cost from tokens × provider rate). Runs only in the
// parent process — the real api key never leaves this call.
export async function callVisionProvider(opts: CallVisionProviderOptions): Promise<VisionCallResult> {
  const { provider } = opts;
  const { url, headers, body } = buildProviderRequest(opts);

  // AbortController timeout (reason.ts's implementation, verbatim): the timer
  // aborts the request; the signal cancels both the header wait and the body
  // read. Cleared in finally so it never outlives the call.
  const controller = new AbortController();
  const timer =
    opts.timeoutMs !== undefined ? setTimeout(() => controller.abort(), opts.timeoutMs) : undefined;
  timer?.unref?.();
  try {
    let response: Response;
    try {
      response = await fetch(url, { method: "POST", headers, body, signal: controller.signal });
    } catch {
      // Never interpolate the caught error into the message: some transport
      // errors echo back request details that could include the key.
      if (controller.signal.aborted) {
        throw new VisionProviderError(`vision provider ${provider.id}: timed out after ${opts.timeoutMs}ms`);
      }
      throw new VisionProviderError(`vision provider ${provider.id}: connection failed`);
    }

    if (!response.ok) {
      const snippet = (await response.text().catch(() => "")).slice(0, 512);
      throw new VisionProviderError(
        `vision provider ${provider.id}: upstream returned ${response.status}${snippet ? `: ${snippet}` : ""}`,
        response.status,
      );
    }

    const json: unknown = await response.json();
    const makeError = (message: string) => new VisionProviderError(`vision provider ${provider.id}: ${message}`);
    const text =
      provider.api === "anthropic-messages"
        ? extractAnthropicText(json, makeError)
        : extractOpenAIText(json, makeError);
    const usage = provider.api === "anthropic-messages" ? extractAnthropicUsage(json) : extractOpenAIUsage(json);
    return { text, ...usage };
  } finally {
    if (timer) clearTimeout(timer);
  }
}
