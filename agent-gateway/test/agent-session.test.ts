import { test } from "node:test";
import assert from "node:assert/strict";
import { resolveEnvModel, BUILTIN_TOOL_NAME, allowedPiToolNames, computeActiveToolNames } from "../src/pi/session";

// ── resolveEnvModel: agent override precedence ────────────────────────────────

function stubSouth(tenantModel: string) {
  return { getTenantModel: async (_req: { tenantId: string }) => ({ model: tenantModel }) };
}

const cfg = { llmModel: "anthropic/claude-sonnet-4.6" };

test("resolveEnvModel: agent model wins over tenant config", async () => {
  // Even when the broker returns a tenant override, the agent model takes precedence.
  const model = await resolveEnvModel(cfg, stubSouth("openai/gpt-4o"), "t1", "mistralai/mistral-large");
  assert.equal(model, "mistralai/mistral-large");
});

test("resolveEnvModel: agent model wins over env default", async () => {
  // Empty tenant config → env default, but agent model still wins.
  const model = await resolveEnvModel(cfg, stubSouth(""), "t1", "google/gemini-pro");
  assert.equal(model, "google/gemini-pro");
});

test("resolveEnvModel: empty agentModel falls through to tenant config", async () => {
  const model = await resolveEnvModel(cfg, stubSouth("openai/gpt-4o"), "t1", "");
  assert.equal(model, "openai/gpt-4o");
});

test("resolveEnvModel: undefined agentModel falls through to env default", async () => {
  const model = await resolveEnvModel(cfg, stubSouth(""), "t1", undefined);
  assert.equal(model, "anthropic/claude-sonnet-4.6");
});

// ── BUILTIN_TOOL_NAME: skill-to-Pi-name filter map ────────────────────────

test("BUILTIN_TOOL_NAME maps every standard builtin skill", () => {
  const expected: Record<string, string> = {
    "web.fetch":      "web_fetch",
    "workspace.read": "workspace_read",
    "doc.read":       "doc_read",
    "doc.write":      "doc_write",
    "email.draft":    "email_draft",
  };
  for (const [brokerId, piName] of Object.entries(expected)) {
    assert.equal(BUILTIN_TOOL_NAME[brokerId], piName, `expected ${brokerId} → ${piName}`);
  }
});

test("BUILTIN_TOOL_NAME includes connector tools", () => {
  assert.equal(BUILTIN_TOOL_NAME["gdrive.read"],    "gdrive_read");
  assert.equal(BUILTIN_TOOL_NAME["gdrive.write"],   "gdrive_write");
  assert.equal(BUILTIN_TOOL_NAME["onedrive.read"],  "onedrive_read");
  assert.equal(BUILTIN_TOOL_NAME["onedrive.write"], "onedrive_write");
});

test("BUILTIN_TOOL_NAME does not include unknown ids", () => {
  assert.equal(BUILTIN_TOOL_NAME["unknown.tool"], undefined);
});

// ── allowedPiToolNames: skill → Pi name conversion ─────────────────────────
// Exercises the exported helper directly so tests assert the real production
// path — not a copy that can drift.

test("allowedPiToolNames: subset of builtins maps correctly", () => {
  const result = allowedPiToolNames(["web.fetch", "doc.read"]);
  assert.ok(result.has("web_fetch"));
  assert.ok(result.has("doc_read"));
  assert.equal(result.has("doc_write"), false);
  assert.equal(result.has("workspace_read"), false);
  // delegate is always present
  assert.ok(result.has("delegate"));
});

test("allowedPiToolNames: empty skills list yields only delegate", () => {
  const result = allowedPiToolNames([]);
  assert.equal(result.size, 1);
  assert.ok(result.has("delegate"));
});

test("allowedPiToolNames: mcp skill converts to mcp__ Pi name", () => {
  const result = allowedPiToolNames(["mcp:conn-abc:list_files"]);
  assert.ok(result.has("mcp__conn-abc__list_files"));
});

test("allowedPiToolNames: mcp skill without second colon is ignored (malformed)", () => {
  const result = allowedPiToolNames(["mcp:no-tool-part"]);
  // only delegate
  assert.equal(result.size, 1);
  assert.ok(result.has("delegate"));
});

test("allowedPiToolNames: unknown builtin id is silently dropped", () => {
  const result = allowedPiToolNames(["unknown.tool", "web.fetch"]);
  assert.ok(result.has("web_fetch"));
  // web_fetch + delegate
  assert.equal(result.size, 2);
});

// ── subagents capability skill ───────────

test("allowedPiToolNames: skill:subagents surfaces spawn_subagents", () => {
  const result = allowedPiToolNames(["subagents"]);
  assert.ok(result.has("spawn_subagents"));
});

test("allowedPiToolNames: without skill:subagents, spawn_subagents is absent", () => {
  const result = allowedPiToolNames(["web.fetch"]);
  assert.equal(result.has("spawn_subagents"), false);
});

// ── computeActiveToolNames: userSkills filtering for personal sessions ────────

const ALL_STATIC = ["web_fetch", "workspace_read", "doc_read", "doc_write", "email_draft",
  "gdrive_read", "gdrive_write", "onedrive_read", "onedrive_write", "delegate"];

test("computeActiveToolNames: userSkills [web.fetch] → web_fetch + delegate only", () => {
  const names = computeActiveToolNames(ALL_STATIC, [], undefined, ["web.fetch"]);
  assert.deepEqual(names.sort(), ["delegate", "web_fetch"].sort());
});

test("computeActiveToolNames: userSkills [] → delegate only", () => {
  const names = computeActiveToolNames(ALL_STATIC, [], undefined, []);
  assert.deepEqual(names, ["delegate"]);
});

test("computeActiveToolNames: userSkills undefined → all tools (back-compat)", () => {
  const names = computeActiveToolNames(ALL_STATIC, [], undefined, undefined);
  assert.deepEqual(names.sort(), ALL_STATIC.sort());
});

test("computeActiveToolNames: userSkills path leaves MCP tools unfiltered", () => {
  const mcpNames = ["mcp__conn1__list_files"];
  const names = computeActiveToolNames(ALL_STATIC, mcpNames, undefined, ["web.fetch"]);
  // web_fetch (from userSkills) + delegate (always) + mcp tool (unfiltered)
  assert.ok(names.includes("web_fetch"));
  assert.ok(names.includes("delegate"));
  assert.ok(names.includes("mcp__conn1__list_files"));
  assert.equal(names.filter((n) => n !== "web_fetch" && n !== "delegate" && n !== "mcp__conn1__list_files").length, 0);
});

test("computeActiveToolNames: spec path takes priority over userSkills", () => {
  // spec.skills = ["doc.read"] should win; userSkills is ignored when spec is present
  const spec = { model: "", approvalMode: "needs_approval", skills: ["doc.read"] };
  const names = computeActiveToolNames(ALL_STATIC, [], spec, ["web.fetch"]);
  assert.ok(names.includes("doc_read"));
  assert.ok(names.includes("delegate"));
  assert.equal(names.includes("web_fetch"), false);
});

test("computeActiveToolNames: spec path leaves MCP tools unfiltered (agent-bound)", () => {
  // Agent MCP access is granted via mcp_servers + per-agent discovery, not via
  // the FGA skill registry, so allowedPiToolNames can never admit an MCP tool.
  // The agent's discovered MCP tools must survive the spec.skills filter — this
  // is what lets MCP access assigned to the agent flow through the external API.
  const spec = { model: "", approvalMode: "auto", skills: ["doc.read"] };
  const mcpNames = ["mcp__conn1__list_files"];
  const names = computeActiveToolNames(ALL_STATIC, mcpNames, spec, []);
  assert.ok(names.includes("doc_read"), "agent skill doc.read surfaces doc_read");
  assert.ok(names.includes("delegate"), "delegate always present");
  assert.ok(names.includes("mcp__conn1__list_files"), "agent MCP tool must survive the spec filter");
  assert.equal(names.includes("web_fetch"), false, "ungranted static tool excluded");
});
