// CP-R3: workflow `reason` step — bounded parent-side LLM call (text-only,
// no tool/image capability). Mirrors vision.ts (CP4) structure/typing exactly,
// minus the multimodal content block.
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
// ── callReasonProvider ───────────────────────────────────────────────────────

export interface ReasonCallProvider {
  id: string;
  endpoint: string;
  api: string;
  apiKey: string;
  // apiVersion is required when api is "azure-openai".
  apiVersion?: string;
}

export interface CallReasonProviderOptions {
  provider: ReasonCallProvider;
  modelId: string;
  instruction: string;
  outputSchema?: Record<string, unknown>;
  maxTokens: number;
  // Abort the upstream request after this many ms so a hung provider can't
  // block a workflow run indefinitely. Reuses the gateway egress timeout
  // (Config.egressTimeoutMs) — no new env knob. Omitted = no timeout.
  timeoutMs?: number;
}

// ReasonProviderError is thrown on a non-2xx upstream response or a transport
// failure. The message includes the provider id + status but never the key.
//
// `status` carries the upstream HTTP status when the failure WAS a response, so
// a caller can classify a retry through provider-fallback's shouldFailover. It
// is undefined for a transport failure, a timeout, or a 200 whose body could not
// be read — all of which are "this provider is not working", i.e. transport-class.
export class ReasonProviderError extends Error {
  constructor(message: string, readonly status?: number) {
    super(message);
  }
}

// buildInstruction appends a directive to reply with exactly one JSON object
// matching outputSchema, embedding the schema verbatim, when present.
function buildInstruction(instruction: string, outputSchema?: Record<string, unknown>): string {
  if (!outputSchema) return instruction;
  return `${instruction}\n\nReply with exactly ONE JSON object matching this schema, no prose, no code fences:\n${JSON.stringify(outputSchema)}`;
}

interface ProviderRequest {
  url: string;
  headers: Record<string, string>;
  body: string;
}

// buildProviderRequest builds the per-dialect URL/headers/body. URL/headers/
// auth resolution is shared with vision.ts and egress-proxy.ts via
// provider-dialect.ts; only the body shape (plain text, minus vision.ts's
// image content block) is per-call-site here.
function buildProviderRequest(opts: CallReasonProviderOptions): ProviderRequest {
  const { provider, modelId, maxTokens } = opts;
  const content = buildInstruction(opts.instruction, opts.outputSchema);
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
        max_tokens: maxTokens,
        messages: [{ role: "user", content }],
      }),
    };
  }

  // azure-openai and openai-completions share the same body shape.
  // max_completion_tokens: newer OpenAI-dialect models reject max_tokens
  return {
    url,
    headers,
    body: JSON.stringify({ model: modelId, messages: [{ role: "user", content }], max_completion_tokens: maxTokens }),
  };
}

export interface ReasonCallResult {
  text: string;
  tokensIn: number;
  tokensOut: number;
}

// callReasonProvider sends one text-only chat-completion request to the
// resolved chat provider and returns the extracted text plus token counts
// (for spend-cap metering — the caller emits EmitLlmUsage with these, cost 0,
// so the broker computes cost from tokens × provider rate). Runs only in the
// parent process — the real api key never leaves this call.
export async function callReasonProvider(opts: CallReasonProviderOptions): Promise<ReasonCallResult> {
  const { provider } = opts;
  const { url, headers, body } = buildProviderRequest(opts);

  // AbortController timeout (F10 egress-proxy precedent): the timer aborts the
  // request; the signal cancels both the header wait and the body read. Cleared
  // in finally so it never outlives the call.
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
        throw new ReasonProviderError(`reason provider ${provider.id}: timed out after ${opts.timeoutMs}ms`);
      }
      throw new ReasonProviderError(`reason provider ${provider.id}: connection failed`);
    }

    if (!response.ok) {
      const snippet = (await response.text().catch(() => "")).slice(0, 512);
      throw new ReasonProviderError(
        `reason provider ${provider.id}: upstream returned ${response.status}${snippet ? `: ${snippet}` : ""}`,
        response.status,
      );
    }

    const json: unknown = await response.json();
    const makeError = (message: string) => new ReasonProviderError(`reason provider ${provider.id}: ${message}`);
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

// ── parseReasonOutput ────────────────────────────────────────────────────────

// ReasonOutputParseError is thrown when hasSchema is true and the model's
// text cannot be parsed as JSON (after stripping an optional fenced block).
export class ReasonOutputParseError extends Error {}

const FENCED_BLOCK_RE = /^```(?:json)?\s*\n([\s\S]*?)\n?```$/;

// parseReasonOutput returns the raw text unchanged when hasSchema is false.
// When hasSchema is true, it strips an optional fenced code block and
// JSON.parses the remainder, throwing ReasonOutputParseError on failure.
// When the schema declares a top-level `required: [...]`, every listed key
// must be present in the parsed object — a lightweight required-keys check,
// not a full JSON-Schema engine. A schema without `required` (or a non-object
// schema) keeps the parse-only behavior.
export function parseReasonOutput(
  text: string,
  hasSchema: boolean,
  schema?: Record<string, unknown>,
): unknown {
  if (!hasSchema) return text;

  const trimmed = text.trim();
  const fenced = trimmed.match(FENCED_BLOCK_RE);
  const candidate = fenced ? fenced[1] : trimmed;

  let parsed: unknown;
  try {
    parsed = JSON.parse(candidate);
  } catch (err) {
    throw new ReasonOutputParseError(`could not parse reason output as JSON: ${err instanceof Error ? err.message : String(err)}`);
  }

  const required =
    schema && Array.isArray(schema.required)
      ? schema.required.filter((k): k is string => typeof k === "string")
      : undefined;
  if (required && required.length > 0) {
    if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
      throw new ReasonOutputParseError(
        `reason output is not a JSON object carrying required key(s): ${required.join(", ")}`,
      );
    }
    const missing = required.filter((k) => !(k in parsed));
    if (missing.length > 0) {
      throw new ReasonOutputParseError(`reason output missing required key(s): ${missing.join(", ")}`);
    }
  }

  return parsed;
}
