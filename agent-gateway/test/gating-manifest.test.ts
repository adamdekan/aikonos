// Conformance tests for the declarative per-tool gating manifest (F4, CP1).
//
// WHY these tests exist: the manifest is the single source of truth for which
// Pi tools skip the JIT plan (bridge-direct), which are unconditionally
// allowed (ungated-builtin), and which flow through the ordinary gate path
// (gated / gate-then-bridge-direct). These tests pin today's actual behavior
// (verified against governance.ts / gate-tool-call.ts / mapping.ts) so a
// manifest edit that silently changes a gating decision fails loudly here,
// before CP2 wires the manifest into the real gate-skip logic.
import { test } from "node:test";
import assert from "node:assert/strict";
import { TOOL_NAMES } from "../src/pi/tools.js";
import { mapTool } from "../src/broker/mapping.js";
import {
  GATING_MANIFEST,
  MCP_GATING,
  gateSkippedTools,
  ungatedBuiltins,
} from "../src/pi/gating-manifest.js";

const PINNED_BRIDGE_DIRECT = [
  "delegate",
  "workflow_save",
  "workflow_run",
  "workflow_list",
  "workflow_publish",
  "workflow_propose",
  "workflow_schedule",
];

test("every TOOL_NAMES entry appears in the manifest exactly once", () => {
  const manifestKeys = Object.keys(GATING_MANIFEST).filter(
    (k) => k !== "load_skill" && k !== "read_skill_file",
  );
  assert.deepStrictEqual(new Set(manifestKeys), new Set(TOOL_NAMES));
});

test("load_skill and read_skill_file are present with model ungated-builtin, and are the only ungated-builtins", () => {
  assert.strictEqual(GATING_MANIFEST.load_skill?.model, "ungated-builtin");
  assert.strictEqual(GATING_MANIFEST.read_skill_file?.model, "ungated-builtin");
  const ungated = Object.entries(GATING_MANIFEST).filter(([, v]) => v.model === "ungated-builtin");
  assert.deepStrictEqual(
    new Set(ungated.map(([k]) => k)),
    new Set(["load_skill", "read_skill_file"]),
  );
});

test("every gated / gate-then-bridge-direct entry resolves via mapTool", () => {
  for (const [name, entry] of Object.entries(GATING_MANIFEST)) {
    if (entry.model === "gated" || entry.model === "gate-then-bridge-direct") {
      assert.ok(mapTool(name) !== undefined, `${name} (${entry.model}) must resolve via mapTool`);
    }
  }
});

test("bridge-direct entries equal the pinned expected list", () => {
  const bridgeDirect = Object.entries(GATING_MANIFEST)
    .filter(([, v]) => v.model === "bridge-direct")
    .map(([k]) => k);
  assert.deepStrictEqual(new Set(bridgeDirect), new Set(PINNED_BRIDGE_DIRECT));
});

test("bridge-direct entries do not resolve to an InvokeTool route (not in mapTool)", () => {
  for (const name of PINNED_BRIDGE_DIRECT) {
    assert.strictEqual(mapTool(name), undefined, `${name} must not resolve via mapTool`);
  }
});

test("gateSkippedTools() equals the pinned bridge-direct list", () => {
  assert.deepStrictEqual(gateSkippedTools(), new Set(PINNED_BRIDGE_DIRECT));
});

test("ungatedBuiltins() equals [load_skill, read_skill_file]", () => {
  assert.deepStrictEqual(ungatedBuiltins(), new Set(["load_skill", "read_skill_file"]));
});

test("every manifest entry has a non-empty authz string", () => {
  for (const [name, entry] of Object.entries(GATING_MANIFEST)) {
    assert.ok(
      typeof entry.authz === "string" && entry.authz.length > 0,
      `${name} must have a non-empty authz string`,
    );
  }
});

test("MCP_GATING structural rule is gated", () => {
  assert.strictEqual(MCP_GATING, "gated");
});

test("CP5 spawn_subagents: declared gate-then-bridge-direct, naming CheckFGA(skill:subagents)", () => {
  // WHY gate-then-bridge-direct: mirrors
  // analyze_image — JIT-plan-gated (CheckFGA skill:subagents) via gate(), then
  // executed bridge-direct, bypassing InvokeTool (spawn_subagents has no Tool
  // Proxy registration; every branch tool call is separately gated on its own).
  assert.strictEqual(GATING_MANIFEST.spawn_subagents?.model, "gate-then-bridge-direct");
  assert.match(GATING_MANIFEST.spawn_subagents?.authz ?? "", /CheckFGA.*skill:subagents/);
});

test("CP5 spawn_subagents: must NOT be pinned bridge-direct — that list is model==='bridge-direct' only", () => {
  assert.ok(
    !PINNED_BRIDGE_DIRECT.includes("spawn_subagents"),
    "spawn_subagents is gate-then-bridge-direct, not bridge-direct — adding it to PINNED_BRIDGE_DIRECT would wrongly skip its JIT gate",
  );
  assert.ok(!gateSkippedTools().has("spawn_subagents"), "gateSkippedTools() must not include spawn_subagents");
});
