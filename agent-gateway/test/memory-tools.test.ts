// memory.read / memory.write are 1:1 tool↔skill like the office tools (no
// umbrella "memory" skill), so they must appear in all three mirrors —
// BUILTIN_TOOL_NAME, TOOL_NAMES/BROKER_TO_PI_TOOL, and mapping.ts's TOOLS.
// Miss any one and an FGA grant on skill:memory.read surfaces no Pi tool, or
// the gate can't resolve the call. Mirrors office-tools.test.ts.
import { test } from "node:test";
import assert from "node:assert/strict";
import { allowedPiToolNames, BUILTIN_TOOL_NAME } from "../src/pi/session.js";
import { TOOL_NAMES, BROKER_TO_PI_TOOL } from "../src/pi/tools.js";
import { mapTool } from "../src/broker/mapping.js";
import { EffectClass } from "../gen/ts/proto/plan.js";

const MEMORY = {
  "memory.read": EffectClass.READ_ONLY,
  "memory.write": EffectClass.WRITE_LOCAL,
} as const;

test("holding both memory grants surfaces both Pi tools", () => {
  // Per-tool skills, not a "memory" umbrella: read-only memory is a valid
  // posture (grant memory.read alone), so neither grant may imply the other.
  const allowed = allowedPiToolNames(["memory.read", "memory.write"]);
  assert.ok(allowed.has("memory_read"), "memory.read grant must surface memory_read");
  assert.ok(allowed.has("memory_write"), "memory.write grant must surface memory_write");

  const readOnly = allowedPiToolNames(["memory.read"]);
  assert.ok(readOnly.has("memory_read"));
  assert.ok(!readOnly.has("memory_write"), "memory.read alone must not surface memory_write");
});

for (const [brokerId, effectClass] of Object.entries(MEMORY)) {
  const piName = BUILTIN_TOOL_NAME[brokerId];

  test(`${brokerId}: wired across every chat-surfacing map`, () => {
    assert.ok(piName, `${brokerId} missing from BUILTIN_TOOL_NAME`);
    assert.ok(TOOL_NAMES.includes(piName), `${piName} missing from TOOL_NAMES`);
    assert.strictEqual(BROKER_TO_PI_TOOL[brokerId], piName, `${brokerId} missing from BROKER_TO_PI_TOOL`);
    assert.deepStrictEqual(
      mapTool(piName),
      { toolId: brokerId, effectClass },
      `${piName} must gate-route to ${brokerId}`,
    );
    assert.ok(allowedPiToolNames([brokerId]).has(piName), `${brokerId} grant must surface ${piName}`);
  });
}
