// Tests for CP4 + CP5: governance mapping, dynamic MCP Pi tools.
import { test } from "node:test";
import assert from "node:assert/strict";
import type { Config } from "../src/config";
import type { BrokerClients } from "../src/broker/clients";
import type { ToolMapping } from "../src/broker/mapping";
import { piMcpToolName } from "../src/pi/mcp-alias.js";

// ── CP4: mcp:* governance mapping ───────────────────────────────────────────

test("mapTool: mcp__<C>__search classifies READ_ONLY (read-verb heuristic)", async () => {
  const { mapTool } = await import("../src/broker/mapping.js");
  const m: ToolMapping | undefined = mapTool(piMcpToolName("abc-123", "search"));
  assert.ok(m, "should return a mapping");
  // READ_ONLY = 1 per plan.proto EffectClass — search matches the read-verb heuristic
  assert.equal(m.effectClass, 1);
  assert.equal(m.toolId, "mcp:abc-123:search");
});

test("mapTool: mcp__ with multi-segment tool name", async () => {
  const { mapTool } = await import("../src/broker/mapping.js");
  const m = mapTool(piMcpToolName("server-id", "do_something"));
  assert.ok(m);
  assert.equal(m.toolId, "mcp:server-id:do_something");
});

test("mapTool: returns undefined for unknown non-mcp tool", async () => {
  const { mapTool } = await import("../src/broker/mapping.js");
  assert.equal(mapTool("not_a_tool"), undefined);
});

test("mapTool: existing static tools not broken", async () => {
  const { mapTool } = await import("../src/broker/mapping.js");
  const m: ToolMapping | undefined = mapTool("web_fetch");
  assert.ok(m);
  // READ_ONLY = 1 per plan.proto EffectClass
  assert.equal(m.effectClass, 1);
  assert.equal(m.toolId, "web.fetch");
});

test("mapTool: get_article read-verb prefix → READ_ONLY", async () => {
  const { mapTool } = await import("../src/broker/mapping.js");
  const m: ToolMapping | undefined = mapTool(piMcpToolName("c", "get_article"));
  assert.ok(m);
  assert.equal(m.effectClass, 1, "get_article should be READ_ONLY");
  assert.equal(m.toolId, "mcp:c:get_article");
});

test("mapTool: create_doc no read-verb → WRITE_EXTERNAL", async () => {
  const { mapTool } = await import("../src/broker/mapping.js");
  const m: ToolMapping | undefined = mapTool(piMcpToolName("c", "create_doc"));
  assert.ok(m);
  assert.equal(m.effectClass, 4, "create_doc should be WRITE_EXTERNAL");
});

test("mapTool: readOnlyHint=true overrides write name → READ_ONLY", async () => {
  const { mapTool } = await import("../src/broker/mapping.js");
  const m: ToolMapping | undefined = mapTool(piMcpToolName("c", "create_doc"), true);
  assert.ok(m);
  assert.equal(m.effectClass, 1, "readOnlyHint=true must override the write-verb name");
});

test("mapTool: list_things read-verb prefix → READ_ONLY", async () => {
  const { mapTool } = await import("../src/broker/mapping.js");
  const m: ToolMapping | undefined = mapTool(piMcpToolName("c", "list_things"));
  assert.ok(m);
  assert.equal(m.effectClass, 1, "list_things should be READ_ONLY");
});

test("mapTool: delete_thing with readOnlyHint=false falls through to heuristic → WRITE_EXTERNAL", async () => {
  const { mapTool } = await import("../src/broker/mapping.js");
  // explicit false must not be treated as truthy; heuristic sees "delete" → not a read verb
  const m: ToolMapping | undefined = mapTool(piMcpToolName("c", "delete_thing"), false);
  assert.ok(m);
  assert.equal(m.effectClass, 4, "delete_thing with hint=false should be WRITE_EXTERNAL");
});

test("mapTool: getArticle camelCase read verb → READ_ONLY", async () => {
  const { mapTool } = await import("../src/broker/mapping.js");
  const m: ToolMapping | undefined = mapTool(piMcpToolName("c", "getArticle"));
  assert.ok(m);
  assert.equal(m.effectClass, 1, "getArticle camelCase should be READ_ONLY");
  assert.equal(m.toolId, "mcp:c:getArticle");
});

test("mapTool: getter is not a read-verb prefix → WRITE_EXTERNAL", async () => {
  const { mapTool } = await import("../src/broker/mapping.js");
  // "getter" = bare "get" + lowercase "t" (no _/uppercase/end boundary) → not a read verb
  const m: ToolMapping | undefined = mapTool(piMcpToolName("c", "getter"));
  assert.ok(m);
  assert.equal(m.effectClass, 4, "getter should NOT match the read heuristic");
});

// ── CP4: execute passes agentId to invokeTool ────────────────────────────────

test("GovernanceBridge.execute passes agentId to invokeTool", async () => {
  const { GovernanceBridge } = await import("../src/broker/governance.js");

  let capturedAgentId: string | undefined;

  const fakeSouth = {
    invokeTool: async (req: Record<string, unknown>) => {
      capturedAgentId = req["agentId"] as string;
      return { success: true, result: null, error: "", costUnitsConsumed: 0 };
    },
    emitStatus: async () => undefined,
    submitPlan: async () => { throw new Error("should not be called in execute"); },
    claimDueScheduledRuns: async () => { throw new Error("nope"); },
    reportScheduledRunResult: async () => undefined,
    listAccessibleMcpServersForAgent: async () => ({ connections: [] }),
    listMcpServerToolsSouth: async () => ({ tools: [] }),
    close: () => undefined,
  };

  const fakeLog = {
    info: () => undefined,
    warn: () => undefined,
    error: () => undefined,
    debug: () => undefined,
  };

  const fakeConfig = {
    defaultTenantId: "t1",
    gatewaySpiffeId: "spiffe://x",
  } as Config;

  const fakeClients = {
    south: fakeSouth,
  } as unknown as BrokerClients;

  const bridge = new GovernanceBridge(
    fakeConfig,
    fakeClients,
    { tenantId: "t1", userId: "alice@example.com", agentId: "alice-agent" },
    async () => true,
    // fakeLog satisfies the warn/info subset used by GovernanceBridge at runtime;
    // cast to unknown to avoid the full pino Logger type in tests.
    fakeLog as unknown as ConstructorParameters<typeof GovernanceBridge>[4],
  );

  const toolCallId = "tc-1";
  // Inject a pending call directly (simulates gate() having run).
  // @ts-expect-error accessing private field for test setup
  bridge.pending.set(toolCallId, {
    taskId: "task-1",
    capToken: "tok",
    mapping: { toolId: "mcp:srv:search", effectClass: 4 },
    args: {},
  });

  await bridge.execute(toolCallId);
  assert.equal(capturedAgentId, "alice-agent");
});
