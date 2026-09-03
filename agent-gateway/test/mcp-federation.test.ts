// Tests for CP4 + CP5: agent identity, governance mapping, dynamic MCP Pi tools.
import { test } from "node:test";
import assert from "node:assert/strict";
import type { Config } from "../src/config";
import type { BrokerClients } from "../src/broker/clients";
import type { ToolMapping } from "../src/broker/mapping";
import { piMcpToolName } from "../src/pi/mcp-alias.js";

// ── CP4: agentForUser ────────────────────────────────────────────────────────

test("agentForUser: localpart-agent default", async () => {
  const { agentForUser } = await import("../src/broker/agent-identity.js");
  assert.equal(agentForUser("alice@example.com", {}), "alice-agent");
  assert.equal(agentForUser("bob@example.com", {}), "bob-agent");
  // no @-sign: entire string used as localpart
  assert.equal(agentForUser("carol", {}), "carol-agent");
});

test("agentForUser: override map respected", async () => {
  const { agentForUser } = await import("../src/broker/agent-identity.js");
  const overrides = { "alice@example.com": "custom-alice-agent" };
  assert.equal(agentForUser("alice@example.com", overrides), "custom-alice-agent");
  // Non-overridden user still gets the default.
  assert.equal(agentForUser("bob@example.com", overrides), "bob-agent");
});

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

// ── CP5: buildMcpTools ────────────────────────────────────────────────────────

function makeMinimalSouth(
  servers: { id: string; name: string }[],
  toolsByServer: Record<string, { name: string; description: string; inputSchemaJson: string; readOnlyHint: boolean }[]>,
) {
  return {
    listAccessibleMcpServersForAgent: async () => ({
      connections: servers.map((s) => ({
        id: s.id,
        name: s.name,
        url: "https://mcp.example.com",
        transport: "streamable_http" as const,
        authType: "bearer" as const,
        authSecretRef: "",
        createdBy: "",
        createdAt: undefined,
      })),
    }),
    listMcpServerToolsSouth: async (req: { connectorId: string }) => ({
      tools: toolsByServer[req.connectorId] ?? [],
    }),
  };
}

const fakeCfg = { defaultTenantId: "t1" } as Config;
// Satisfies the McpLog interface (warn/info with obj+msg signature).
const fakeLog = {
  info: (_obj: Record<string, unknown>, _msg: string) => undefined,
  warn: (_obj: Record<string, unknown>, _msg: string) => undefined,
};
const fakeBridge = { gate: async () => ({ allow: true }), execute: async () => ({ ok: true, output: null }) } as unknown;

test("buildMcpTools: registers one tool per discovered tool", async () => {
  const { buildMcpTools } = await import("../src/pi/mcp-tools.js");

  const south = makeMinimalSouth(
    [{ id: "srv-1", name: "My Server" }],
    {
      "srv-1": [{ name: "search", description: "Search the internet", inputSchemaJson: JSON.stringify({ type: "object", properties: { q: { type: "string" } } }), readOnlyHint: false }],
    },
  );

  const tools = await buildMcpTools(
    fakeCfg,
    south as Parameters<typeof buildMcpTools>[1],
    fakeBridge as Parameters<typeof buildMcpTools>[2],
    "alice-agent",
    fakeLog,
  );

  assert.equal(tools.length, 1);
  assert.equal(tools[0].name, "mcp__srv-1__search");
  assert.ok(tools[0].description.includes("search") || tools[0].description.includes("Search"), "description should contain tool info");
  // Schema must be appended so the LLM knows the argument shape.
  assert.ok(tools[0].description.includes("Arguments JSON Schema:"), "description must include the argument JSON Schema");
  assert.ok(tools[0].description.includes('"type"'), "schema content must appear in description");
});

test("buildMcpTools: passes agentId as userId to listMcpServerToolsSouth", async () => {
  const { buildMcpTools } = await import("../src/pi/mcp-tools.js");

  let capturedUserId: string | undefined;
  const south = {
    listAccessibleMcpServersForAgent: async () => ({
      connections: [{ id: "srv-1", name: "S", url: "x", transport: "streamable_http" as const, authType: "bearer" as const, authSecretRef: "", createdBy: "", createdAt: undefined }],
    }),
    listMcpServerToolsSouth: async (req: { userId: string }) => {
      capturedUserId = req.userId;
      return { tools: [] };
    },
  };

  await buildMcpTools(
    fakeCfg,
    south as Parameters<typeof buildMcpTools>[1],
    fakeBridge as Parameters<typeof buildMcpTools>[2],
    "alice-agent",
    fakeLog,
  );

  assert.equal(capturedUserId, "alice-agent", "agentId should be forwarded as userId to listMcpServerToolsSouth");
});

test("buildMcpTools: discovery failure for a server skips it without throwing", async () => {
  const { buildMcpTools } = await import("../src/pi/mcp-tools.js");

  const south = {
    listAccessibleMcpServersForAgent: async () => ({
      connections: [{ id: "srv-fail", name: "Failing", url: "x", transport: "streamable_http" as const, authType: "bearer" as const, authSecretRef: "", createdBy: "", createdAt: undefined }],
    }),
    listMcpServerToolsSouth: async () => { throw new Error("server unreachable"); },
  };

  const tools = await buildMcpTools(
    fakeCfg,
    south as Parameters<typeof buildMcpTools>[1],
    fakeBridge as Parameters<typeof buildMcpTools>[2],
    "alice-agent",
    fakeLog,
  );

  assert.equal(tools.length, 0, "failed server should be skipped, returning empty array");
});

test("buildMcpTools: tool execute routes through bridge with correct toolId", async () => {
  const { buildMcpTools } = await import("../src/pi/mcp-tools.js");

  let capturedGateName: string | undefined;
  let capturedExecuteId: string | undefined;

  const south = makeMinimalSouth(
    [{ id: "srv-1", name: "Server 1" }],
    { "srv-1": [{ name: "search", description: "Search", inputSchemaJson: "{}", readOnlyHint: false }] },
  );

  const capturingBridge = {
    gate: async (toolCallId: string, toolName: string) => {
      capturedGateName = toolName;
      return { allow: true };
    },
    execute: async (toolCallId: string) => {
      capturedExecuteId = toolCallId;
      return { ok: true, output: "result" };
    },
  };

  const tools = await buildMcpTools(
    fakeCfg,
    south as Parameters<typeof buildMcpTools>[1],
    capturingBridge as Parameters<typeof buildMcpTools>[2],
    "alice-agent",
    fakeLog,
  );

  assert.equal(tools.length, 1);

  // Execute the tool — it should call bridge.gate with the Pi tool name, then bridge.execute.
  const toolCallId = "tc-test";
  // The execute signature is (toolCallId, params, signal, onUpdate, ctx) but we
  // call with just the first two since the bridge stub ignores signal/ctx.
  // @ts-expect-error partial args for test (bridge stub doesn't need signal/ctx)
  await tools[0].execute(toolCallId, {});

  assert.equal(capturedGateName, "mcp__srv-1__search", "gate should receive the Pi tool name");
  assert.equal(capturedExecuteId, toolCallId, "execute should be called with the tool call id");
});
