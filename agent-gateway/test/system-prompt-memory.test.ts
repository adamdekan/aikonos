// The memory-discipline block is conditional on the memory tools being active,
// and its write half is conditional again on memory_write.
//
// WHY this test exists: read-only memory is a supported posture, so a session
// that cannot write must not be taught to record concepts — guidance for a tool
// the model does not have is what produces invented tool calls. Mirrors
// system-prompt-reason-step.test.ts.
import { test } from "node:test";
import assert from "node:assert/strict";
import { buildSystemPrompt } from "../src/pi/system-prompt.js";

// Distinctive phrases pinned from the memory block's three parts.
const MEMORY_MARKER = "You have durable memory";
const READ_MARKER = "Recall progressively with `memory_read`";
const WRITE_MARKER = "Record with `memory_write`";

test("buildSystemPrompt: no memory block when no memory tool is active", () => {
  const prompt = buildSystemPrompt(["web_fetch", "doc_read"]);
  assert.ok(!prompt.includes(MEMORY_MARKER));
  assert.ok(!prompt.includes(READ_MARKER));
  assert.ok(!prompt.includes(WRITE_MARKER));
});

test("buildSystemPrompt: memory block present when memory_read is active", () => {
  const prompt = buildSystemPrompt(["memory_read", "web_fetch"]);
  assert.ok(prompt.includes(MEMORY_MARKER));
  // The index → frontmatter → concept discipline, and the untrusted-data posture.
  assert.ok(prompt.includes(READ_MARKER));
  assert.ok(/not instruction/i.test(prompt));
});

test("buildSystemPrompt: a read-only memory session gets no write guidance", () => {
  const prompt = buildSystemPrompt(["memory_read"]);
  assert.ok(prompt.includes(READ_MARKER));
  assert.ok(!prompt.includes(WRITE_MARKER));
});

test("buildSystemPrompt: write guidance present when memory_write is active", () => {
  const prompt = buildSystemPrompt(["memory_write"]);
  assert.ok(prompt.includes(MEMORY_MARKER));
  assert.ok(prompt.includes(WRITE_MARKER));
  // Write-only session: the recall discipline belongs to memory_read alone.
  assert.ok(!prompt.includes(READ_MARKER));
});
