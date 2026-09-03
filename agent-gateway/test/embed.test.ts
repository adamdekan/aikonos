// P6: src/llm/embed.ts — callEmbeddingProvider.
// Mirrors reason.test.ts's structure (same AbortController/timeout/extraction
// pattern). Provider selection is not tested here: embed.ts consumes
// embeddingCandidates, covered by provider-fallback.test.ts.
import { test, mock } from "node:test";
import assert from "node:assert/strict";
import { callEmbeddingProvider, EmbeddingProviderError } from "../src/llm/embed.js";

const KEY = "sk-super-secret-test-key";

function fakeFetchOk(body: unknown) {
  return mock.method(globalThis, "fetch", async () =>
    new Response(JSON.stringify(body), { status: 200, headers: { "content-type": "application/json" } }),
  );
}

function headerValue(headers: RequestInit["headers"], name: string): string | undefined {
  if (!headers || headers instanceof Headers || Array.isArray(headers)) {
    return undefined;
  }
  const value = headers[name];
  return typeof value === "string" ? value : undefined;
}

test.afterEach(() => {
  mock.restoreAll();
});

test("callEmbeddingProvider: openai dialect builds the expected request and parses the ordered vectors", async () => {
  const fetchMock = fakeFetchOk({
    data: [{ embedding: [0.1, 0.2] }, { embedding: [0.3, 0.4] }],
    usage: { prompt_tokens: 12 },
  });

  const { embeddings, tokensIn } = await callEmbeddingProvider({
    provider: { id: "openai", endpoint: "https://api.openai.com/v1", api: "openai-completions", apiKey: KEY },
    modelId: "text-embed-3",
    inputs: ["hello", "world"],
  });

  assert.deepEqual(embeddings, [[0.1, 0.2], [0.3, 0.4]], "vectors must stay in the order data[] returned them");
  assert.equal(tokensIn, 12, "tokensIn must come from usage.prompt_tokens");
  const [url, init] = fetchMock.mock.calls[0].arguments;
  assert.equal(url, "https://api.openai.com/v1/embeddings");
  assert.equal(headerValue(init?.headers, "authorization"), `Bearer ${KEY}`);
  const body = JSON.parse(String(init?.body));
  assert.equal(body.model, "text-embed-3");
  assert.deepEqual(body.input, ["hello", "world"]);
});

test("callEmbeddingProvider: azure-openai dialect builds the deployment route with the api-key header", async () => {
  const fetchMock = fakeFetchOk({ data: [{ embedding: [1, 2, 3] }] });

  await callEmbeddingProvider({
    provider: {
      id: "azure",
      endpoint: "https://foo.openai.azure.com",
      api: "azure-openai",
      apiKey: KEY,
      apiVersion: "2024-06-01",
    },
    modelId: "text-embed-deploy",
    inputs: ["hi"],
  });

  const [url, init] = fetchMock.mock.calls[0].arguments;
  assert.equal(
    url,
    "https://foo.openai.azure.com/openai/deployments/text-embed-deploy/embeddings?api-version=2024-06-01",
  );
  assert.equal(headerValue(init?.headers, "api-key"), KEY);
  assert.equal(headerValue(init?.headers, "authorization"), undefined);
});

test("callEmbeddingProvider: anthropic-messages rejects before any fetch call", async () => {
  const fetchMock = mock.method(globalThis, "fetch", async () => {
    throw new Error("must not be called");
  });

  await assert.rejects(
    () =>
      callEmbeddingProvider({
        provider: { id: "anthropic", endpoint: "https://api.anthropic.com", api: "anthropic-messages", apiKey: KEY },
        modelId: "claude-sonnet-4.6",
        inputs: ["hi"],
      }),
    /embeddings/i,
  );
  assert.equal(fetchMock.mock.calls.length, 0, "must never reach the network");
});

test("callEmbeddingProvider: missing usage block reports tokensIn 0, not a throw", async () => {
  fakeFetchOk({ data: [{ embedding: [1] }] });
  const { tokensIn } = await callEmbeddingProvider({
    provider: { id: "openai", endpoint: "https://api.openai.com/v1", api: "openai-completions", apiKey: KEY },
    modelId: "text-embed-3",
    inputs: ["hi"],
  });
  assert.equal(tokensIn, 0);
});

test("callEmbeddingProvider: malformed data (not an array) throws, never the key", async () => {
  fakeFetchOk({ data: "not-an-array" });

  await assert.rejects(
    () =>
      callEmbeddingProvider({
        provider: { id: "openai", endpoint: "https://api.openai.com/v1", api: "openai-completions", apiKey: KEY },
        modelId: "text-embed-3",
        inputs: ["hi"],
      }),
    (err: unknown) => {
      assert.ok(err instanceof EmbeddingProviderError);
      assert.match(err.message, /unexpected response shape/);
      assert.ok(!err.message.includes(KEY));
      return true;
    },
  );
});

test("callEmbeddingProvider: an entry missing a numeric embedding array throws", async () => {
  fakeFetchOk({ data: [{ embedding: ["not", "numbers"] }] });

  await assert.rejects(
    () =>
      callEmbeddingProvider({
        provider: { id: "openai", endpoint: "https://api.openai.com/v1", api: "openai-completions", apiKey: KEY },
        modelId: "text-embed-3",
        inputs: ["hi"],
      }),
    EmbeddingProviderError,
  );
});

test("callEmbeddingProvider: throws with status + provider id on a non-2xx response, never the key", async () => {
  mock.method(globalThis, "fetch", async () => new Response("unauthorized", { status: 401 }));

  await assert.rejects(
    () =>
      callEmbeddingProvider({
        provider: { id: "openai", endpoint: "https://api.openai.com/v1", api: "openai-completions", apiKey: KEY },
        modelId: "text-embed-3",
        inputs: ["hi"],
      }),
    (err: unknown) => {
      assert.ok(err instanceof EmbeddingProviderError);
      assert.equal(err.status, 401);
      assert.match(err.message, /401/);
      assert.match(err.message, /openai/);
      assert.ok(!err.message.includes(KEY));
      return true;
    },
  );
});

test("callEmbeddingProvider: aborts and throws a 'timed out' error when the provider never responds", async () => {
  mock.method(globalThis, "fetch", (_url: string, init?: RequestInit) =>
    new Promise((_resolve, reject) => {
      init?.signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")));
    }),
  );

  // The abort timer is deliberately unref()'d (a pending LLM call must never be
  // the reason the process stays alive), so the test has to hold the loop open
  // itself — otherwise node exits before the 20ms timer can fire (vision.test.ts
  // precedent).
  const keepAlive = setTimeout(() => {}, 5000);
  try {
    await assert.rejects(
      () =>
        callEmbeddingProvider({
          provider: { id: "openai", endpoint: "https://api.openai.com/v1", api: "openai-completions", apiKey: KEY },
          modelId: "text-embed-3",
          inputs: ["hi"],
          timeoutMs: 20,
        }),
      (err: unknown) => {
        assert.ok(err instanceof Error);
        assert.match(err.message, /timed out/i);
        assert.ok(!err.message.includes(KEY));
        return true;
      },
    );
  } finally {
    clearTimeout(keepAlive);
  }
});

test("callEmbeddingProvider: no timeoutMs → no AbortController abort, normal completion", async () => {
  fakeFetchOk({ data: [{ embedding: [1, 2] }] });
  const { embeddings } = await callEmbeddingProvider({
    provider: { id: "openai", endpoint: "https://api.openai.com/v1", api: "openai-completions", apiKey: KEY },
    modelId: "text-embed-3",
    inputs: ["hi"],
  });
  assert.deepEqual(embeddings, [[1, 2]]);
});

test("callEmbeddingProvider: a connection failure (non-abort) reports 'connection failed'", async () => {
  mock.method(globalThis, "fetch", async () => {
    throw new Error("ECONNREFUSED");
  });

  await assert.rejects(
    () =>
      callEmbeddingProvider({
        provider: { id: "openai", endpoint: "https://api.openai.com/v1", api: "openai-completions", apiKey: KEY },
        modelId: "text-embed-3",
        inputs: ["hi"],
      }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /connection failed/);
      return true;
    },
  );
});
