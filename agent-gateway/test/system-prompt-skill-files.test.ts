// buildSystemPrompt's read_skill_file teaching line is conditional on hasSkillBundles, mirroring the personal-skills note's
// hasPersonalSkills-gated pattern (system-prompt-personal-skills.test.ts).
//
// WHY this test exists: read_skill_file is registered whenever load_skill is
// (createSessionFromPlan's skillBundles.length > 0 gate) — a session with no
// skill bundles must not be taught a convention it has no tool to exercise.
import { test } from "node:test";
import assert from "node:assert/strict";
import { buildSystemPrompt } from "../src/pi/system-prompt.js";

const SKILL_FILES_MARKER = "read_skill_file(skill=";

test("buildSystemPrompt: no read_skill_file note when hasSkillBundles is absent", () => {
  const prompt = buildSystemPrompt(["web_fetch", "delegate"]);
  assert.ok(!prompt.includes(SKILL_FILES_MARKER));
});

test("buildSystemPrompt: no read_skill_file note when hasSkillBundles is false", () => {
  const prompt = buildSystemPrompt(["web_fetch", "delegate"], undefined, undefined, undefined, false, false);
  assert.ok(!prompt.includes(SKILL_FILES_MARKER));
});

test("buildSystemPrompt: read_skill_file note present when hasSkillBundles is true", () => {
  const catalog = "Available skills (call load_skill(name) to activate):\n- my-skill: does a thing";
  const prompt = buildSystemPrompt(["web_fetch", "delegate"], undefined, catalog, undefined, false, true);
  assert.ok(prompt.includes(SKILL_FILES_MARKER));
  assert.ok(prompt.includes("## Skill files"), "note must reference the manifest heading the model will see");
});
