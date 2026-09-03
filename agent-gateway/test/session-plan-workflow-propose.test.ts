// CP21 tests: buildSystemPrompt names workflow_propose as the improve path.
//
// WHY: when the resolved skills include `workflows`, the system prompt must
// tell the agent that `workflow_propose` is the way to improve/refine an
// existing workflow (create a proposed version through the owner-gated loop),
// distinct from `workflow_save` (new authoring). Without this guidance the
// agent has no signal to reach for propose vs save when the user asks to
// improve a workflow.
import { test } from "node:test";
import assert from "node:assert/strict";
import { buildSystemPrompt } from "../src/pi/session-plan.js";

test("CP21 buildSystemPrompt: with workflow_save in activeToolNames, names workflow_propose as the improve path", () => {
  // WHY: the improve guidance is conditional on workflow_save being active
  // (which is only true when the user holds the workflows skill). The mention
  // of workflow_propose must be in that block so unskilled users never see it.
  const prompt = buildSystemPrompt(["delegate", "web_fetch", "workflow_save", "workflow_run", "workflow_propose", "workflow_list", "workflow_publish"]);

  assert.ok(
    prompt.includes("workflow_propose"),
    `system prompt must name 'workflow_propose'; got: ${prompt}`,
  );
});

test("CP21 buildSystemPrompt: workflow_propose guidance describes it as the improve/refine path (proposed version)", () => {
  const prompt = buildSystemPrompt(["delegate", "workflow_save", "workflow_run", "workflow_propose"]);

  // The guidance must distinguish propose (improve existing → proposed version
  // awaiting owner approval) from save (new authoring → owned version directly).
  assert.ok(
    prompt.includes("workflow_propose"),
    "prompt must mention workflow_propose",
  );
  // Must convey that propose creates a proposed version, not a direct save.
  const lower = prompt.toLowerCase();
  assert.ok(
    lower.includes("propos") || lower.includes("improve") || lower.includes("refine"),
    `prompt must describe propose as improve/refine path; got: ${prompt}`,
  );
});

test("CP21 buildSystemPrompt: without workflow_save in activeToolNames, workflow_propose is NOT mentioned", () => {
  // WHY: a user without the workflows skill must see no workflow guidance at
  // all — not even a mention of workflow_propose. Leaking tool names to
  // unskilled users is a discoverability gap.
  const prompt = buildSystemPrompt(["delegate", "web_fetch"]);

  assert.ok(
    !prompt.includes("workflow_propose"),
    `prompt must NOT mention workflow_propose when workflows skill is absent; got: ${prompt}`,
  );
});

test("CP21 buildSystemPrompt: workflow_propose guidance is in the workflow tools block (not appended separately)", () => {
  // WHY: the propose guidance must live in the same conditional block as the
  // rest of the workflow tool guidance, so it only appears together with the
  // workflow preamble and not floating standalone.
  const withWorkflows = buildSystemPrompt(["delegate", "workflow_save", "workflow_propose"]);
  const withoutWorkflows = buildSystemPrompt(["delegate"]);

  // The block appears exactly once when workflows are active.
  const proposeCount = (withWorkflows.match(/workflow_propose/g) ?? []).length;
  assert.ok(proposeCount >= 1, "workflow_propose must appear at least once in the workflows-active prompt");

  // Absent entirely when workflows are not active.
  assert.ok(!withoutWorkflows.includes("workflow_propose"), "workflow_propose must be absent without workflows skill");
});
