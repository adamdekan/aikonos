// TDD tests for workflowDefFromToolParams (CP5 bug fix).
//
// WHY these tests exist: the workflow_save Pi tool passes flat params to
// GovernanceBridge.saveWorkflow, which must wrap them into a canonical
// broker-valid WorkflowDef before calling SaveWorkflow RPC. Without the
// envelope (apiVersion / kind / metadata) the broker rejects with
// "apiVersion must be aikonos.com/v1". These tests drive the helper that
// produces the canonical envelope from the flat tool-call shape.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  workflowDefFromToolParams,
  type WorkflowDef,
} from "../src/workflow/author.js";

// ── Test 1: full params → valid canonical envelope ────────────────────────────

test("workflowDefFromToolParams: full params produce a broker-valid canonical WorkflowDef", () => {
  // WHY: the workflow_save tool params must be wrapped into a canonical document
  // that the broker accepts: apiVersion, kind, metadata envelope must all be present.
  const params = {
    name: "demo-fetch",
    description: "fetch a URL on a schedule",
    steps: [{ skill: "web.fetch", args: { url: "https://example.com" } }],
    inputs: [{ name: "since", default: "-7d" }],
  };

  const wf: WorkflowDef = workflowDefFromToolParams(params);

  assert.equal(wf.apiVersion, "aikonos.com/v1");
  assert.equal(wf.kind, "Workflow");
  assert.equal(wf.metadata.name, "demo-fetch");
  assert.equal(wf.metadata.visibility.kind, "private");
});

// ── Test 2: steps are mapped correctly ───────────────────────────────────────

test("workflowDefFromToolParams: steps are mapped from flat params", () => {
  // WHY: every step in the flat params must appear as a WorkflowStep with
  // skill and args preserved — this is the execution contract.
  const params = {
    name: "demo-fetch",
    steps: [{ skill: "web.fetch", args: { url: "https://example.com" } }],
    inputs: [{ name: "since", default: "-7d" }],
  };

  const wf: WorkflowDef = workflowDefFromToolParams(params);

  assert.equal(wf.steps.length, 1);
  assert.equal(wf.steps[0].skill, "web.fetch");
  assert.deepEqual(wf.steps[0].args, { url: "https://example.com" });
});

// ── Test 3: inputs are mapped correctly ──────────────────────────────────────

test("workflowDefFromToolParams: inputs are mapped from flat params", () => {
  // WHY: inputs carry the default values used to resolve ${inputs.*} tokens
  // at run time — they must round-trip faithfully.
  const params = {
    name: "demo-fetch",
    steps: [{ skill: "web.fetch", args: {} }],
    inputs: [{ name: "since", default: "-7d" }],
  };

  const wf: WorkflowDef = workflowDefFromToolParams(params);

  assert.equal((wf.inputs ?? []).length, 1);
  assert.equal(wf.inputs?.[0].name, "since");
  assert.equal(wf.inputs?.[0].default, "-7d");
});

// ── Test 4: description forwarded when present ────────────────────────────────

test("workflowDefFromToolParams: description forwarded into metadata when present", () => {
  // WHY: description surfaces in the workflow library UI; it must not be silently
  // dropped when the tool caller provides it.
  const params = {
    name: "my-wf",
    description: "does things",
    steps: [{ skill: "doc.write", args: {} }],
  };

  const wf: WorkflowDef = workflowDefFromToolParams(params);

  assert.equal(wf.metadata.description, "does things");
});

// ── Test 5: description omitted when absent ───────────────────────────────────

test("workflowDefFromToolParams: description omitted from metadata when not in params", () => {
  // WHY: optional fields must not be set to empty string — the webui renders
  // whatever metadata.description contains.
  const params = {
    name: "my-wf",
    steps: [{ skill: "doc.write", args: {} }],
  };

  const wf: WorkflowDef = workflowDefFromToolParams(params);

  assert.equal(wf.metadata.description, undefined);
});

// ── Test 6: minimal input (name + steps only) → valid envelope ───────────────

test("workflowDefFromToolParams: minimal params (name + steps only) still yield a valid envelope", () => {
  // WHY: inputs is optional in the tool schema. The function must not crash or
  // produce an invalid document when the LLM omits optional fields.
  const params = {
    name: "minimal",
    steps: [{ skill: "web.fetch", args: {} }],
  };

  const wf: WorkflowDef = workflowDefFromToolParams(params);

  assert.equal(wf.apiVersion, "aikonos.com/v1");
  assert.equal(wf.kind, "Workflow");
  assert.equal(wf.metadata.name, "minimal");
  assert.equal(wf.metadata.visibility.kind, "private");
  assert.deepEqual(wf.inputs ?? [], []);
});

// ── Test 7: step with no args → args defaults to {} ──────────────────────────

test("workflowDefFromToolParams: step without args field defaults args to {}", () => {
  // WHY: args is optional in the tool schema but the run driver iterates over it;
  // the step must always carry an args object, never undefined.
  const params = {
    name: "no-args-wf",
    steps: [{ skill: "web.fetch" }],
  };

  const wf: WorkflowDef = workflowDefFromToolParams(params);

  assert.deepEqual(wf.steps[0].args, {});
});

// ── Test 8: name absent → empty string (broker rejects with clear error) ──────

test("workflowDefFromToolParams: absent name maps to empty string (broker will reject clearly)", () => {
  // WHY: the broker's own validation produces a clear message for empty metadata.name.
  // Letting it reach the broker is preferable to a silent fallback name that would
  // produce a confusing workflow entry.
  const params = {
    steps: [{ skill: "web.fetch", args: {} }],
  };

  const wf: WorkflowDef = workflowDefFromToolParams(params);

  assert.equal(wf.metadata.name, "");
});

// ── Test 9: visibility is always private ────────────────────────────────────

test("workflowDefFromToolParams: visibility is always private regardless of input", () => {
  // WHY: the workflow_save tool always creates a private workflow. Publishing is
  // a separate workflow_publish operation with its own FGA gate.
  const params = {
    name: "test-wf",
    steps: [{ skill: "web.fetch", args: {} }],
  };

  const wf: WorkflowDef = workflowDefFromToolParams(params);

  assert.equal(wf.metadata.visibility.kind, "private");
  assert.equal(wf.metadata.visibility.groups, undefined);
});

// ── Reason step (CP-R2) ───────────────────────────────────────────────────────

test("workflowDefFromToolParams: mixed tool + reason steps round-trip", () => {
  // WHY: the core CP-R2 contract — a definition mixing tool and reason steps
  // must pass through unchanged, with the reason step carrying instruction and
  // no skill/args.
  const params = {
    name: "ip-alert",
    steps: [
      { skill: "doc.read", args: { path: "registry.csv" } },
      {
        kind: "reason",
        instruction: "Find the row whose CIDR contains ${inputs.ip}: ${steps.0.output}",
        output_schema: { type: "object", properties: { email: { type: "string" } } },
      },
      { kind: "tool", skill: "doc.write", args: { path: "alert.txt" } },
    ],
  };

  const wf: WorkflowDef = workflowDefFromToolParams(params);

  assert.equal(wf.steps.length, 3);
  assert.equal(wf.steps[0].kind, "tool");
  assert.equal(wf.steps[0].skill, "doc.read");
  assert.equal(wf.steps[1].kind, "reason");
  assert.equal(
    wf.steps[1].instruction,
    "Find the row whose CIDR contains ${inputs.ip}: ${steps.0.output}",
  );
  assert.deepEqual(wf.steps[1].output_schema, {
    type: "object",
    properties: { email: { type: "string" } },
  });
  assert.equal(wf.steps[1].skill, "", "a reason step carries no skill");
  assert.equal(wf.steps[2].kind, "tool");
  assert.equal(wf.steps[2].skill, "doc.write");
});

test("workflowDefFromToolParams: rejects an invalid kind, naming the step index and field", () => {
  const params = {
    name: "bad-kind-wf",
    steps: [{ kind: "loop", skill: "doc.read", args: {} }],
  };

  assert.throws(
    () => workflowDefFromToolParams(params),
    /step 0:.*kind/,
    "error must name the step index and the kind field",
  );
});

test("workflowDefFromToolParams: reason step missing instruction is rejected, naming the field", () => {
  const params = {
    name: "missing-instruction-wf",
    steps: [{ kind: "reason" }],
  };

  assert.throws(
    () => workflowDefFromToolParams(params),
    /step 0:.*instruction/,
    "error must name the step index and the instruction field",
  );
});

test("workflowDefFromToolParams: reason step with a skill is rejected", () => {
  const params = {
    name: "reason-with-skill-wf",
    steps: [{ kind: "reason", instruction: "do something", skill: "doc.read" }],
  };

  assert.throws(
    () => workflowDefFromToolParams(params),
    /step 0:.*skill/,
    "error must name the step index and the skill field",
  );
});

test("workflowDefFromToolParams: tool step with an instruction is rejected", () => {
  const params = {
    name: "tool-with-instruction-wf",
    steps: [{ skill: "doc.read", instruction: "do something" }],
  };

  assert.throws(
    () => workflowDefFromToolParams(params),
    /step 0:.*instruction/,
    "error must name the step index and the instruction field",
  );
});

test("workflowDefFromToolParams: tool step missing a skill is rejected", () => {
  const params = {
    name: "tool-missing-skill-wf",
    steps: [{ args: {} }],
  };

  assert.throws(
    () => workflowDefFromToolParams(params),
    /step 0:.*skill/,
    "error must name the step index and the skill field",
  );
});

test("workflowDefFromToolParams: output_schema that is an array is rejected, naming the field", () => {
  const params = {
    name: "bad-output-schema-array-wf",
    steps: [
      { kind: "reason", instruction: "do something", output_schema: ["not", "an", "object"] },
    ],
  };

  assert.throws(
    () => workflowDefFromToolParams(params),
    /step 0:.*output_schema/,
    "error must name the step index and the output_schema field",
  );
});

test("workflowDefFromToolParams: output_schema that is a string is rejected, naming the field", () => {
  const params = {
    name: "bad-output-schema-string-wf",
    steps: [{ kind: "reason", instruction: "do something", output_schema: "not-an-object" }],
  };

  assert.throws(
    () => workflowDefFromToolParams(params),
    /step 0:.*output_schema/,
    "error must name the step index and the output_schema field",
  );
});

test("workflowDefFromToolParams: tool step args that is not a plain object is rejected, naming the field", () => {
  const params = {
    name: "bad-args-wf",
    steps: [{ skill: "doc.read", args: "not-an-object" }],
  };

  assert.throws(
    () => workflowDefFromToolParams(params),
    /step 0:.*args/,
    "error must name the step index and the args field",
  );
});

test("workflowDefFromToolParams: reason step with args present, even empty, is rejected", () => {
  const params = {
    name: "reason-with-empty-args-wf",
    steps: [{ kind: "reason", instruction: "do something", args: {} }],
  };

  assert.throws(
    () => workflowDefFromToolParams(params),
    /step 0:.*args.*absent.*reason step/,
    "error must name the step index and require args be absent on a reason step",
  );
});
