// CP-R5: workflow_save / workflow_propose step param schema documents both
// step kinds (tool / reason).
//
// WHY this test exists:
//   Once the driver (CP-R4) and authoring guard (CP-R2) accept reason steps,
//   the Pi tool schema the model actually sees must advertise `kind`,
//   `instruction`, and `output_schema` on step items — otherwise the model
//   has no way to discover the reason-step shape and keeps inventing
//   nonexistent skills for computation/synthesis work.
import { test } from "node:test";
import assert from "node:assert/strict";

import { makeTools } from "../src/pi/tools.js";
import type { BridgeClientLike } from "../src/ipc/bridge-client.js";
import type { Approver } from "../src/broker/governance.js";

function makeFakeBridge(): BridgeClientLike {
  return {
    async gate() { return { allow: true }; },
    async execute() { return { ok: true, output: null }; },
    async delegate() { return { ok: true }; },
    setApprover(_a: Approver) {},
    setToken(_t?: string) {},
    usageIdentity() { return { tenantId: "", userId: "", agentId: "" }; },
    async saveWorkflow() { return { ok: true, lineageId: "l", workflowId: "w", version: 1 }; },
    async runWorkflow() { return { ok: true, result: { halted: false, steps: [] } }; },
    async listWorkflows() { return { ok: true, items: [] }; },
    async publishWorkflow() { return { ok: true }; },
    async proposeWorkflow() { return { ok: true }; },
    async analyzeImage() { return { ok: true, text: "" }; },
    async scheduleWorkflow() { return { ok: true }; },
    async reason() { return { ok: true, output: "" }; },
  };
}

function stepPropertiesOf(toolName: string): Record<string, unknown> {
  const tools = makeTools(makeFakeBridge());
  const tool = tools.find((t) => t.name === toolName);
  assert.ok(tool, `${toolName} tool must be in makeTools() output`);
  const schema = tool.parameters as { properties?: { steps?: { items?: { properties?: Record<string, unknown> } } } };
  const stepProps = schema.properties?.steps?.items?.properties;
  assert.ok(stepProps, `${toolName} steps schema must have item properties`);
  return stepProps;
}

for (const toolName of ["workflow_save", "workflow_propose"]) {
  test(`${toolName} step schema documents kind/instruction/output_schema`, () => {
    const stepProps = stepPropertiesOf(toolName);
    assert.ok(stepProps.kind, "step schema must include 'kind'");
    assert.ok(stepProps.instruction, "step schema must include 'instruction'");
    assert.ok(stepProps.output_schema, "step schema must include 'output_schema'");

    const kindDesc = JSON.stringify(stepProps.kind).toLowerCase();
    assert.ok(kindDesc.includes("tool"), "kind description must mention 'tool'");
    assert.ok(kindDesc.includes("reason"), "kind description must mention 'reason'");

    const instructionDesc = JSON.stringify(stepProps.instruction).toLowerCase();
    assert.ok(instructionDesc.includes("reason"), "instruction description must mention reason steps");

    const schemaDesc = JSON.stringify(stepProps.output_schema).toLowerCase();
    assert.ok(schemaDesc.includes("reason"), "output_schema description must mention reason steps");

    // skill's description must state the per-kind rule (required for tool steps).
    const skillDesc = JSON.stringify(stepProps.skill).toLowerCase();
    assert.ok(skillDesc.includes("tool"), "skill description must state it applies to tool steps");
  });
}
