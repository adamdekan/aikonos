// G3 / L-23 (and F66): the parent-side provider calls (reason, vision, embed) send the
// real key in api-key / x-api-key, which fetch's redirect handling does NOT
// strip on a cross-origin hop (undici drops only authorization and cookie). A
// provider (or a MITM on an http endpoint) answering 307 must fail the call
// before a second host ever sees the key or the body.
import { test } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import { callReasonProvider } from "../src/llm/reason.js";
import { callVisionProvider } from "../src/llm/vision.js";
import { callEmbeddingProvider } from "../src/llm/embed.js";

const KEY = "sk-redirect-test-key";

async function listen(handler: http.RequestListener): Promise<{ url: string; close: () => Promise<void> }> {
  const server = http.createServer(handler);
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const addr = server.address();
  assert.ok(addr && typeof addr === "object");
  return {
    url: `http://127.0.0.1:${addr.port}`,
    close: () => new Promise<void>((resolve) => server.close(() => resolve())),
  };
}

test("G3: a 307 from the provider fails reason/vision/embed calls and never reaches the redirect target", async () => {
  const seenByTarget: http.IncomingHttpHeaders[] = [];
  const target = await listen((req, res) => {
    seenByTarget.push(req.headers);
    req.resume();
    res.end("{}");
  });
  const provider = await listen((req, res) => {
    req.resume();
    res.writeHead(307, { location: `${target.url}/steal` }).end();
  });
  try {
    await assert.rejects(
      callReasonProvider({
        provider: { id: "anth", endpoint: provider.url, api: "anthropic-messages", apiKey: KEY },
        modelId: "m",
        instruction: "hi",
        maxTokens: 16,
      }),
      (err: Error) => !err.message.includes(KEY),
    );
    await assert.rejects(
      callVisionProvider({
        provider: { id: "az", endpoint: provider.url, api: "azure-openai", apiKey: KEY, apiVersion: "2024-06-01" },
        modelId: "m",
        imageBase64: "AAAA",
        mimeType: "image/png",
      }),
      (err: Error) => !err.message.includes(KEY),
    );
    await assert.rejects(
      callEmbeddingProvider({
        provider: { id: "az", endpoint: provider.url, api: "azure-openai", apiKey: KEY, apiVersion: "2024-06-01" },
        modelId: "m",
        inputs: ["x"],
      }),
      (err: Error) => !err.message.includes(KEY),
    );
    assert.equal(seenByTarget.length, 0, "the redirect target must never receive the key or the body");
  } finally {
    await provider.close();
    await target.close();
  }
});

test("F66: an upstream error body is redacted before it reaches the model", async () => {
  const provider = await listen((req, res) => {
    req.resume();
    res.writeHead(400).end(JSON.stringify({ error: { message: "bad key sk-live-abcdefghijklmnop for https://internal.example/v1" } }));
  });
  try {
    await assert.rejects(
      callReasonProvider({
        provider: { id: "anth", endpoint: provider.url, api: "anthropic-messages", apiKey: KEY },
        modelId: "m",
        instruction: "hi",
        maxTokens: 16,
      }),
      (err: Error) => /upstream returned 400: bad key \[redacted\] for \[url\]/.test(err.message),
    );
  } finally {
    await provider.close();
  }
});
