// CP-R3: src/llm/reason.ts — callReasonProvider, parseReasonOutput.
// Mirrors vision.test.ts's structure (CP4 precedent). Provider selection is not
// tested here: reason.ts consumes chatCandidates, covered by
// provider-fallback.test.ts.
import { test, mock } from "node:test";
import assert from "node:assert/strict";
import {
  callReasonProvider,
  parseReasonOutput,
  ReasonOutputParseError,
} from "../src/llm/reason.js";

// ── callReasonProvider ───────────────────────────────────────────────────────

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

test("callReasonProvider: openai dialect builds the expected text-only request", async () => {
  const fetchMock = fakeFetchOk({
    choices: [{ message: { content: "42" } }],
    usage: { prompt_tokens: 10, completion_tokens: 3 },
  });

  const { text, tokensIn, tokensOut } = await callReasonProvider({
    provider: { id: "openai", endpoint: "https://api.openai.com/v1", api: "openai-completions", apiKey: KEY },
    modelId: "gpt-4o",
    instruction: "What is the answer?",
    maxTokens: 2048,
  });

  assert.equal(text, "42");
  assert.equal(tokensIn, 10, "tokensIn must come from usage.prompt_tokens");
  assert.equal(tokensOut, 3, "tokensOut must come from usage.completion_tokens");
  const [url, init] = fetchMock.mock.calls[0].arguments;
  assert.equal(url, "https://api.openai.com/v1/chat/completions");
  assert.equal(headerValue(init?.headers, "authorization"), `Bearer ${KEY}`);
  const body = JSON.parse(String(init?.body));
  assert.equal(body.model, "gpt-4o");
  assert.equal(body.max_completion_tokens, 2048);
  assert.deepEqual(body.messages, [{ role: "user", content: "What is the answer?" }]);
});

// A reasoning model spends max_completion_tokens on reasoning tokens as well as
// output, so it can hit the cap and return a well-formed response carrying
// content:"" with finish_reason:"length". That empty string used to flow back to
// parseReasonOutput, which failed with "Unexpected end of JSON input" — an error
// naming JSON when the actual problem was a token budget. This is what broke the
// 07-27 scheduled workflow run on the on-prem host at reason step 5.
test("callReasonProvider: a truncated openai completion reports truncation, not empty text", async () => {
  fakeFetchOk({
    choices: [{ message: { content: "" }, finish_reason: "length" }],
    usage: { prompt_tokens: 10, completion_tokens: 2048 },
  });

  await assert.rejects(
    callReasonProvider({
      provider: { id: "openai", endpoint: "https://api.openai.com/v1", api: "openai-completions", apiKey: KEY },
      modelId: "gpt-5.6-terra",
      instruction: "Summarise.",
      maxTokens: 2048,
    }),
    (err: unknown) => {
      const message = err instanceof Error ? err.message : String(err);
      assert.match(message, /truncated/, `expected a truncation error, got: ${message}`);
      assert.match(message, /finish_reason=length/);
      assert.ok(!message.includes(KEY), "must never echo the api key");
      return true;
    },
  );
});

test("callReasonProvider: a truncated anthropic completion reports truncation", async () => {
  fakeFetchOk({
    content: [],
    stop_reason: "max_tokens",
    usage: { input_tokens: 7, output_tokens: 2048 },
  });

  await assert.rejects(
    callReasonProvider({
      provider: { id: "anthropic", endpoint: "https://api.anthropic.com", api: "anthropic-messages", apiKey: KEY },
      modelId: "claude-sonnet-4.6",
      instruction: "Summarise.",
      maxTokens: 2048,
    }),
    (err: unknown) => {
      const message = err instanceof Error ? err.message : String(err);
      assert.match(message, /truncated/, `expected a truncation error, got: ${message}`);
      assert.match(message, /stop_reason=max_tokens/);
      return true;
    },
  );
});

// finish_reason=length is not the only route to an empty completion, so the
// truncation guard above does not cover this case on its own. A content filter
// reports content_filter, and a reasoning model that burns its whole budget on
// reasoning can return content:"" with finish_reason:"stop" — a 200 the guard
// waves through. That empty string reached parseReasonOutput and resurfaced as
// "Unexpected end of JSON input", pointing the reader at JSON when the response
// simply carried nothing.
test("callReasonProvider: an empty openai completion names its finish_reason, not JSON", async () => {
  fakeFetchOk({
    choices: [{ message: { content: "" }, finish_reason: "content_filter" }],
    usage: { prompt_tokens: 10, completion_tokens: 0 },
  });

  await assert.rejects(
    callReasonProvider({
      provider: { id: "openai", endpoint: "https://api.openai.com/v1", api: "openai-completions", apiKey: KEY },
      modelId: "gpt-5.6-terra",
      instruction: "Summarise.",
      maxTokens: 8192,
    }),
    (err: unknown) => {
      const message = err instanceof Error ? err.message : String(err);
      assert.match(message, /carried no content/, `expected an empty-content error, got: ${message}`);
      assert.match(message, /finish_reason=content_filter/);
      assert.doesNotMatch(message, /JSON/, "must not blame JSON for an empty response");
      assert.ok(!message.includes(KEY), "must never echo the api key");
      return true;
    },
  );
});

test("callReasonProvider: an empty anthropic completion names its stop_reason", async () => {
  fakeFetchOk({
    content: [{ type: "text", text: "   " }],
    stop_reason: "stop_sequence",
    usage: { input_tokens: 7, output_tokens: 0 },
  });

  await assert.rejects(
    callReasonProvider({
      provider: { id: "anthropic", endpoint: "https://api.anthropic.com", api: "anthropic-messages", apiKey: KEY },
      modelId: "claude-sonnet-4.6",
      instruction: "Summarise.",
      maxTokens: 8192,
    }),
    (err: unknown) => {
      const message = err instanceof Error ? err.message : String(err);
      assert.match(message, /carried no content/, `expected an empty-content error, got: ${message}`);
      assert.match(message, /stop_reason=stop_sequence/);
      return true;
    },
  );
});

// The downstream half of the same failure, pinned so the two guards above have
// a visible reason to exist: this is verbatim what the on-prem workflow run
// reported at reason step 5. An empty completion reaching this far can only
// produce an error about JSON, which sends the reader looking at the schema
// instead of at the token budget or the finish_reason that actually caused it.
test("parseReasonOutput: an empty string yields the misleading JSON error the guards prevent", () => {
  assert.throws(
    () => parseReasonOutput("", true, { type: "object", required: ["script"] }),
    (err: unknown) => {
      assert.ok(err instanceof ReasonOutputParseError);
      assert.equal(err.message, "could not parse reason output as JSON: Unexpected end of JSON input");
      return true;
    },
  );
});

test("callReasonProvider: anthropic-messages dialect builds the expected request", async () => {
  const fetchMock = fakeFetchOk({
    content: [{ type: "text", text: "hello" }],
    usage: { input_tokens: 7, output_tokens: 2 },
  });

  const { text, tokensIn, tokensOut } = await callReasonProvider({
    provider: { id: "anthropic", endpoint: "https://api.anthropic.com", api: "anthropic-messages", apiKey: KEY },
    modelId: "claude-sonnet",
    instruction: "say hi",
    maxTokens: 512,
  });

  assert.equal(text, "hello");
  assert.equal(tokensIn, 7, "tokensIn must come from usage.input_tokens");
  assert.equal(tokensOut, 2, "tokensOut must come from usage.output_tokens");
  const [url, init] = fetchMock.mock.calls[0].arguments;
  assert.equal(url, "https://api.anthropic.com/v1/messages");
  assert.equal(headerValue(init?.headers, "x-api-key"), KEY);
  assert.equal(headerValue(init?.headers, "anthropic-version"), "2023-06-01");
  const body = JSON.parse(String(init?.body));
  assert.equal(body.model, "claude-sonnet");
  assert.equal(body.max_tokens, 512);
  assert.deepEqual(body.messages, [{ role: "user", content: "say hi" }]);
});

test("callReasonProvider: azure-openai dialect builds the expected request", async () => {
  const fetchMock = fakeFetchOk({ choices: [{ message: { content: "ok" } }] });

  const { text } = await callReasonProvider({
    provider: { id: "azure", endpoint: "https://foo.openai.azure.com", api: "azure-openai", apiKey: KEY, apiVersion: "2024-06-01" },
    modelId: "gpt-4o-deploy",
    instruction: "hi",
    maxTokens: 100,
  });

  assert.equal(text, "ok");
  const [url, init] = fetchMock.mock.calls[0].arguments;
  assert.equal(
    url,
    "https://foo.openai.azure.com/openai/deployments/gpt-4o-deploy/chat/completions?api-version=2024-06-01",
  );
  assert.equal(headerValue(init?.headers, "api-key"), KEY);
  assert.equal(headerValue(init?.headers, "authorization"), undefined);
  const body = JSON.parse(String(init?.body));
  assert.equal(body.max_completion_tokens, 100);
});

test("callReasonProvider: appends the schema directive to the instruction when outputSchema is present", async () => {
  const fetchMock = fakeFetchOk({ choices: [{ message: { content: "{}" } }] });
  const schema = { type: "object", properties: { email: { type: "string" } }, required: ["email"] };

  await callReasonProvider({
    provider: { id: "openai", endpoint: "https://api.openai.com/v1", api: "openai-completions", apiKey: KEY },
    modelId: "gpt-4o",
    instruction: "extract the email",
    outputSchema: schema,
    maxTokens: 2048,
  });

  const [, init] = fetchMock.mock.calls[0].arguments;
  const body = JSON.parse(String(init?.body));
  const content = body.messages[0].content as string;
  assert.match(content, /extract the email/);
  assert.match(content, /JSON object/i);
  assert.ok(content.includes(JSON.stringify(schema)), "must embed the schema verbatim");
});

test("callReasonProvider: throws with status + provider id on a non-2xx response, never the key", async () => {
  mock.method(globalThis, "fetch", async () => new Response("unauthorized", { status: 401 }));

  await assert.rejects(
    () =>
      callReasonProvider({
        provider: { id: "openai", endpoint: "https://api.openai.com/v1", api: "openai-completions", apiKey: KEY },
        modelId: "gpt-4o",
        instruction: "hi",
        maxTokens: 100,
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

// ── parseReasonOutput ────────────────────────────────────────────────────────

test("parseReasonOutput: hasSchema=false returns the raw text unchanged", () => {
  assert.equal(parseReasonOutput("some free text\nwith lines", false), "some free text\nwith lines");
});

test("parseReasonOutput: hasSchema=true parses bare JSON", () => {
  assert.deepEqual(parseReasonOutput('{"email":"a@b.com"}', true), { email: "a@b.com" });
});

test("parseReasonOutput: hasSchema=true strips a ```json fenced block", () => {
  const text = "```json\n{\"email\":\"a@b.com\"}\n```";
  assert.deepEqual(parseReasonOutput(text, true), { email: "a@b.com" });
});

test("parseReasonOutput: hasSchema=true strips a plain ``` fenced block", () => {
  const text = "```\n{\"email\":\"a@b.com\"}\n```";
  assert.deepEqual(parseReasonOutput(text, true), { email: "a@b.com" });
});

test("parseReasonOutput: hasSchema=true throws ReasonOutputParseError on malformed JSON", () => {
  assert.throws(() => parseReasonOutput("not json at all", true), ReasonOutputParseError);
});

// ── Task 4: required-keys check ────────────────────────────────────────────────

test("parseReasonOutput: all required keys present → parses", () => {
  const schema = { type: "object", required: ["a", "b"] };
  assert.deepEqual(parseReasonOutput('{"a":1,"b":2}', true, schema), { a: 1, b: 2 });
});

test("parseReasonOutput: a missing required key throws ReasonOutputParseError", () => {
  const schema = { type: "object", required: ["a", "b"] };
  assert.throws(() => parseReasonOutput('{"a":1}', true, schema), ReasonOutputParseError);
});

test("parseReasonOutput: required keys but output is not an object throws", () => {
  const schema = { required: ["a"] };
  assert.throws(() => parseReasonOutput("[1,2,3]", true, schema), ReasonOutputParseError);
});

test("parseReasonOutput: schema without required keeps parse-only behaviour", () => {
  const schema = { type: "object", properties: { a: { type: "string" } } };
  assert.deepEqual(parseReasonOutput('{"x":1}', true, schema), { x: 1 });
});

test("parseReasonOutput: no schema arg keeps parse-only behaviour", () => {
  assert.deepEqual(parseReasonOutput('{"a":1}', true), { a: 1 });
});

// ── Task 2: upstream timeout ───────────────────────────────────────────────────

test("callReasonProvider: aborts and throws a 'timed out' error when the provider never responds", async () => {
  // fetch that only ever settles by rejecting on abort — models a hung provider.
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
        callReasonProvider({
          provider: { id: "openai", endpoint: "https://api.openai.com/v1", api: "openai-completions", apiKey: KEY },
          modelId: "gpt-4o",
          instruction: "hi",
          maxTokens: 100,
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

test("callReasonProvider: no timeoutMs → no AbortController abort, normal completion", async () => {
  // A resolving fetch with timeoutMs omitted must still complete cleanly.
  fakeFetchOk({ choices: [{ message: { content: "done" } }] });
  const { text } = await callReasonProvider({
    provider: { id: "openai", endpoint: "https://api.openai.com/v1", api: "openai-completions", apiKey: KEY },
    modelId: "gpt-4o",
    instruction: "hi",
    maxTokens: 100,
  });
  assert.equal(text, "done");
});
