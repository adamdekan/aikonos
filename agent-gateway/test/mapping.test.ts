// Tests for mapTool — the tool-name → broker toolId + effectClass resolver.
//
// WHY these exist: gate() calls mapTool for every non-workflow tool call. Two
// callers pass DIFFERENT name forms:
//   - the Pi loop passes underscore names (web_fetch)      — LLM function names,
//   - the workflow run driver passes broker skill ids      — stored WorkflowDef
//     steps carry the dotted form (web.fetch), because workflow_save/propose
//     document `skill` as "broker skill id, e.g. web.fetch".
// If mapTool only recognises the underscore form, every workflow step is denied
// ("tool 'web.fetch' is not permitted by aikonos policy") and runs always halt at
// step 0. These tests pin that mapTool accepts BOTH forms.
import { test } from "node:test";
import { piMcpToolName } from "../src/pi/mcp-alias.js";
import assert from "node:assert/strict";
import { mapTool, knownToolIds, unknownSkills } from "../src/broker/mapping.js";
import { EffectClass } from "../gen/ts/proto/plan.js";

// ── Pi underscore form (the Pi loop) ─────────────────────────────────────────

test("mapTool: underscore Pi names resolve to the dotted broker toolId", () => {
  assert.deepEqual(mapTool("web_fetch"), { toolId: "web.fetch", effectClass: EffectClass.READ_ONLY });
  assert.deepEqual(mapTool("email_draft"), { toolId: "email.draft", effectClass: EffectClass.WRITE_EXTERNAL });
  assert.deepEqual(mapTool("doc_write"), { toolId: "doc.write", effectClass: EffectClass.WRITE_LOCAL });
});

// ── Dotted broker-id form (the workflow run driver) ──────────────────────────

test("mapTool: dotted broker skill ids resolve (workflow-stored form)", () => {
  // This is the regression: workflows store `doc.read`, `web.fetch`, etc.
  assert.deepEqual(mapTool("web.fetch"), { toolId: "web.fetch", effectClass: EffectClass.READ_ONLY });
  assert.deepEqual(mapTool("doc.read"), { toolId: "doc.read", effectClass: EffectClass.READ_ONLY });
  assert.deepEqual(mapTool("doc.write"), { toolId: "doc.write", effectClass: EffectClass.WRITE_LOCAL });
  assert.deepEqual(mapTool("email.draft"), { toolId: "email.draft", effectClass: EffectClass.WRITE_EXTERNAL });
});

// ── web_search (CP3: ) ───────────────────────────────

test("mapTool: web_search resolves to the web.search toolId, read-only effect class", () => {
  assert.deepEqual(mapTool("web_search"), { toolId: "web.search", effectClass: EffectClass.READ_ONLY });
  assert.deepEqual(mapTool("web.search"), { toolId: "web.search", effectClass: EffectClass.READ_ONLY });
});

// ── MCP tools, both forms ────────────────────────────────────────────────────

test("mapTool: MCP Pi name (mcp__conn__tool) resolves to colon toolId", () => {
  assert.deepEqual(mapTool(piMcpToolName("jira", "get_issue")), {
    toolId: "mcp:jira:get_issue",
    effectClass: EffectClass.READ_ONLY, // "get_" is a read verb
  });
  assert.deepEqual(mapTool(piMcpToolName("jira", "create_issue")), {
    toolId: "mcp:jira:create_issue",
    effectClass: EffectClass.WRITE_EXTERNAL,
  });
});

test("mapTool: MCP broker-id form (mcp:conn:tool) resolves (workflow-stored form)", () => {
  assert.deepEqual(mapTool("mcp:jira:get_issue"), {
    toolId: "mcp:jira:get_issue",
    effectClass: EffectClass.READ_ONLY,
  });
  assert.deepEqual(mapTool("mcp:jira:create_issue"), {
    toolId: "mcp:jira:create_issue",
    effectClass: EffectClass.WRITE_EXTERNAL,
  });
});

// ── analyze_image (F7/CP6: skill:vision-gated tool) ─────────────────────────

test("mapTool: analyze_image resolves to the vision toolId, read-only effect class", () => {
  // WHY: analyze_image must flow through the standard gate() → SubmitPlan →
  // CheckFGA(user, can_invoke, "skill:"+toolID) path like any other tool (no
  // load_skill-style bypass). That only happens if mapTool recognizes it; the
  // resolved toolId must be exactly "vision" so the FGA check becomes
  // "skill:vision" — the capability skill CP2 registered.
  assert.deepEqual(mapTool("analyze_image"), { toolId: "vision", effectClass: EffectClass.READ_ONLY });
});

// ── spawn_subagents ─

test("mapTool: spawn_subagents resolves to the subagents toolId, read-only effect class", () => {
  // WHY: spawn_subagents itself spawns children and calls no external service —
  // every branch tool call is separately gated through the ordinary path — so
  // READ_ONLY is the correct effect class for the spawn call itself (same
  // reasoning as analyze_image's "vision" entry above).
  assert.deepEqual(mapTool("spawn_subagents"), { toolId: "subagents", effectClass: EffectClass.READ_ONLY });
});

// ── Unknown ──────────────────────────────────────────────────────────────────

test("mapTool: unknown tool names return undefined", () => {
  assert.equal(mapTool("not_a_tool"), undefined);
  assert.equal(mapTool("not.a.tool"), undefined);
});

// ── knownToolIds / unknownSkills (workflow authoring guard) ───────────────────

test("knownToolIds: returns the built-in aikonos tool ids in dotted form", () => {
  const ids = knownToolIds();
  assert.ok(ids.includes("web.fetch"));
  assert.ok(ids.includes("doc.read"));
  assert.ok(ids.includes("doc.write"));
  assert.ok(ids.includes("email.draft"));
  assert.ok(ids.includes("vision"));
  // Never the Pi underscore form.
  assert.ok(!ids.includes("web_fetch"));
});

test("unknownSkills: flags invented tools, accepts real + mcp ids", () => {
  // The reported bug: the model composed workflows from invented tools.
  const bad = unknownSkills(["web.fetch", "data.transform", "doc.read", "template.render", "chat.output"]);
  assert.deepEqual(bad, ["data.transform", "template.render", "chat.output"]);
});

test("unknownSkills: real ids (dotted + underscore) and mcp ids are all accepted", () => {
  assert.deepEqual(
    unknownSkills(["web.fetch", "doc_read", "email.draft", "mcp:jira:get_issue"]),
    [],
    "every resolvable form must pass the authoring guard",
  );
});

test('unknownSkills: flags "vision" even though mapTool resolves it (F7/CP6 gap)', () => {
  // WHY: analyze_image's own tool_call must resolve via mapTool (toolId: "vision")
  // so its per-call FGA gate works — asserted above via mapTool("analyze_image").
  // But "vision" has no Tool Proxy registration, so a workflow step referencing
  // it would fail at RUN time instead of being rejected at authoring time. It
  // must be excluded from the skill set workflow-step authoring accepts, the
  // same way the five workflow tool names are implicitly excluded (absent from
  // TOOLS/TOOLS_BY_ID) — without breaking mapTool("analyze_image") itself.
  assert.deepEqual(mapTool("analyze_image"), { toolId: "vision", effectClass: EffectClass.READ_ONLY });
  assert.deepEqual(
    unknownSkills(["doc.read", "vision"]),
    ["vision"],
    '"vision" must be rejected as an unresolvable workflow-step skill',
  );
});

test('unknownSkills: flags "subagents" even though mapTool resolves it (same posture as vision)', () => {
  // WHY: spawn_subagents's own tool_call must resolve via mapTool (toolId:
  // "subagents") for its per-call FGA gate to work — asserted above. But
  // "subagents" has no Tool Proxy registration, so a workflow step referencing
  // it must be rejected at authoring time — mirrors the vision/workflow-
  // unresolvable-skills precedent (fan-out is a spec non-goal for workflows).
  assert.deepEqual(mapTool("spawn_subagents"), { toolId: "subagents", effectClass: EffectClass.READ_ONLY });
  assert.deepEqual(
    unknownSkills(["doc.read", "subagents"]),
    ["subagents"],
    '"subagents" must be rejected as an unresolvable workflow-step skill',
  );
});
