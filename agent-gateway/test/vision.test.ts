import { test, mock } from "node:test";
import assert from "node:assert/strict";
// Provider selection is not tested here: vision.ts consumes visionCandidates,
// covered by provider-fallback.test.ts.
import { callVisionProvider } from "../src/llm/vision.js";

// ── callVisionProvider ───────────────────────────────────────────────────────

const KEY = "sk-super-secret-test-key";

function fakeFetchOk(body: unknown) {
  return mock.method(globalThis, "fetch", async () =>
    new Response(JSON.stringify(body), { status: 200, headers: { "content-type": "application/json" } }),
  );
}

// headerValue narrows RequestInit["headers"] (a HeadersInit union) down to a
// plain record so tests can assert on individual header names without a cast.
// callVisionProvider only ever builds plain Record<string,string> headers.
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

test("callVisionProvider: openai-completions dialect builds the expected request", async () => {
  const fetchMock = fakeFetchOk({
    choices: [{ message: { content: "a red apple" } }],
    usage: { prompt_tokens: 20, completion_tokens: 5 },
  });

  const { text, tokensIn, tokensOut } = await callVisionProvider({
    provider: { id: "openai", endpoint: "https://api.openai.com/v1", api: "openai-completions", apiKey: KEY },
    modelId: "gpt-4o",
    imageBase64: "AAAA",
    mimeType: "image/png",
    prompt: "describe this",
  });

  assert.equal(text, "a red apple");
  assert.equal(tokensIn, 20, "tokensIn must come from usage.prompt_tokens");
  assert.equal(tokensOut, 5, "tokensOut must come from usage.completion_tokens");
  assert.equal(fetchMock.mock.calls.length, 1);
  const [url, init] = fetchMock.mock.calls[0].arguments;
  assert.equal(url, "https://api.openai.com/v1/chat/completions");
  assert.equal(headerValue(init?.headers, "content-type"), "application/json");
  assert.equal(headerValue(init?.headers, "authorization"), `Bearer ${KEY}`);
  const body = JSON.parse(String(init?.body));
  assert.equal(body.model, "gpt-4o");
  assert.deepEqual(body.messages, [
    {
      role: "user",
      content: [
        { type: "text", text: "describe this" },
        { type: "image_url", image_url: { url: "data:image/png;base64,AAAA" } },
      ],
    },
  ]);
});

test("callVisionProvider: anthropic-messages dialect builds the expected request", async () => {
  const fetchMock = fakeFetchOk({
    content: [{ type: "text", text: "a blue car" }],
    usage: { input_tokens: 15, output_tokens: 4 },
  });

  const { text, tokensIn, tokensOut } = await callVisionProvider({
    provider: { id: "anthropic", endpoint: "https://api.anthropic.com", api: "anthropic-messages", apiKey: KEY },
    modelId: "claude-sonnet",
    imageBase64: "BBBB",
    mimeType: "image/jpeg",
  });

  assert.equal(text, "a blue car");
  assert.equal(tokensIn, 15, "tokensIn must come from usage.input_tokens");
  assert.equal(tokensOut, 4, "tokensOut must come from usage.output_tokens");
  const [url, init] = fetchMock.mock.calls[0].arguments;
  assert.equal(url, "https://api.anthropic.com/v1/messages");
  assert.equal(headerValue(init?.headers, "x-api-key"), KEY);
  assert.equal(headerValue(init?.headers, "anthropic-version"), "2023-06-01");
  assert.equal(headerValue(init?.headers, "authorization"), undefined);
  const body = JSON.parse(String(init?.body));
  assert.equal(body.model, "claude-sonnet");
  assert.deepEqual(body.messages, [
    {
      role: "user",
      content: [
        { type: "image", source: { type: "base64", media_type: "image/jpeg", data: "BBBB" } },
        { type: "text", text: "Describe this image." },
      ],
    },
  ]);
});

test("callVisionProvider: azure-openai dialect builds the expected request", async () => {
  const fetchMock = fakeFetchOk({ choices: [{ message: { content: "a green tree" } }] });

  const { text } = await callVisionProvider({
    provider: { id: "azure", endpoint: "https://foo.openai.azure.com", api: "azure-openai", apiKey: KEY, apiVersion: "2024-06-01" },
    modelId: "gpt-4o-deploy",
    imageBase64: "CCCC",
    mimeType: "image/png",
    prompt: "what is this",
  });

  assert.equal(text, "a green tree");
  const [url, init] = fetchMock.mock.calls[0].arguments;
  assert.equal(
    url,
    "https://foo.openai.azure.com/openai/deployments/gpt-4o-deploy/chat/completions?api-version=2024-06-01",
  );
  assert.equal(headerValue(init?.headers, "api-key"), KEY);
  assert.equal(headerValue(init?.headers, "authorization"), undefined);
});

test("callVisionProvider: throws with status + provider id on a non-2xx response, never the key", async () => {
  mock.method(globalThis, "fetch", async () =>
    new Response("unauthorized", { status: 401 }),
  );

  await assert.rejects(
    () =>
      callVisionProvider({
        provider: { id: "openai", endpoint: "https://api.openai.com/v1", api: "openai-completions", apiKey: KEY },
        modelId: "gpt-4o",
        imageBase64: "AAAA",
        mimeType: "image/png",
      }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /401/);
      assert.match(err.message, /openai/);
      assert.ok(!err.message.includes(KEY));
      return true;
    },
  );
});

test("callVisionProvider: a transport failure never leaks the key in its error", async () => {
  mock.method(globalThis, "fetch", async () => {
    throw new Error("connect ECONNREFUSED");
  });

  await assert.rejects(
    () =>
      callVisionProvider({
        provider: { id: "openai", endpoint: "https://api.openai.com/v1", api: "openai-completions", apiKey: KEY },
        modelId: "gpt-4o",
        imageBase64: "AAAA",
        mimeType: "image/png",
      }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.ok(!err.message.includes(KEY));
      return true;
    },
  );
});

// ── Upstream timeout ─────────────────────────────────────────────────────────
//
// WHY: callVisionProvider used a bare fetch with no AbortController, so a hung
// vision provider blocked analyze_image (and the child waiting on it) forever.
// Mirrors reason.test.ts's coverage of the same guard.

test("callVisionProvider: aborts and throws a 'timed out' error when the provider never responds", async () => {
  // fetch that only ever settles by rejecting on abort — models a hung provider.
  mock.method(globalThis, "fetch", (_url: string, init?: RequestInit) =>
    new Promise((_resolve, reject) => {
      init?.signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")));
    }),
  );

  // The abort timer is deliberately unref()'d (a pending LLM call must never be
  // the reason the process stays alive), so the test has to hold the loop open
  // itself — otherwise node exits before the 20ms timer can fire.
  const keepAlive = setTimeout(() => {}, 5000);
  try {
    await assert.rejects(
      () =>
        callVisionProvider({
          provider: { id: "openai", endpoint: "https://api.openai.com/v1", api: "openai-completions", apiKey: KEY },
          modelId: "gpt-4o",
          imageBase64: "AAAA",
          mimeType: "image/png",
          timeoutMs: 20,
        }),
      (err: unknown) => {
        assert.ok(err instanceof Error);
        assert.match(err.message, /timed out/i);
        // The abort error must never be interpolated — it can echo request details.
        assert.ok(!err.message.includes(KEY));
        return true;
      },
    );
  } finally {
    clearTimeout(keepAlive);
  }
});

test("callVisionProvider: no timeoutMs → no abort, normal completion", async () => {
  fakeFetchOk({ choices: [{ message: { content: "a cat" } }] });
  const { text } = await callVisionProvider({
    provider: { id: "openai", endpoint: "https://api.openai.com/v1", api: "openai-completions", apiKey: KEY },
    modelId: "gpt-4o",
    imageBase64: "AAAA",
    mimeType: "image/png",
  });
  assert.equal(text, "a cat");
});
