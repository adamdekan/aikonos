// CP5: system prompt teaches spawn_subagents when it is an active tool.
// .
//
// WHY this test exists: the model cannot discover the fan-out width cap, the
// depth-1 restriction, or the approval-denial behavior by trying — the spec
// requires the prompt teach these real bounds explicitly. Mirrors the
// workflow_schedule precedent (system-prompt-workflow-schedule.test.ts).
import { test } from "node:test";
import assert from "node:assert/strict";
import { buildSystemPrompt } from "../src/pi/system-prompt.js";

test("buildSystemPrompt: mentions spawn_subagents and its real bounds when it is active", () => {
  const prompt = buildSystemPrompt(["spawn_subagents"]);
  assert.ok(prompt.includes("spawn_subagents"), "prompt must mention spawn_subagents by name");
  assert.ok(/width.*cap|cap.*width/i.test(prompt), "prompt must mention the fan-out width cap");
  assert.ok(/3/.test(prompt), "prompt must state the default cap of 3");
  assert.ok(/cannot spawn|no nesting|nested/i.test(prompt), "prompt must state subagents cannot spawn subagents");
  assert.ok(/denied|refused/i.test(prompt), "prompt must state that a human-approval-requiring call is denied, not awaited");
});

test("buildSystemPrompt: no spawn_subagents guidance when the tool is not active", () => {
  const prompt = buildSystemPrompt(["web_fetch"]);
  assert.ok(!prompt.includes("spawn_subagents"), "prompt must not mention spawn_subagents when it is not an active tool");
});
