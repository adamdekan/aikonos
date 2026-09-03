// CP3: system prompt teaches workflow_schedule when it is an active tool.
//
// WHY this test exists: without a line teaching the tool, the model has no
// way to know workflow_schedule exists or how its recurrence/inputs work,
// even once it is visible in the session's tool list.
import { test } from "node:test";
import assert from "node:assert/strict";
import { buildSystemPrompt } from "../src/pi/system-prompt.js";

test("buildSystemPrompt: mentions workflow_schedule when it is active", () => {
  const prompt = buildSystemPrompt(["workflow_save", "workflow_schedule"]);
  assert.ok(prompt.includes("workflow_schedule"), "prompt must mention workflow_schedule by name");
  assert.ok(/cron|recurring/i.test(prompt), "prompt must mention the cron/recurring recurrence");
  assert.ok(/once|runAt/i.test(prompt), "prompt must mention the one-shot runAt recurrence");
});

test("buildSystemPrompt: no workflow_schedule guidance when the tool is not active", () => {
  const prompt = buildSystemPrompt(["workflow_save", "web_fetch"]);
  assert.ok(!prompt.includes("workflow_schedule"), "prompt must not mention workflow_schedule when it is not an active tool");
});
