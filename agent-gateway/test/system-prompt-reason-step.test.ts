// CP-R5: workflow guidance teaches the tool-step / reason-step factoring model.
//
// WHY this test exists:
//   Now that workflows support a `reason` step (CP-R1–R4), the system prompt
//   must teach the model to factor its own work: tool calls become tool
//   steps, the reasoning/synthesis it did between tool calls becomes reason
//   steps with the instruction written down. Without this the model keeps
//   inventing nonexistent skills (data.transform, chat.output, …) for
//   computation, exactly the failure the existing skill-vocabulary guidance
//   already fixed for tool steps.
import { test } from "node:test";
import assert from "node:assert/strict";
import { buildSystemPrompt } from "../src/pi/system-prompt.js";

// Distinctive phrase pinned from the reason-step guidance paragraph.
const REASON_MARKER = "kind: reason";

test("buildSystemPrompt: reason-step guidance present when workflow_save is active", () => {
  const prompt = buildSystemPrompt(["workflow_save", "web_fetch"]);
  assert.ok(prompt.includes(REASON_MARKER));
  // Existing exact-skill-id vocabulary sentence must remain.
  assert.ok(prompt.includes("MUST be exactly one of your available aikonos tool ids"));
});

test("buildSystemPrompt: reason-step guidance absent when workflow_save is not active", () => {
  const prompt = buildSystemPrompt(["web_fetch", "email_draft"]);
  assert.ok(!prompt.includes(REASON_MARKER));
});

test("buildSystemPrompt: reason-step guidance never invents skill ids for computation", () => {
  const prompt = buildSystemPrompt(["workflow_save", "doc_read"]);
  assert.ok(/never invent skill ids/i.test(prompt));
  assert.ok(/reason step/i.test(prompt));
});
