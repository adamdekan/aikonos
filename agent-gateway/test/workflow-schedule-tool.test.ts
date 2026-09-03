// CP3 tests: workflow_schedule Pi tool.
//
// WHY these tests exist:
//   1. Gating: workflow_schedule must be visible only when BOTH "workflows"
//      and "scheduler" skills are held — neither alone suffices. Also pinned
//      into TOOL_NAMES and gateSkippedTools() (bridge-direct: it calls a
//      broker RPC directly, not the Tool Proxy).
//   2. Tool→bridge wiring: execute() must forward lineageId/inputs/recurrence
//      to bridge.scheduleWorkflow exactly, and the result text must surface
//      the missing-inputs warning when present — never a hard failure.
import { test } from "node:test";
import assert from "node:assert/strict";

import { allowedPiToolNames, computeActiveToolNames } from "../src/pi/session.js";
import { makeTools, TOOL_NAMES } from "../src/pi/tools.js";
import { gateSkippedTools } from "../src/pi/gating-manifest.js";
import type { ToolDefinition } from "@earendil-works/pi-coding-agent";
import type { BridgeClientLike } from "../src/ipc/bridge-client.js";
import type { ScheduleRecurrence } from "../src/ipc/protocol.js";
import type { Approver } from "../src/broker/governance.js";

// ── Fake bridge ────────────────────────────────────────────────────────────────

interface ScheduleFakeBridge extends BridgeClientLike {
  scheduleWorkflowCalls: Array<{ lineageId: string; inputs: Record<string, string>; recurrence: ScheduleRecurrence }>;
  scheduleWorkflowResult: { ok: boolean; scheduleId?: string; missingInputs?: string[]; error?: string };
}

function makeScheduleFakeBridge(
  scheduleWorkflowResult: { ok: boolean; scheduleId?: string; missingInputs?: string[]; error?: string } = {
    ok: true,
    scheduleId: "sched-1",
    missingInputs: [],
  },
): ScheduleFakeBridge {
  const bridge: ScheduleFakeBridge = {
    scheduleWorkflowCalls: [],
    scheduleWorkflowResult,

    async gate() { return { allow: true }; },
    async execute() { return { ok: true, output: null }; },
    async delegate() { return { ok: true }; },
    setApprover(_a: Approver) {},
    setToken(_t?: string) {},
    usageIdentity() { return { tenantId: "", userId: "", agentId: "" }; },

    async saveWorkflow() { return { ok: true }; },
    async runWorkflow() { return { ok: true, result: null }; },
    async listWorkflows() { return { ok: true, items: [] }; },
    async publishWorkflow() { return { ok: true }; },
    async proposeWorkflow() { return { ok: true }; },
    async analyzeImage() { return { ok: true, text: "" }; },

    async scheduleWorkflow(lineageId: string, inputs: Record<string, string>, recurrence: ScheduleRecurrence) {
      bridge.scheduleWorkflowCalls.push({ lineageId, inputs, recurrence });
      return bridge.scheduleWorkflowResult;
    },
    async reason() { return { ok: true, output: "" }; },
  };
  return bridge;
}

function exec(tool: ToolDefinition, toolCallId: string, params: unknown) {
  return tool.execute(toolCallId, params, undefined, undefined, undefined as never);
}

// ── 1. Static registration ──────────────────────────────────────────────────────

test("workflow_schedule: present in TOOL_NAMES", () => {
  assert.ok(TOOL_NAMES.includes("workflow_schedule"), "workflow_schedule must be in TOOL_NAMES");
});

test("workflow_schedule: bridge-direct (gate-skipped) — it calls the broker RPC directly, not Tool Proxy", () => {
  assert.ok(
    gateSkippedTools().has("workflow_schedule"),
    "workflow_schedule must be in gateSkippedTools() — it never carries an approved-tools list of its own and is FGA-gated server-side",
  );
});

test("workflow_schedule: is present in makeTools() output", () => {
  const bridge = makeScheduleFakeBridge();
  const tools = makeTools(bridge);
  assert.ok(tools.find((t) => t.name === "workflow_schedule"), "workflow_schedule tool must be in makeTools() output");
});

// ── 2. Visibility gating ────────────────────────────────────────────────────────

test("gating: allowedPiToolNames requires BOTH 'workflows' AND 'scheduler' — neither alone suffices", () => {
  assert.equal(allowedPiToolNames(["workflows"]).has("workflow_schedule"), false, "'workflows' alone must not surface workflow_schedule");
  assert.equal(allowedPiToolNames(["scheduler"]).has("workflow_schedule"), false, "'scheduler' alone must not surface workflow_schedule");
  assert.equal(
    allowedPiToolNames(["workflows", "scheduler"]).has("workflow_schedule"),
    true,
    "both skills together must surface workflow_schedule",
  );
});

test("gating: computeActiveToolNames surfaces workflow_schedule only with both skills", () => {
  const withBoth = computeActiveToolNames(TOOL_NAMES, [], undefined, ["workflows", "scheduler"]);
  assert.ok(withBoth.includes("workflow_schedule"), "workflow_schedule must appear with both skills held");

  const withOnlyWorkflows = computeActiveToolNames(TOOL_NAMES, [], undefined, ["workflows"]);
  assert.ok(!withOnlyWorkflows.includes("workflow_schedule"), "workflow_schedule must NOT appear with only 'workflows'");

  const withOnlyScheduler = computeActiveToolNames(TOOL_NAMES, [], undefined, ["scheduler"]);
  assert.ok(!withOnlyScheduler.includes("workflow_schedule"), "workflow_schedule must NOT appear with only 'scheduler'");
});

// ── 3. Tool → bridge wiring ──────────────────────────────────────────────────────

test("wiring: execute() forwards lineageId, inputs, and recurrence to bridge.scheduleWorkflow", async () => {
  const bridge = makeScheduleFakeBridge();
  const tools = makeTools(bridge);
  const tool = tools.find((t) => t.name === "workflow_schedule");
  assert.ok(tool, "workflow_schedule tool must be present");

  await exec(tool, "tc-1", {
    lineageId: "lin-1",
    kind: "cron",
    cronExpr: "0 8 * * 1",
    inputs: { region: "eu" },
  });

  assert.equal(bridge.scheduleWorkflowCalls.length, 1);
  const call = bridge.scheduleWorkflowCalls[0];
  assert.equal(call.lineageId, "lin-1");
  assert.deepEqual(call.inputs, { region: "eu" });
  assert.deepEqual(call.recurrence, { kind: "cron", cronExpr: "0 8 * * 1", runAt: undefined });
});

test("wiring: omitted inputs default to {}", async () => {
  const bridge = makeScheduleFakeBridge();
  const tools = makeTools(bridge);
  const tool = tools.find((t) => t.name === "workflow_schedule");
  assert.ok(tool, "workflow_schedule tool must be present");

  await exec(tool, "tc-2", { lineageId: "lin-2", kind: "once", runAt: "2099-01-01T00:00:00Z" });

  assert.deepEqual(bridge.scheduleWorkflowCalls[0].inputs, {});
});

// ── 4. Result text formatting ────────────────────────────────────────────────────

test("result text: success with no missing inputs has no warning", async () => {
  const bridge = makeScheduleFakeBridge({ ok: true, scheduleId: "sched-1", missingInputs: [] });
  const tools = makeTools(bridge);
  const tool = tools.find((t) => t.name === "workflow_schedule");
  assert.ok(tool, "workflow_schedule tool must be present");

  const result = await exec(tool, "tc-3", { lineageId: "lin-1", kind: "cron", cronExpr: "0 8 * * 1" });
  const first = result.content[0];
  const text = first && first.type === "text" ? (first.text ?? "") : "";

  assert.ok(text.includes("sched-1"), `result text must include the schedule id; got: ${text}`);
  assert.ok(!text.includes("WARNING"), `result text must have no warning when nothing is missing; got: ${text}`);
});

test("result text: success with missing inputs still succeeds and carries a warning naming them — never a hard failure", async () => {
  const bridge = makeScheduleFakeBridge({ ok: true, scheduleId: "sched-2", missingInputs: ["region", "since"] });
  const tools = makeTools(bridge);
  const tool = tools.find((t) => t.name === "workflow_schedule");
  assert.ok(tool, "workflow_schedule tool must be present");

  const result = await exec(tool, "tc-4", { lineageId: "lin-1", kind: "cron", cronExpr: "0 8 * * 1" });
  const first = result.content[0];
  const text = first && first.type === "text" ? (first.text ?? "") : "";

  assert.ok(!text.startsWith("ERROR:"), `missing inputs must never be a hard failure; got: ${text}`);
  assert.ok(text.includes("sched-2"), "success text must still include the schedule id");
  assert.ok(text.includes("region") && text.includes("since"), `warning must name every missing key; got: ${text}`);
});

test("result text: bridge failure (e.g. no-token) surfaces as ERROR:", async () => {
  const bridge = makeScheduleFakeBridge({ ok: false, error: "workflow_schedule requires an interactive chat session" });
  const tools = makeTools(bridge);
  const tool = tools.find((t) => t.name === "workflow_schedule");
  assert.ok(tool, "workflow_schedule tool must be present");

  const result = await exec(tool, "tc-5", { lineageId: "lin-1", kind: "cron", cronExpr: "0 8 * * 1" });
  const first = result.content[0];
  const text = first && first.type === "text" ? (first.text ?? "") : "";

  assert.ok(text.startsWith("ERROR:"), `bridge failure must surface as ERROR:; got: ${text}`);
});
