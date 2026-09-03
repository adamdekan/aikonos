// CP1: shared per-dialect wire knowledge
// for the LLM wire dialects the gateway speaks — anthropic-messages /
// azure-openai / openai-completions, the last shared by the two
// OpenAI-compatible families google-gemini + aws-bedrock and also the fallback
// for any unrecognized `api` value, matching prior per-call-site behavior.
// Extracted
// from vision.ts, reason.ts, and egress-proxy.ts, which each independently
// duplicated it. Pure extraction — no behavior change.
//
// Callers combine `path` with their own notion of "base URL": vision.ts and
// reason.ts build a full fetch() URL string (`${base}${path}`); egress-proxy.ts
// builds an http.request() path against a pre-parsed upstream URL
// (`${basePath}${path}`) and does not URL-encode modelId/apiVersion itself —
// callers that need encoding (egress-proxy.ts) must pre-encode before calling,
// since vision.ts/reason.ts never did.
//
// `headers` carries only the dialect's auth header(s) — content-type and any
// other passthrough headers remain each caller's concern.

export const ANTHROPIC_VERSION = "2023-06-01";

export interface DialectRequestOptions {
  api: string;
  apiKey: string;
  // apiVersion is required when api is "azure-openai".
  apiVersion?: string;
  modelId: string;
}

export interface DialectRequestTarget {
  path: string;
  headers: Record<string, string>;
}

export function resolveDialectRequest(opts: DialectRequestOptions): DialectRequestTarget {
  if (opts.api === "anthropic-messages") {
    return {
      path: "/v1/messages",
      headers: { "x-api-key": opts.apiKey, "anthropic-version": ANTHROPIC_VERSION },
    };
  }

  if (opts.api === "azure-openai") {
    return {
      path: `/openai/deployments/${opts.modelId}/chat/completions?api-version=${opts.apiVersion}`,
      headers: { "api-key": opts.apiKey },
    };
  }

  // google-gemini and aws-bedrock are only ever reached through their official
  // OpenAI-compatible endpoints — https://generativelanguage.googleapis.com/v1beta/openai
  // and https://bedrock-runtime.<region>.amazonaws.com/openai/v1 — so the
  // provider's stored base endpoint already carries the compat prefix and the
  // request needs no body translation: same Bearer + /chat/completions shape as
  // openai-completions. Named explicitly (rather than left to the fallthrough)
  // so a future native Gemini/Bedrock dialect has an obvious hook.
  if (opts.api === "google-gemini" || opts.api === "aws-bedrock") {
    return {
      path: "/chat/completions",
      headers: { authorization: `Bearer ${opts.apiKey}` },
    };
  }

  // default: openai-completions (and any unrecognized api value)
  return {
    path: "/chat/completions",
    headers: { authorization: `Bearer ${opts.apiKey}` },
  };
}

// resolveEmbeddingsRequest is resolveDialectRequest's twin for the /embeddings
// route: azure-openai speaks through the same
// deployment path shape as chat completions, just against the embeddings
// endpoint; anthropic-messages has no embeddings API at all — rejected before
// any network call, mirroring the broker-side probe builder — and every other
// family (both OpenAI-compat families, plus any unrecognized api value) shares
// one Bearer route.
export function resolveEmbeddingsRequest(opts: DialectRequestOptions): DialectRequestTarget {
  if (opts.api === "anthropic-messages") {
    throw new Error("anthropic-messages does not support an embeddings API");
  }

  if (opts.api === "azure-openai") {
    return {
      path: `/openai/deployments/${opts.modelId}/embeddings?api-version=${opts.apiVersion}`,
      headers: { "api-key": opts.apiKey },
    };
  }

  return {
    path: "/embeddings",
    headers: { authorization: `Bearer ${opts.apiKey}` },
  };
}

// isRecord narrows unknown to a plain object without an `as` cast.
export function isRecord(v: unknown): v is Record<string, unknown> {
  return v !== null && typeof v === "object" && !Array.isArray(v);
}

// extractOpenAIText pulls choices[0].message.content out of an OpenAI-shaped
// (and Azure-OpenAI-shaped, same body) chat-completions response. `makeError`
// lets each caller keep its own Error subclass + message prefix.
export function extractOpenAIText(body: unknown, makeError: (message: string) => Error): string {
  if (!isRecord(body) || !Array.isArray(body.choices) || body.choices.length === 0) {
    throw makeError("unexpected response shape");
  }
  const [choice] = body.choices;
  if (!isRecord(choice)) {
    throw makeError("unexpected response shape");
  }
  // Truncation is checked before content, and reported as truncation. A
  // reasoning model spends max_completion_tokens on reasoning tokens too, so it
  // can hit the cap and return content:"" with finish_reason:"length" — a
  // perfectly well-formed response carrying nothing. Returning that "" sends the
  // caller off to fail somewhere unrelated (a JSON.parse of empty string, say)
  // with an error that names the wrong cause.
  if (choice.finish_reason === "length") {
    throw makeError(
      "response truncated before completion (finish_reason=length) — raise the model's max token budget",
    );
  }
  const content = isRecord(choice.message) ? choice.message.content : undefined;
  if (typeof content !== "string") {
    throw makeError("unexpected response shape");
  }
  // finish_reason=length is not the only way to get an empty completion: a
  // content filter reports content_filter, and a reasoning model that spends its
  // whole budget on reasoning can return content:"" with finish_reason=stop.
  // An empty string is never a usable result for either caller — reason.ts
  // JSON.parses it, vision.ts reads it as prose — so it has to fail here, where
  // the finish_reason is still in hand to name. Letting "" through is what made
  // the on-prem workflow failure report a JSON error for a token problem.
  if (content.trim() === "") {
    const reason = typeof choice.finish_reason === "string" ? choice.finish_reason : "unknown";
    throw makeError(`response carried no content (finish_reason=${reason})`);
  }
  return content;
}

// extractAnthropicText pulls content[0].text out of an Anthropic messages response.
export function extractAnthropicText(body: unknown, makeError: (message: string) => Error): string {
  if (!isRecord(body)) {
    throw makeError("unexpected response shape");
  }
  // Same truncation check as the OpenAI dialect; Anthropic spells it stop_reason.
  // Checked before the content-array shape test because a truncated response can
  // come back with an empty content array.
  if (body.stop_reason === "max_tokens") {
    throw makeError(
      "response truncated before completion (stop_reason=max_tokens) — raise the model's max token budget",
    );
  }
  if (!Array.isArray(body.content) || body.content.length === 0) {
    throw makeError("unexpected response shape");
  }
  const [block] = body.content;
  const text = isRecord(block) ? block.text : undefined;
  if (typeof text !== "string") {
    throw makeError("unexpected response shape");
  }
  // Same reasoning as the OpenAI dialect: an empty text block is unusable to
  // both callers, and stop_reason=max_tokens is not the only way to get one.
  if (text.trim() === "") {
    const reason = typeof body.stop_reason === "string" ? body.stop_reason : "unknown";
    throw makeError(`response carried no content (stop_reason=${reason})`);
  }
  return text;
}

export interface ProviderUsage {
  tokensIn: number;
  tokensOut: number;
}

// extractOpenAIUsage reads usage.prompt_tokens/completion_tokens from an
// OpenAI-shaped (and Azure-OpenAI-shaped) chat-completions response.
// Best-effort: a missing or malformed usage block returns zeros rather than
// throwing — a reason/vision call must not fail just because token counts
// couldn't be read off the response; the broker still records the call with
// cost 0 in that case.
export function extractOpenAIUsage(body: unknown): ProviderUsage {
  if (!isRecord(body) || !isRecord(body.usage)) return { tokensIn: 0, tokensOut: 0 };
  const tokensIn = typeof body.usage.prompt_tokens === "number" ? body.usage.prompt_tokens : 0;
  const tokensOut = typeof body.usage.completion_tokens === "number" ? body.usage.completion_tokens : 0;
  return { tokensIn, tokensOut };
}

// extractAnthropicUsage reads usage.input_tokens/output_tokens from an
// Anthropic messages response. Same best-effort posture as extractOpenAIUsage.
export function extractAnthropicUsage(body: unknown): ProviderUsage {
  if (!isRecord(body) || !isRecord(body.usage)) return { tokensIn: 0, tokensOut: 0 };
  const tokensIn = typeof body.usage.input_tokens === "number" ? body.usage.input_tokens : 0;
  const tokensOut = typeof body.usage.output_tokens === "number" ? body.usage.output_tokens : 0;
  return { tokensIn, tokensOut };
}
