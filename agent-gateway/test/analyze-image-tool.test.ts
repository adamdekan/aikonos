// CP6 tests: the analyze_image Pi tool (agent-gateway/src/pi/tools.ts).
//
// WHY these tests exist:
//   1. Schema: analyze_image must accept { path: string, prompt?: string } —
//      the shape the model needs to reference a workspace image and steer the
//      vision call.
//   2. Wiring: execute() must delegate to bridge.analyzeImage(path, prompt)
//      unchanged — the bridge (CP5) does the real HTTP call in the parent
//      process; the Pi tool must not duplicate or reshape that work.
//   3. Result shaping: success and failure (fail-closed, e.g. no vision
//      provider or non-image MIME) must both surface as readable text content
//      the model can act on, mirroring workflow_save's { ok, error }-handling
//      convention (ERROR: <reason> prefix, no throw).
import { test } from "node:test";
import assert from "node:assert/strict";
import { IsObject } from "typebox";

import { makeTools } from "../src/pi/tools.js";
import type { ToolDefinition } from "@earendil-works/pi-coding-agent";
import type { BridgeClientLike } from "../src/ipc/bridge-client.js";
import type { Approver } from "../src/broker/governance.js";

interface AnalyzeImageFakeBridge extends BridgeClientLike {
  analyzeImageCalls: Array<{ path: string; prompt?: string }>;
  analyzeImageResult: { ok: boolean; text?: string; error?: string };
}

function makeFakeBridge(
  analyzeImageResult: { ok: boolean; text?: string; error?: string },
): AnalyzeImageFakeBridge {
  const bridge: AnalyzeImageFakeBridge = {
    analyzeImageCalls: [],
    analyzeImageResult,

    async gate() { return { allow: true }; },
    async execute() { return { ok: true, output: null }; },
    async delegate() { return { ok: true }; },
    setApprover(_a: Approver) {},
    setToken(_t?: string) {},
    usageIdentity() { return { tenantId: "", userId: "", agentId: "" }; },
    async saveWorkflow() { return { ok: true }; },
    async runWorkflow() { return { ok: true, result: {} }; },
    async listWorkflows() { return { ok: true, items: [] }; },
    async publishWorkflow() { return { ok: true }; },
    async proposeWorkflow() { return { ok: true }; },

    async analyzeImage(path: string, prompt?: string) {
      bridge.analyzeImageCalls.push({ path, prompt });
      return bridge.analyzeImageResult;
    },
    async scheduleWorkflow() { return { ok: true }; },
    async reason() { return { ok: true, output: "" }; },
  };
  return bridge;
}

function exec(tool: ToolDefinition, toolCallId: string, params: unknown) {
  return tool.execute(toolCallId, params, undefined, undefined, undefined as never);
}

test("analyze_image: tool definition is present with { path, prompt? } schema", () => {
  const bridge = makeFakeBridge({ ok: true, text: "" });
  const tools = makeTools(bridge);
  const tool = tools.find((t) => t.name === "analyze_image");
  assert.ok(tool, "analyze_image tool must be in makeTools() output");

  assert.ok(IsObject(tool.parameters), "schema must be an object schema");
  if (!IsObject(tool.parameters)) return;
  assert.ok("path" in tool.parameters.properties, "schema must declare a path property");
  assert.ok("prompt" in tool.parameters.properties, "schema must declare a prompt property");
  assert.ok(tool.parameters.required.includes("path"), "path must be required");
  assert.ok(!tool.parameters.required.includes("prompt"), "prompt must be optional");
});

test("analyze_image: execute() delegates to bridge.analyzeImage with path and prompt", async () => {
  const bridge = makeFakeBridge({ ok: true, text: "a red bicycle" });
  const tools = makeTools(bridge);
  const tool = tools.find((t) => t.name === "analyze_image");
  assert.ok(tool, "analyze_image tool must be present");

  await exec(tool, "tc-1", { path: "references/bike.png", prompt: "what color is it?" });

  assert.equal(bridge.analyzeImageCalls.length, 1, "bridge.analyzeImage must be called once");
  const call = bridge.analyzeImageCalls[0];
  assert.ok(call, "analyzeImage call must be recorded");
  assert.equal(call.path, "references/bike.png");
  assert.equal(call.prompt, "what color is it?");
});

test("analyze_image: execute() passes prompt as undefined when omitted", async () => {
  const bridge = makeFakeBridge({ ok: true, text: "a chart" });
  const tools = makeTools(bridge);
  const tool = tools.find((t) => t.name === "analyze_image");
  assert.ok(tool, "analyze_image tool must be present");

  await exec(tool, "tc-2", { path: "references/chart.png" });

  const call = bridge.analyzeImageCalls[0];
  assert.ok(call, "analyzeImage call must be recorded");
  assert.equal(call.path, "references/chart.png");
  assert.equal(call.prompt, undefined);
});

test("analyze_image: success result surfaces bridge text as text content", async () => {
  const bridge = makeFakeBridge({ ok: true, text: "a red bicycle leaning on a wall" });
  const tools = makeTools(bridge);
  const tool = tools.find((t) => t.name === "analyze_image");
  assert.ok(tool, "analyze_image tool must be present");

  const result = await exec(tool, "tc-3", { path: "references/bike.png" });

  assert.ok(result.content.length > 0, "result must have content");
  const first = result.content[0];
  assert.ok(first, "result must have at least one content item");
  assert.equal(first.type, "text");
  assert.equal(first.text, "a red bicycle leaning on a wall");
  assert.deepEqual(result.details, { path: "references/bike.png" });
});

test("analyze_image: failure result surfaces the bridge error as text, not a throw", async () => {
  // WHY: fail-closed per the spec invariants — no vision provider assigned, or
  // a non-image MIME type, must come back as a clear error the model can relay
  // to the user, mirroring workflow_save's `ERROR: <reason>` convention rather
  // than throwing and crashing the tool-call turn.
  const bridge = makeFakeBridge({ ok: false, error: "no vision provider assigned" });
  const tools = makeTools(bridge);
  const tool = tools.find((t) => t.name === "analyze_image");
  assert.ok(tool, "analyze_image tool must be present");

  const result = await exec(tool, "tc-4", { path: "references/bike.png" });

  assert.ok(result.content.length > 0, "result must have content even on failure");
  const first = result.content[0];
  assert.ok(first, "result must have at least one content item");
  const text = first.type === "text" ? (first.text ?? "") : "";
  assert.ok(text.length > 0, "error text must be non-empty");
  assert.ok(text.includes("no vision provider assigned"), `error text must surface the bridge error, got: ${text}`);
});
