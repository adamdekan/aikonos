// P6: parent-side embeddings HTTP call —
// the first non-chat consumer of the provider catalog. Mirrors reason.ts's
// AbortController/timeout/extraction structure; the body/response shape is
// the OpenAI-compatible /embeddings contract instead of chat completions.
//
// Runs only in the trusted parent process — the real provider API key never
// leaves this call.
import { resolveEmbeddingsRequest, isRecord, type DialectRequestOptions } from "./provider-dialect.js";

export interface EmbeddingCallProvider {
  id: string;
  endpoint: string;
  api: string;
  apiKey: string;
  // apiVersion is required when api is "azure-openai".
  apiVersion?: string;
}

export interface CallEmbeddingProviderOptions {
  provider: EmbeddingCallProvider;
  modelId: string;
  inputs: string[];
  // Abort the upstream request after this many ms so a hung provider can't
  // block a chat turn indefinitely. Omitted = no timeout.
  timeoutMs?: number;
}

// EmbeddingProviderError is thrown on a non-2xx upstream response, a malformed
// body, or a transport failure. The message never echoes the api key.
//
// `status` carries the upstream HTTP status when the failure WAS a response,
// so a caller can classify a retry through provider-fallback's shouldFailover.
// Undefined for a transport failure, a timeout, or an unparseable 200 body —
// all "this provider is not working", i.e. transport-class.
export class EmbeddingProviderError extends Error {
  constructor(message: string, readonly status?: number) {
    super(message);
  }
}

export interface EmbeddingCallResult {
  embeddings: number[][];
  tokensIn: number;
}

// extractEmbeddings pulls data[].embedding out of an OpenAI-shaped embeddings
// response, in the order the API returned them (== the order sent — the
// OpenAI-compatible /embeddings contract echoes input order). Any missing or
// non-numeric-array entry is treated as malformed.
function extractEmbeddings(body: unknown, makeError: (message: string) => Error): number[][] {
  if (!isRecord(body) || !Array.isArray(body.data)) {
    throw makeError("unexpected response shape");
  }
  return body.data.map((entry) => {
    if (!isRecord(entry) || !Array.isArray(entry.embedding) || !entry.embedding.every((v) => typeof v === "number")) {
      throw makeError("unexpected response shape");
    }
    return entry.embedding;
  });
}

// extractTokensIn reads usage.prompt_tokens off an embeddings response.
// Best-effort: a missing or malformed usage block returns 0 rather than
// throwing — the embeddings themselves are still usable without a token count.
function extractTokensIn(body: unknown): number {
  if (!isRecord(body) || !isRecord(body.usage)) return 0;
  return typeof body.usage.prompt_tokens === "number" ? body.usage.prompt_tokens : 0;
}

// callEmbeddingProvider sends one /embeddings request to the resolved
// embedding provider and returns the ordered vectors plus the input token
// count (for spend-cap metering — the caller emits EmitLlmUsage with this,
// cost 0, so the broker computes cost from tokens × provider rate).
export async function callEmbeddingProvider(opts: CallEmbeddingProviderOptions): Promise<EmbeddingCallResult> {
  const { provider, modelId, inputs } = opts;
  const base = provider.endpoint.replace(/\/+$/, "");
  const dialectOpts: DialectRequestOptions = {
    api: provider.api,
    apiKey: provider.apiKey,
    apiVersion: provider.apiVersion,
    modelId,
  };
  const { path, headers: authHeaders } = resolveEmbeddingsRequest(dialectOpts);
  const url = `${base}${path}`;
  const headers = { "content-type": "application/json", ...authHeaders };
  const body = JSON.stringify({ model: modelId, input: inputs });

  // AbortController timeout (reason.ts/vision.ts precedent): the timer aborts
  // the request; the signal cancels both the header wait and the body read.
  // Cleared in finally so it never outlives the call.
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
        throw new EmbeddingProviderError(`embedding provider ${provider.id}: timed out after ${opts.timeoutMs}ms`);
      }
      throw new EmbeddingProviderError(`embedding provider ${provider.id}: connection failed`);
    }

    if (!response.ok) {
      const snippet = (await response.text().catch(() => "")).slice(0, 512);
      throw new EmbeddingProviderError(
        `embedding provider ${provider.id}: upstream returned ${response.status}${snippet ? `: ${snippet}` : ""}`,
        response.status,
      );
    }

    const json: unknown = await response.json();
    const makeError = (message: string) => new EmbeddingProviderError(`embedding provider ${provider.id}: ${message}`);
    return { embeddings: extractEmbeddings(json, makeError), tokensIn: extractTokensIn(json) };
  } finally {
    if (timer) clearTimeout(timer);
  }
}
