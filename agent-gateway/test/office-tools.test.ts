// Regression: the 15 office tools were wired into the broker (toolproxy
// plugins, toolregistry, FGA seed, skill bundles) but never into the gateway's
// Pi tool surface, so an FGA grant on skill:xlsx.extract mapped to no Pi tool
// and the tool never appeared in chat. These tests pin every office tool
// through the two chat-surfacing paths: allowedPiToolNames (skill → Pi name)
// and mapTool (gate-time Pi name → broker id + effect class).
import { test } from "node:test";
import assert from "node:assert/strict";
import { allowedPiToolNames, BUILTIN_TOOL_NAME } from "../src/pi/session.js";
import { TOOL_NAMES, BROKER_TO_PI_TOOL } from "../src/pi/tools.js";
import { mapTool } from "../src/broker/mapping.js";
import { EffectClass } from "../gen/ts/proto/plan.js";

const OFFICE = {
  "docx.create": EffectClass.WRITE_LOCAL,
  "docx.edit": EffectClass.WRITE_LOCAL,
  "docx.extract": EffectClass.READ_ONLY,
  "xlsx.create": EffectClass.WRITE_LOCAL,
  "xlsx.edit": EffectClass.WRITE_LOCAL,
  "xlsx.extract": EffectClass.READ_ONLY,
  "xlsx.recalc": EffectClass.WRITE_LOCAL,
  "pptx.create": EffectClass.WRITE_LOCAL,
  "pptx.edit": EffectClass.WRITE_LOCAL,
  "pptx.extract": EffectClass.READ_ONLY,
  "pptx.thumbnail": EffectClass.WRITE_LOCAL,
  "pdf.create": EffectClass.WRITE_LOCAL,
  "pdf.transform": EffectClass.WRITE_LOCAL,
  "pdf.extract": EffectClass.READ_ONLY,
  "office.convert": EffectClass.WRITE_LOCAL,
} as const;

test("granting an office skill surfaces exactly its Pi tool in chat", () => {
  // The reported bug: office-workers granted skill:xlsx.extract saw no tool.
  const allowed = allowedPiToolNames(["xlsx.extract"]);
  assert.ok(allowed.has("xlsx_extract"), "xlsx.extract grant must surface xlsx_extract");
});

for (const [brokerId, effectClass] of Object.entries(OFFICE)) {
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
