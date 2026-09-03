// #<path> file-mention guidance.
//
// WHY this test exists:
//   The webui Composer inserts "#<path>" tokens when the user picks a workspace
//   file, but the system prompt only explained that convention for images
//   (analyze_image). Agents receiving "#report.pdf" asked the user what the "#"
//   meant instead of reading the file. The guidance must appear whenever a
//   file-read tool is active, and must stay absent when the agent has no way
//   to read files (telling it to read would be a dead instruction).
import { test } from "node:test";
import assert from "node:assert/strict";
import { buildSystemPrompt } from "../src/pi/system-prompt.js";

// Distinctive phrase pinned from the guidance paragraph.
const MENTION_MARKER = "is a reference to a file in your workspace";

test("buildSystemPrompt: #<path> guidance present when workspace_read is active", () => {
  const prompt = buildSystemPrompt(["workspace_read"]);
  assert.ok(prompt.includes(MENTION_MARKER));
});

test("buildSystemPrompt: #<path> guidance present when doc_read is active", () => {
  const prompt = buildSystemPrompt(["doc_read"]);
  assert.ok(prompt.includes(MENTION_MARKER));
});

test("buildSystemPrompt: #<path> guidance absent when no file-read tool is active", () => {
  const prompt = buildSystemPrompt(["web_fetch", "email_draft"]);
  assert.ok(!prompt.includes(MENTION_MARKER));
});
