// Tests for redactCredentials (zt-remediation) — pure string sanitiser in agui.ts.
// Validates: known credential patterns are replaced; clean text is unchanged;
// multiple matches in one string are all redacted; empty string is safe.
import { test } from "node:test";
import assert from "node:assert/strict";
import { redactCredentials } from "../src/routes/agui.js";

test("redactCredentials: AWS access key is redacted", () => {
  const input = "key=AKIAIOSFODNN7EXAMPLE and more text";
  const out = redactCredentials(input);
  assert.ok(!out.includes("AKIAIOSFODNN7EXAMPLE"), "AWS key must not appear in output");
  assert.ok(out.includes("[AWS-KEY-REDACTED]"), "AWS redaction marker must appear");
});

test("redactCredentials: GitHub PAT (ghp_) is redacted", () => {
  const input = "token: ghp_1234567890ABCDEFGHIJKLMNOPQRSTUVWXa1";
  const out = redactCredentials(input);
  assert.ok(!out.includes("ghp_1234567890"), "GH PAT must not appear in output");
  assert.ok(out.includes("[GH-TOKEN-REDACTED]"), "GH token redaction marker must appear");
});

test("redactCredentials: GitHub actions token (ghs_) is redacted", () => {
  // Token part after ghs_ must be ≥36 chars to satisfy the pattern {36,}
  const input = "ghs_1234567890abcdefghijklmnopqrstuvwxyz";
  const out = redactCredentials(input);
  assert.ok(!out.includes("ghs_1234567890"), "GH actions token must not appear in output");
  assert.ok(out.includes("[GH-TOKEN-REDACTED]"), "GH token redaction marker must appear");
});

test("redactCredentials: generic api_key credential is redacted", () => {
  const input = "api_key=secretvaluethatisverylongindeed123";
  const out = redactCredentials(input);
  assert.ok(!out.includes("secretvaluethatisverylongindeed123"), "secret value must not appear in output");
  assert.ok(out.includes("[CREDENTIAL-REDACTED]"), "generic credential redaction marker must appear");
});

test("redactCredentials: clean text passes through unchanged", () => {
  const input = "The weather today is nice.";
  const out = redactCredentials(input);
  assert.strictEqual(out, input, "clean text must not be modified");
});

test("redactCredentials: multiple matches in one string are all redacted", () => {
  const input = "aws=AKIAIOSFODNN7EXAMPLE github=ghp_1234567890ABCDEFGHIJKLMNOPQRSTUVWXa1";
  const out = redactCredentials(input);
  assert.ok(!out.includes("AKIAIOSFODNN7EXAMPLE"), "AWS key must be redacted");
  assert.ok(!out.includes("ghp_1234567890"), "GH PAT must be redacted");
  assert.ok(out.includes("[AWS-KEY-REDACTED]"), "AWS redaction marker must appear");
  assert.ok(out.includes("[GH-TOKEN-REDACTED]"), "GH token redaction marker must appear");
});

test("redactCredentials: empty string returns empty string", () => {
  assert.strictEqual(redactCredentials(""), "");
});
