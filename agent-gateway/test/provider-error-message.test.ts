// providerErrorMessage: the chat-facing form of an LLM provider failure must
// keep the provider's explanation but never a key, URL or unbounded blob.
import { test } from "node:test";
import assert from "node:assert/strict";
import { providerErrorMessage } from "../src/http-errors.js";

test("providerErrorMessage keeps the explanation and strips secrets", () => {
  assert.equal(
    providerErrorMessage("Provider stopped with: sensitive"),
    "The model provider refused or failed this request: Provider stopped with: sensitive",
  );
  const out = providerErrorMessage("401 invalid x-api-key: sk-ant-api03-AAAAAAAAAAAAAAAA at https://api.example.com/v1?k=1 Bearer abcdefghijklmnopqrstuvwxyz0123456789");
  assert.doesNotMatch(out, /sk-ant|AAAAAAAA|https?:|abcdefghijklmnop/);
  assert.ok(providerErrorMessage("x".repeat(1000)).length < 400);
  assert.equal(providerErrorMessage(""), "The model provider refused or failed this request.");
});
