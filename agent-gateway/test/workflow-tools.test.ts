// CP5b tests: workflow Pi tools (workflow_save / workflow_run / workflow_list)
//
// WHY these tests exist:
//   1. Gating: a user WITHOUT the "workflows" skill must not see the three tool
//      names; one WITH it must see all three. This is the deny-by-default
//      invariant for the skill gate.
//   2. Tool→bridge wiring: each Pi tool's execute() must call the correct
//      bridge method with exactly the right arguments. A fake bridge records calls
//      so we can assert the mapping without any broker/process involvement.
//
// These tests are intentionally narrow (pure unit tests on pure data-transform
// and thin bridge-call paths). Integration coverage (IPC round-trip, south RPC)
// is provided by bridge-server.test.ts and governance tests.
import { test } from "node:test";
import assert from "node:assert/strict";

import { allowedPiToolNames, computeActiveToolNames } from "../src/pi/session.js";
import { makeTools, TOOL_NAMES } from "../src/pi/tools.js";
import type { ToolDefinition } from "@earendil-works/pi-coding-agent";
import type { BridgeClientLike } from "../src/ipc/bridge-client.js";
import type { Approver } from "../src/broker/governance.js";

// ── Fake bridge ────────────────────────────────────────────────────────────────

interface WorkflowFakeBridge extends BridgeClientLike {
  saveWorkflowCalls: Array<{ def: Record<string, unknown> }>;
  runWorkflowCalls: Array<{ lineageId: string; inputs: Record<string, string> }>;
  listWorkflowsCalls: number;
}

function makeWorkflowFakeBridge(): WorkflowFakeBridge {
  const bridge: WorkflowFakeBridge = {
    saveWorkflowCalls: [],
    runWorkflowCalls: [],
    listWorkflowsCalls: 0,

    async gate() { return { allow: true }; },
    async execute() { return { ok: true, output: null }; },
    async delegate() { return { ok: true }; },
    setApprover(_a: Approver) {},
    setToken(_t?: string) {},
    usageIdentity() { return { tenantId: "", userId: "", agentId: "" }; },

    async saveWorkflow(def: Record<string, unknown>) {
      bridge.saveWorkflowCalls.push({ def });
      return { ok: true, lineageId: "lineage-1", workflowId: "wf-1", version: 1 };
    },
    async runWorkflow(lineageId: string, inputs: Record<string, string>) {
      bridge.runWorkflowCalls.push({ lineageId, inputs });
      return { ok: true, result: { halted: false, steps: [] } };
    },
    async listWorkflows() {
      bridge.listWorkflowsCalls++;
      return { ok: true, items: [] };
    },
    async publishWorkflow() {
      return { ok: true };
    },
    async proposeWorkflow() {
      return { ok: true };
    },
    async analyzeImage() {
      return { ok: true, text: "" };
    },
    async scheduleWorkflow() {
      return { ok: true };
    },
    async reason() {
      return { ok: true, output: "" };
    },
  };
  return bridge;
}

// ── Gating tests ───────────────────────────────────────────────────────────────

const WORKFLOW_TOOL_NAMES = ["workflow_save", "workflow_run", "workflow_list"];

test("CP5b gating: allowedPiToolNames WITHOUT 'workflows' skill does not include workflow tools", () => {
  // WHY: deny-by-default. A user with no workflows skill must not be offered
  // workflow_save/run/list — they are not in the broker's skill vocabulary for
  // this user, so the tools must be absent from the session.
  const allowed = allowedPiToolNames(["web.fetch", "doc.write"]);
  for (const name of WORKFLOW_TOOL_NAMES) {
    assert.equal(
      allowed.has(name),
      false,
      `${name} must NOT be in allowed set without 'workflows' skill`,
    );
  }
});

test("CP5b gating: allowedPiToolNames WITH 'workflows' skill includes all three workflow tools", () => {
  // WHY: the positive path. Holding the 'workflows' skill is the only gate.
  // All three tools must be surfaced so the agent can save, run, and list.
  const allowed = allowedPiToolNames(["workflows"]);
  for (const name of WORKFLOW_TOOL_NAMES) {
    assert.equal(
      allowed.has(name),
      true,
      `${name} must be in allowed set when 'workflows' skill is held`,
    );
  }
  // delegate must also be present (always-included invariant)
  assert.equal(allowed.has("delegate"), true, "delegate must always be present");
});

test("CP5b gating: computeActiveToolNames with workflows skill includes all three workflow tools", () => {
  // WHY: computeActiveToolNames is what buildSession uses to build the final
  // tool name list. It must propagate the skill → tool name mapping correctly.
  const active = computeActiveToolNames(TOOL_NAMES, [], undefined, ["workflows"]);
  for (const name of WORKFLOW_TOOL_NAMES) {
    assert.equal(
      active.includes(name),
      true,
      `${name} must appear in computeActiveToolNames result with 'workflows' skill`,
    );
  }
});

test("CP5b gating: computeActiveToolNames WITHOUT workflows skill excludes workflow tools", () => {
  // WHY: the deny side of the same invariant — computeActiveToolNames must not
  // leak workflow tools to users who lack the skill.
  const active = computeActiveToolNames(TOOL_NAMES, [], undefined, ["web.fetch"]);
  for (const name of WORKFLOW_TOOL_NAMES) {
    assert.equal(
      active.includes(name),
      false,
      `${name} must NOT appear in computeActiveToolNames result without 'workflows' skill`,
    );
  }
});

test("CP5b gating: TOOL_NAMES includes all three workflow tool names", () => {
  // WHY: TOOL_NAMES is the authoritative list fed to computeActiveToolNames as
  // allStaticNames. If a name is absent here it can never appear in any session.
  for (const name of WORKFLOW_TOOL_NAMES) {
    assert.equal(
      TOOL_NAMES.includes(name),
      true,
      `${name} must be present in TOOL_NAMES`,
    );
  }
});

// ── Tool → bridge wiring tests ─────────────────────────────────────────────────

// exec() invokes a ToolDefinition's execute() with only the two arguments the
// workflow tools actually use (toolCallId + params). signal/onUpdate/ctx are
// all optional at runtime; undefined is safe for these pure-transform tools.
// The helper exists to avoid repeating the undefined fillers at every call site.
function exec(tool: ToolDefinition, toolCallId: string, params: unknown) {
  return tool.execute(toolCallId, params, undefined, undefined, undefined as never);
}

test("CP5b wiring: workflow_save execute() calls bridge.saveWorkflow with the tool params", async () => {
  // WHY: the tool definition must pass the full WorkflowDef params to the bridge,
  // not a subset. A mismatch here means the broker receives an incomplete definition.
  const bridge = makeWorkflowFakeBridge();
  const tools = makeTools(bridge);
  const saveTool = tools.find((t) => t.name === "workflow_save");
  assert.ok(saveTool, "workflow_save tool must be in makeTools() output");

  const def = {
    name: "my-workflow",
    description: "does stuff",
    steps: [{ skill: "web.fetch", args: { url: "https://example.com" } }],
    inputs: [{ name: "query", default: "hello" }],
  };

  await exec(saveTool, "tc-save-1", def);

  assert.equal(bridge.saveWorkflowCalls.length, 1, "bridge.saveWorkflow must be called once");
  const call = bridge.saveWorkflowCalls[0];
  assert.ok(call, "saveWorkflow call must be recorded");
  assert.equal(call.def.name, "my-workflow");
  assert.deepEqual(call.def.steps, def.steps);
  assert.deepEqual(call.def.inputs, def.inputs);
});

test("CP5b wiring: workflow_run execute() calls bridge.runWorkflow with lineageId and inputs", async () => {
  // WHY: the run tool must forward lineageId and inputs exactly — the broker
  // uses lineageId to look up the workflow and inputs to resolve ${inputs.*} tokens.
  const bridge = makeWorkflowFakeBridge();
  const tools = makeTools(bridge);
  const runTool = tools.find((t) => t.name === "workflow_run");
  assert.ok(runTool, "workflow_run tool must be in makeTools() output");

  await exec(runTool, "tc-run-1", { lineageId: "lineage-abc", inputs: { query: "test" } });

  assert.equal(bridge.runWorkflowCalls.length, 1, "bridge.runWorkflow must be called once");
  const call = bridge.runWorkflowCalls[0];
  assert.ok(call, "runWorkflow call must be recorded");
  assert.equal(call.lineageId, "lineage-abc");
  assert.deepEqual(call.inputs, { query: "test" });
});

test("CP5b wiring: workflow_run execute() passes empty inputs when omitted", async () => {
  // WHY: inputs is optional in the tool schema. The bridge must receive {} not
  // undefined so the run driver can iterate over it without a null check.
  const bridge = makeWorkflowFakeBridge();
  const tools = makeTools(bridge);
  const runTool = tools.find((t) => t.name === "workflow_run");
  assert.ok(runTool, "workflow_run tool must be present");

  await exec(runTool, "tc-run-2", { lineageId: "lineage-xyz" });

  const call = bridge.runWorkflowCalls[0];
  assert.ok(call, "runWorkflow call must be recorded");
  assert.equal(call.lineageId, "lineage-xyz");
  assert.deepEqual(call.inputs, {}, "omitted inputs must default to {}");
});

test("CP5b wiring: workflow_list execute() calls bridge.listWorkflows", async () => {
  // WHY: the list tool must call listWorkflows exactly once with no extra args.
  const bridge = makeWorkflowFakeBridge();
  const tools = makeTools(bridge);
  const listTool = tools.find((t) => t.name === "workflow_list");
  assert.ok(listTool, "workflow_list tool must be in makeTools() output");

  await exec(listTool, "tc-list-1", {});

  assert.equal(bridge.listWorkflowsCalls, 1, "bridge.listWorkflows must be called once");
});

test("CP5b wiring: workflow_save returns formatted text result on success", async () => {
  // WHY: the tool result text is what the LLM reads back. It must contain enough
  // info for the model to proceed (lineageId at minimum).
  const bridge = makeWorkflowFakeBridge();
  const tools = makeTools(bridge);
  const saveTool = tools.find((t) => t.name === "workflow_save");
  assert.ok(saveTool, "workflow_save tool must be present");

  const result = await exec(saveTool, "tc-save-ok", {
    name: "test-wf",
    steps: [{ skill: "web.fetch", args: {} }],
  });

  assert.ok(result.content.length > 0, "result must have content");
  const first = result.content[0];
  assert.ok(first, "result must have at least one content item");
  const text = first.type === "text" ? (first.text ?? "") : "";
  assert.ok(text.length > 0, "result text must be non-empty");
  assert.ok(!text.startsWith("ERROR:"), `result must not be an error on success, got: ${text}`);
});

test("CP5b wiring: workflow_run returns formatted text result on success", async () => {
  const bridge = makeWorkflowFakeBridge();
  const tools = makeTools(bridge);
  const runTool = tools.find((t) => t.name === "workflow_run");
  assert.ok(runTool, "workflow_run tool must be present");

  const result = await exec(runTool, "tc-run-ok", {
    lineageId: "lineage-1",
    inputs: {},
  });

  assert.ok(result.content.length > 0, "result must have content");
  const first = result.content[0];
  assert.ok(first, "result must have at least one content item");
  const text = first.type === "text" ? (first.text ?? "") : "";
  assert.ok(!text.startsWith("ERROR:"), `result must not be an error on success, got: ${text}`);
});
