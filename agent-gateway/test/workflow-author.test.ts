// TDD tests for authorWorkflow (CP3 — agent authoring).
//
// WHY these tests exist: authorWorkflow is a pure deterministic function that
// maps a session record (prompt + tool calls) onto a WorkflowDef. Tests drive it
// with a golden fixture without any network/broker involvement.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { authorWorkflow, type WorkflowDef } from "../src/workflow/author.js";
import type {
  SessionToolEntry,
} from "../src/scheduler/session-record.js";

const __dirname = dirname(fileURLToPath(import.meta.url));

// ── Load the golden fixture ───────────────────────────────────────────────────

interface SessionFixture {
  prompt: string;
  tools: SessionToolEntry[];
}

const fixture = JSON.parse(
  readFileSync(join(__dirname, "testdata/session-fixture.json"), "utf8"),
) as SessionFixture;

// ── Test 1: happy path — valid Workflow from fixture ─────────────────────────

test("authorWorkflow: produces a schema-valid Workflow from the golden fixture", () => {
  // WHY: the primary contract — a recorded session must map to a valid WorkflowDef
  // with apiVersion, kind, metadata.name, and metadata.visibility.kind set.
  const wf: WorkflowDef = authorWorkflow(fixture.prompt, fixture.tools);

  assert.equal(wf.apiVersion, "aikonos.com/v1");
  assert.equal(wf.kind, "Workflow");
  assert.ok(wf.metadata.name.length > 0, "metadata.name must not be empty");
  assert.equal(wf.metadata.visibility.kind, "private");
});

// ── Test 2: steps mirror tool calls in order ──────────────────────────────────

test("authorWorkflow: steps match tool calls in order", () => {
  // WHY: the step sequence is the whole point of authoring — every tool call
  // the agent made must be preserved as a step, in order.
  const wf: WorkflowDef = authorWorkflow(fixture.prompt, fixture.tools);

  assert.equal(wf.steps.length, fixture.tools.length);
  for (let i = 0; i < fixture.tools.length; i++) {
    assert.equal(wf.steps[i].skill, fixture.tools[i].name);
  }
});

// ── Test 3: at least one arg is templated as ${inputs.*} ─────────────────────

test("authorWorkflow: at least one arg is templated with a matching inputs entry", () => {
  // WHY: variable detection is the core value — args that echo the user prompt
  // or look like dates must be replaced with ${inputs.<name>} so the workflow
  // can be re-run with different values.
  const wf: WorkflowDef = authorWorkflow(fixture.prompt, fixture.tools);

  // Find all ${inputs.*} placeholders across all steps.
  const allArgs = wf.steps.flatMap((s) => Object.values(s.args ?? {}));
  const templated = allArgs.filter(
    (v) => typeof v === "string" && v.startsWith("${inputs."),
  );
  assert.ok(templated.length >= 1, "at least one arg must be templated as ${inputs.*}");

  // Every templated placeholder must have a matching inputs entry.
  for (const placeholder of templated) {
    const name = (placeholder as string).slice("${inputs.".length, -1);
    const found = wf.inputs?.some((inp) => inp.name === name);
    assert.ok(found, `inputs entry missing for placeholder "${placeholder}"`);
  }
});

// ── Test 4: name derives from the user prompt ─────────────────────────────────

test("authorWorkflow: metadata.name derives from the user prompt", () => {
  // WHY: the title/name is the human-facing identifier for the workflow in the
  // library — it must not be empty or generic.
  const wf: WorkflowDef = authorWorkflow(fixture.prompt, fixture.tools);
  // The name should contain words from the prompt; at minimum it is non-empty
  // and differs from the default empty string.
  assert.ok(wf.metadata.name.length > 0);
  // Should be derived from the prompt (first few words, same as titleFrom).
  const promptWords = fixture.prompt.trim().split(/\s+/).slice(0, 6).join(" ");
  const truncated = promptWords.length > 40 ? promptWords.slice(0, 40) : promptWords;
  assert.equal(wf.metadata.name, truncated);
});

// ── Test 5: empty tools → zero steps, no inputs ──────────────────────────────

test("authorWorkflow: empty tool list → zero steps and empty inputs", () => {
  // WHY: a session with no tool calls produces a workflow with an empty step
  // sequence (still valid — metadata is required, steps is optional in the schema).
  const wf: WorkflowDef = authorWorkflow("just a chat message", []);

  assert.equal(wf.steps.length, 0);
  assert.equal((wf.inputs ?? []).length, 0);
});
