// resolveDialectRequest family→wire mapping pin.
//
// WHY: the provider catalog grew from three families to five, and the two new
// ones (google-gemini, aws-bedrock) are consumed through their official
// OpenAI-compatible endpoints — the stored base endpoint carries the compat
// prefix, so they must resolve to exactly the openai-completions path/headers
// and never grow their own suffix or auth header without a deliberate change
// here. The unknown-string case pins the pre-existing fallthrough: an `api`
// value this build doesn't recognize still speaks OpenAI, it does not throw.
import { test } from "node:test";
import assert from "node:assert/strict";

import { resolveDialectRequest, resolveEmbeddingsRequest, ANTHROPIC_VERSION } from "../src/llm/provider-dialect.js";

const OPENAI_COMPAT = {
  path: "/chat/completions",
  headers: { authorization: "Bearer k1" },
};

test("resolveDialectRequest: anthropic-messages → x-api-key + /v1/messages", () => {
  const got = resolveDialectRequest({ api: "anthropic-messages", apiKey: "k1", modelId: "claude-sonnet-4.6" });
  assert.deepEqual(got, {
    path: "/v1/messages",
    headers: { "x-api-key": "k1", "anthropic-version": ANTHROPIC_VERSION },
  });
});

test("resolveDialectRequest: azure-openai → api-key + deployment route carrying the api version", () => {
  const got = resolveDialectRequest({
    api: "azure-openai",
    apiKey: "k1",
    apiVersion: "2024-10-21",
    modelId: "gpt-4o",
  });
  assert.deepEqual(got, {
    path: "/openai/deployments/gpt-4o/chat/completions?api-version=2024-10-21",
    headers: { "api-key": "k1" },
  });
});

test("resolveDialectRequest: openai-completions → Bearer + /chat/completions", () => {
  const got = resolveDialectRequest({ api: "openai-completions", apiKey: "k1", modelId: "gpt-4o" });
  assert.deepEqual(got, OPENAI_COMPAT);
});

test("resolveDialectRequest: google-gemini resolves to the OpenAI-compat shape", () => {
  const got = resolveDialectRequest({ api: "google-gemini", apiKey: "k1", modelId: "gemini-2.5-pro" });
  assert.deepEqual(got, OPENAI_COMPAT);
});

test("resolveDialectRequest: aws-bedrock resolves to the OpenAI-compat shape", () => {
  const got = resolveDialectRequest({ api: "aws-bedrock", apiKey: "k1", modelId: "anthropic.claude-sonnet-4-v1" });
  assert.deepEqual(got, OPENAI_COMPAT);
});

test("resolveDialectRequest: unrecognized api falls through to the OpenAI-compat shape", () => {
  const got = resolveDialectRequest({ api: "some-future-family", apiKey: "k1", modelId: "m1" });
  assert.deepEqual(got, OPENAI_COMPAT);
});

test("resolveDialectRequest: modelId only affects the azure deployment path", () => {
  // The three compat families put the model in the body, not the URL — a
  // regression that started interpolating it into the path fails here.
  for (const api of ["openai-completions", "google-gemini", "aws-bedrock"]) {
    const got = resolveDialectRequest({ api, apiKey: "k1", modelId: "model-in-url?" });
    assert.equal(got.path, "/chat/completions", `${api} must not carry the model in its path`);
  }
});

// ── resolveEmbeddingsRequest ──────────────

test("resolveEmbeddingsRequest: azure-openai → api-key + deployment route carrying the api version", () => {
  const got = resolveEmbeddingsRequest({
    api: "azure-openai",
    apiKey: "k1",
    apiVersion: "2024-10-21",
    modelId: "text-embed-3",
  });
  assert.deepEqual(got, {
    path: "/openai/deployments/text-embed-3/embeddings?api-version=2024-10-21",
    headers: { "api-key": "k1" },
  });
});

test("resolveEmbeddingsRequest: anthropic-messages throws before any network call", () => {
  assert.throws(
    () => resolveEmbeddingsRequest({ api: "anthropic-messages", apiKey: "k1", modelId: "claude-sonnet-4.6" }),
    /embeddings/i,
  );
});

const EMBED_BEARER = { path: "/embeddings", headers: { authorization: "Bearer k1" } };

test("resolveEmbeddingsRequest: openai-completions → Bearer + /embeddings", () => {
  assert.deepEqual(
    resolveEmbeddingsRequest({ api: "openai-completions", apiKey: "k1", modelId: "text-embed-3" }),
    EMBED_BEARER,
  );
});

test("resolveEmbeddingsRequest: google-gemini → Bearer + /embeddings", () => {
  assert.deepEqual(
    resolveEmbeddingsRequest({ api: "google-gemini", apiKey: "k1", modelId: "text-embedding-004" }),
    EMBED_BEARER,
  );
});

test("resolveEmbeddingsRequest: aws-bedrock → Bearer + /embeddings", () => {
  assert.deepEqual(
    resolveEmbeddingsRequest({ api: "aws-bedrock", apiKey: "k1", modelId: "amazon.titan-embed-text-v2" }),
    EMBED_BEARER,
  );
});

test("resolveEmbeddingsRequest: unrecognized api falls through to the Bearer /embeddings shape", () => {
  assert.deepEqual(
    resolveEmbeddingsRequest({ api: "some-future-family", apiKey: "k1", modelId: "m1" }),
    EMBED_BEARER,
  );
});
