// Pins web_search across every chat-surfacing map,
// same pattern as office-tools.test.ts's regression coverage: a granted
// skill:web.search must surface exactly the web_search Pi tool in chat, and
// gate-time mapping must route it to web.search/READ_ONLY.
import { test } from "node:test";
import assert from "node:assert/strict";
import { allowedPiToolNames, BUILTIN_TOOL_NAME } from "../src/pi/session.js";
import { TOOL_NAMES, BROKER_TO_PI_TOOL, makeTools } from "../src/pi/tools.js";
import { mapTool } from "../src/broker/mapping.js";
import { GATING_MANIFEST } from "../src/pi/gating-manifest.js";
import { EffectClass } from "../gen/ts/proto/plan.js";

test("granting skill:web.search surfaces exactly the web_search Pi tool in chat", () => {
  const allowed = allowedPiToolNames(["web.search"]);
  assert.ok(allowed.has("web_search"), "web.search grant must surface web_search");
});

test("web.search: wired across every chat-surfacing map", () => {
  const piName = BUILTIN_TOOL_NAME["web.search"];
  assert.strictEqual(piName, "web_search", "missing from BUILTIN_TOOL_NAME");
  assert.ok(TOOL_NAMES.includes("web_search"), "missing from TOOL_NAMES");
  assert.strictEqual(BROKER_TO_PI_TOOL["web.search"], "web_search", "missing from BROKER_TO_PI_TOOL");
  assert.deepStrictEqual(
    mapTool("web_search"),
    { toolId: "web.search", effectClass: EffectClass.READ_ONLY },
    "web_search must gate-route to web.search/READ_ONLY",
  );
});

test("web_search is declared 'gated' in the gating manifest, ordinary JIT-plan path", () => {
  assert.strictEqual(GATING_MANIFEST.web_search?.model, "gated");
  assert.match(GATING_MANIFEST.web_search?.authz ?? "", /skill:web\.search/);
});

test("web_search Pi tool definition exists with query required, count optional", () => {
  const tools = makeTools({ execute: async () => ({ ok: true, output: "" }) } as never);
  const def = tools.find((t) => t.name === "web_search");
  assert.ok(def, "web_search tool definition missing from makeTools()");
  assert.match(def!.description, /web_fetch/, "description must steer loading result content via web_fetch");
});
