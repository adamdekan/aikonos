// buildSystemPrompt's personal-skills note
// is conditional on hasPersonalSkills, mirroring the memory/workflow blocks'
// activeToolNames-gated pattern. Personal skills aren't a tool name — the
// caller (resolveSessionPlan) computes the flag from the union it built.
//
// WHY this test exists: personal skills are opt-in per user (only sessions with
// at least one valid Skills/<name>/ entry get the note). A session with none
// must not be taught a load_skill activation convention it can't exercise —
// same "don't bloat the prompt with unreachable guidance" posture as the
// memory-write block. Mirrors system-prompt-memory.test.ts.
import { test } from "node:test";
import assert from "node:assert/strict";
import { buildSystemPrompt } from "../src/pi/system-prompt.js";

const PERSONAL_MARKER = "personal skills the user authored themselves";

test("buildSystemPrompt: no personal-skills note when hasPersonalSkills is absent", () => {
  const prompt = buildSystemPrompt(["web_fetch", "delegate"]);
  assert.ok(!prompt.includes(PERSONAL_MARKER));
});

test("buildSystemPrompt: no personal-skills note when hasPersonalSkills is false", () => {
  const prompt = buildSystemPrompt(["web_fetch", "delegate"], undefined, "Available skills (call load_skill(name) to activate):\n- admin-skill: does a thing", undefined, false);
  assert.ok(!prompt.includes(PERSONAL_MARKER));
});

test("buildSystemPrompt: personal-skills note present when hasPersonalSkills is true", () => {
  const catalog = "Available skills (call load_skill(name) to activate):\n- personal:my-notes: My notes (personal)";
  const prompt = buildSystemPrompt(["web_fetch", "delegate"], undefined, catalog, undefined, true);
  assert.ok(prompt.includes(PERSONAL_MARKER));
  assert.ok(prompt.includes("load_skill"), "note must point the model at the load_skill activation path");
  // The note must come after the catalog it refers to.
  assert.ok(prompt.indexOf(catalog) < prompt.indexOf(PERSONAL_MARKER));
});
