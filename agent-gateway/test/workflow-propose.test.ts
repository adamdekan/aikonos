// CP20 tests: workflow_propose Pi tool — gating, bridge wiring, IPC round-trip.
//
// WHY these tests exist:
//   1. Gating: allowedPiToolNames with 'workflows' skill must include
//      workflow_propose (alongside save/run/list/publish). Without it the tool is
//      unreachable from chat regardless of skill grant.
//   2. TOOL_NAMES: workflow_propose must be in the authoritative static list so
//      computeActiveToolNames can ever surface it.
//   3. Tool→bridge wiring: workflow_propose must call bridge.proposeWorkflow with
//      exactly lineageId and def. A mismatch means the broker receives a wrong request.
//   4. NORTH/SOUTH routing: GovernanceBridge.proposeWorkflow must call the OIDC
//      north client when a bearer token is present, and the SPIFFE south client
//      (with ownerGrant) when absent. Mirrors the publishWorkflow routing test.
//   5. IPC round-trip: a propose-workflow request sent over the fake paired channel
//      must arrive at the RemoteBridgeServer, be dispatched to the bound bridge, and
//      produce a propose-workflow-result reply.
//   6. workflow_save is unchanged: it still creates an owned version directly (not
//      a proposed version).
//
// Intent encoded: "proposing a workflow version is reachable from chat as a gated
// tool and routes north under OIDC, south under ownerGrant; workflow_save is unchanged."
import { test } from "node:test";
import assert from "node:assert/strict";

import { allowedPiToolNames, computeActiveToolNames } from "../src/pi/session.js";
import { makeTools, TOOL_NAMES } from "../src/pi/tools.js";
import type { ToolDefinition } from "@earendil-works/pi-coding-agent";
import type { BridgeClientLike } from "../src/ipc/bridge-client.js";
import type { Approver } from "../src/broker/governance.js";
import { GovernanceBridge } from "../src/broker/governance.js";
import { makePairedChannel, makeSeq, ParentLink, ChildLink } from "../src/ipc/protocol.js";
import type { ProposeWorkflowRequest as IpcProposeRequest, ProposeWorkflowResult } from "../src/ipc/protocol.js";
import { RemoteBridgeServer } from "../src/ipc/bridge-server.js";
import type { BridgeLike } from "../src/ipc/bridge-server.js";

// ── Fake bridge ────────────────────────────────────────────────────────────────

interface ProposeFakeBridge extends BridgeClientLike {
  proposeWorkflowCalls: Array<{ lineageId: string; def: Record<string, unknown> }>;
  saveWorkflowCalls: number;
}

function makeProposeFakeBridge(): ProposeFakeBridge {
  const bridge: ProposeFakeBridge = {
    proposeWorkflowCalls: [],
    saveWorkflowCalls: 0,

    async gate() { return { allow: true }; },
    async execute() { return { ok: true, output: null }; },
    async delegate() { return { ok: true }; },
    setApprover(_a: Approver) {},
    setToken(_t?: string) {},
    usageIdentity() { return { tenantId: "", userId: "", agentId: "" }; },

    async saveWorkflow() {
      bridge.saveWorkflowCalls++;
      return { ok: true, lineageId: "lineage-1", workflowId: "wf-1", version: 1 };
    },
    async runWorkflow() { return { ok: true, result: null }; },
    async listWorkflows() { return { ok: true, items: [] }; },
    async publishWorkflow() { return { ok: true }; },

    async proposeWorkflow(lineageId: string, def: Record<string, unknown>) {
      bridge.proposeWorkflowCalls.push({ lineageId, def });
      return { ok: true, version: 2 };
    },
    async analyzeImage() { return { ok: true, text: "" }; },
    async scheduleWorkflow() { return { ok: true }; },
    async reason() { return { ok: true, output: "" }; },
  };
  return bridge;
}

// exec() invokes a ToolDefinition's execute() with only the two arguments the
// workflow tools actually use (toolCallId + params).
function exec(tool: ToolDefinition, toolCallId: string, params: unknown) {
  return tool.execute(toolCallId, params, undefined, undefined, undefined as never);
}

// ── 1. Gating tests ────────────────────────────────────────────────────────────

test("CP20 gating: allowedPiToolNames WITH 'workflows' skill includes workflow_propose", () => {
  // WHY: workflow_propose must surface alongside save/run/list/publish when the
  // user holds the workflows skill — it is part of the same skill grant.
  const allowed = allowedPiToolNames(["workflows"]);
  assert.equal(
    allowed.has("workflow_propose"),
    true,
    "workflow_propose must be in allowed set when 'workflows' skill is held",
  );
});

test("CP20 gating: allowedPiToolNames WITHOUT 'workflows' skill does not include workflow_propose", () => {
  // WHY: deny-by-default. A user without the workflows skill must not see
  // workflow_propose.
  const allowed = allowedPiToolNames(["web.fetch"]);
  assert.equal(
    allowed.has("workflow_propose"),
    false,
    "workflow_propose must NOT be in allowed set without 'workflows' skill",
  );
});

test("CP20 gating: computeActiveToolNames with workflows skill includes workflow_propose", () => {
  // WHY: computeActiveToolNames is what buildSession uses for the final tool
  // name list. All five workflow tools must propagate.
  const active = computeActiveToolNames(TOOL_NAMES, [], undefined, ["workflows"]);
  assert.equal(
    active.includes("workflow_propose"),
    true,
    "workflow_propose must appear in computeActiveToolNames result with 'workflows' skill",
  );
});

test("CP20 gating: TOOL_NAMES includes workflow_propose", () => {
  // WHY: TOOL_NAMES is the authoritative list fed to computeActiveToolNames as
  // allStaticNames. If absent here it can never appear in any session.
  assert.equal(
    TOOL_NAMES.includes("workflow_propose"),
    true,
    "workflow_propose must be present in TOOL_NAMES",
  );
});

test("CP20 gating: computeActiveToolNames WITHOUT workflows skill excludes workflow_propose", () => {
  // WHY: deny side — the tool must not leak to users without the skill.
  const active = computeActiveToolNames(TOOL_NAMES, [], undefined, ["web.fetch"]);
  assert.equal(
    active.includes("workflow_propose"),
    false,
    "workflow_propose must NOT appear in computeActiveToolNames result without 'workflows' skill",
  );
});

// ── 2. Tool→bridge wiring tests ───────────────────────────────────────────────

test("CP20 wiring: workflow_propose is present in makeTools() output", () => {
  const bridge = makeProposeFakeBridge();
  const tools = makeTools(bridge);
  const proposeTool = tools.find((t) => t.name === "workflow_propose");
  assert.ok(proposeTool, "workflow_propose tool must be in makeTools() output");
});

test("CP20 wiring: workflow_propose execute() calls bridge.proposeWorkflow with lineageId and def", async () => {
  // WHY: the tool definition must forward lineageId and def to the bridge exactly.
  const bridge = makeProposeFakeBridge();
  const tools = makeTools(bridge);
  const proposeTool = tools.find((t) => t.name === "workflow_propose");
  assert.ok(proposeTool, "workflow_propose tool must be present");

  await exec(proposeTool, "tc-prop-1", {
    lineageId: "lineage-abc",
    name: "my-workflow",
    description: "improved version",
    steps: [{ skill: "web.fetch", args: { url: "https://example.com" } }],
  });

  assert.equal(bridge.proposeWorkflowCalls.length, 1, "bridge.proposeWorkflow must be called once");
  const call = bridge.proposeWorkflowCalls[0];
  assert.ok(call, "proposeWorkflow call must be recorded");
  assert.equal(call.lineageId, "lineage-abc");
  assert.ok(call.def, "def must be forwarded");
});

test("CP20 wiring: workflow_propose returns formatted success text", async () => {
  // WHY: the result text is what the LLM reads back. It must mention the version
  // and not start with ERROR:.
  const bridge = makeProposeFakeBridge();
  const tools = makeTools(bridge);
  const proposeTool = tools.find((t) => t.name === "workflow_propose");
  assert.ok(proposeTool, "workflow_propose tool must be present");

  const result = await exec(proposeTool, "tc-prop-ok", {
    lineageId: "lineage-1",
    name: "my-workflow",
    steps: [{ skill: "web.fetch", args: {} }],
  });

  assert.ok(result.content.length > 0, "result must have content");
  const first = result.content[0];
  assert.ok(first, "result must have at least one content item");
  const text = first.type === "text" ? (first.text ?? "") : "";
  assert.ok(text.length > 0, "result text must be non-empty");
  assert.ok(!text.startsWith("ERROR:"), `result must not be an error on success, got: ${text}`);
});

test("CP20 wiring: workflow_propose returns ERROR text on bridge failure", async () => {
  // WHY: the model must see an ERROR: prefix so it can report failure rather than
  // silently treating an empty/undefined response as success.
  const bridge = makeProposeFakeBridge();
  bridge.proposeWorkflow = async () => ({ ok: false, error: "not the lineage owner" });
  const tools = makeTools(bridge);
  const proposeTool = tools.find((t) => t.name === "workflow_propose");
  assert.ok(proposeTool, "workflow_propose tool must be present");

  const result = await exec(proposeTool, "tc-prop-fail", {
    lineageId: "lineage-1",
    name: "my-workflow",
    steps: [{ skill: "web.fetch", args: {} }],
  });

  const first = result.content[0];
  assert.ok(first, "result must have at least one content item");
  const text = first.type === "text" ? (first.text ?? "") : "";
  assert.ok(text.startsWith("ERROR:"), `result must start with ERROR: on failure, got: ${text}`);
});

test("CP20 invariant: workflow_save is unchanged — it still calls bridge.saveWorkflow (not proposeWorkflow)", async () => {
  // WHY: workflow_save (authoring) must continue to create an owned version
  // directly. The propose path is strictly for proposing a change to an existing
  // lineage through the owner-gated decide loop.
  const bridge = makeProposeFakeBridge();
  const tools = makeTools(bridge);
  const saveTool = tools.find((t) => t.name === "workflow_save");
  assert.ok(saveTool, "workflow_save tool must still be present");

  await exec(saveTool, "tc-save-inv", {
    name: "direct-save",
    steps: [{ skill: "web.fetch", args: {} }],
  });

  assert.equal(bridge.saveWorkflowCalls, 1, "bridge.saveWorkflow must be called once by workflow_save");
  assert.equal(bridge.proposeWorkflowCalls.length, 0, "bridge.proposeWorkflow must NOT be called by workflow_save");
});

// ── 3. North/south routing tests ──────────────────────────────────────────────

const cfg = {
  gatewaySpiffeId: "spiffe://aikonos.com/agent-gateway",
  llmModel: "",
  brokerNorthAddr: "",
  brokerSouthAddr: "",
  brokerServerName: "",
  tlsCert: "",
  tlsKey: "",
  tlsCa: "",
  port: 8080,
  defaultTenantId: "11111111-1111-1111-1111-111111111111",
  keycloakUrl: "",
  keycloakRealm: "",
  keycloakClient: "",
  schedulerEnabled: false,
  schedulerTickMs: 30000,
  schedulerClaimLimit: 10,
  schedulerRunTimeoutMs: 180000,
  agentForUserOverrides: {},
  openrouterApiKey: "",
  oidcIssuer: "",
  oidcJwksUrl: "",
  oidcAudience: "",
} as unknown as import("../src/config.js").Config;

const log = {
  info: () => {},
  warn: () => {},
  error: () => {},
  debug: () => {},
} as unknown as import("../src/log.js").Logger;

interface CallLog { method: string; args: unknown[] }

function makeNorth() {
  const calls: CallLog[] = [];
  return {
    calls,
    createTask: (...args: unknown[]) => {
      calls.push({ method: "createTask", args });
      return Promise.resolve({ taskId: "task-1" });
    },
    approveTask: (...args: unknown[]) => {
      calls.push({ method: "approveTask", args });
      return Promise.resolve({ capabilityTokenIds: { 1: "tok-north" } });
    },
    sendEnvelope: (...args: unknown[]) => {
      calls.push({ method: "sendEnvelope", args });
      return Promise.resolve({ envelopeId: "env-1" });
    },
    saveWorkflow: (...args: unknown[]) => {
      calls.push({ method: "saveWorkflow", args });
      return Promise.resolve({ workflowId: "wf-1", lineageId: "lin-1", version: 1 });
    },
    getWorkflow: (...args: unknown[]) => {
      calls.push({ method: "getWorkflow", args });
      return Promise.resolve({ definitionJson: '{"steps":[]}' });
    },
    listWorkflows: (...args: unknown[]) => {
      calls.push({ method: "listWorkflows", args });
      return Promise.resolve({ items: [] });
    },
    publishWorkflow: (...args: unknown[]) => {
      calls.push({ method: "publishWorkflow", args });
      return Promise.resolve({ visibilityKind: "group", groups: ["security-team"] });
    },
    proposeWorkflowVersion: (...args: unknown[]) => {
      calls.push({ method: "proposeWorkflowVersion", args });
      return Promise.resolve({ version: 3 });
    },
  };
}

function makeSouth() {
  const calls: CallLog[] = [];
  return {
    calls,
    createGatewayTask: (...args: unknown[]) => {
      calls.push({ method: "createGatewayTask", args });
      return Promise.resolve({ taskId: "task-1" });
    },
    approveGatewayTask: (...args: unknown[]) => {
      calls.push({ method: "approveGatewayTask", args });
      return Promise.resolve({ capabilityTokenIds: { 1: "tok-south" } });
    },
    submitPlan: (...args: unknown[]) => {
      calls.push({ method: "submitPlan", args });
      return Promise.resolve({ outcome: 1, capabilityTokenIds: { 1: "tok-1" }, violations: [], steps: [] });
    },
    invokeTool: (...args: unknown[]) => {
      calls.push({ method: "invokeTool", args });
      return Promise.resolve({ success: true, result: "ok", error: "", costUnitsConsumed: 0 });
    },
    emitStatus: (...args: unknown[]) => {
      calls.push({ method: "emitStatus", args });
      return Promise.resolve();
    },
    saveWorkflow: (...args: unknown[]) => {
      calls.push({ method: "saveWorkflow", args });
      return Promise.resolve({ workflowId: "wf-1", lineageId: "lin-1", version: 1 });
    },
    getWorkflow: (...args: unknown[]) => {
      calls.push({ method: "getWorkflow", args });
      return Promise.resolve({ definitionJson: '{"steps":[]}' });
    },
    listWorkflows: (...args: unknown[]) => {
      calls.push({ method: "listWorkflows", args });
      return Promise.resolve({ items: [] });
    },
    publishWorkflow: (...args: unknown[]) => {
      calls.push({ method: "publishWorkflow", args });
      return Promise.resolve({ visibilityKind: "group", groups: ["security-team"] });
    },
    proposeWorkflowVersion: (...args: unknown[]) => {
      calls.push({ method: "proposeWorkflowVersion", args });
      return Promise.resolve({ version: 3 });
    },
  };
}

function makeClients(
  north: ReturnType<typeof makeNorth>,
  south: ReturnType<typeof makeSouth>,
) {
  return { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;
}

test("CP20 routing: proposeWorkflow uses the NORTH client when a bearer token is present", async () => {
  // WHY: proposeWorkflow is a user-initiated action in interactive chat. When a
  // token is available the broker uses the OIDC-bound subject for authz —
  // north.proposeWorkflowVersion must be called.
  const north = makeNorth();
  const south = makeSouth();
  const clients = makeClients(north, south);

  const identity = {
    token: "oidc-bearer",
    tenantId: "11111111-1111-1111-1111-111111111111",
    userId: "alice@example.com",
    agentId: "alice-agent",
    ownerGrant: "should-not-be-used",
  };
  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);

  const result = await bridge.proposeWorkflow("lineage-1", { name: "test", steps: [] });

  assert.equal(result.ok, true, "proposeWorkflow must succeed");
  assert.ok(
    north.calls.some((c: CallLog) => c.method === "proposeWorkflowVersion"),
    "north.proposeWorkflowVersion must be called when a bearer token is present",
  );
  assert.equal(
    south.calls.filter((c: CallLog) => c.method === "proposeWorkflowVersion").length,
    0,
    "south.proposeWorkflowVersion must NOT be called on the north path",
  );
});

test("CP20 routing: proposeWorkflow uses the SOUTH client with ownerGrant when no token", async () => {
  // WHY: scheduled/unattended runs have no OIDC bearer. The south path uses
  // SPIFFE mTLS + ownerGrant — same pattern as saveWorkflow.
  const north = makeNorth();
  const south = makeSouth();
  const clients = makeClients(north, south);

  const identity = {
    // no token — south path
    tenantId: "11111111-1111-1111-1111-111111111111",
    userId: "alice@example.com",
    agentId: "alice-agent",
    ownerGrant: "test-grant-value",
  };
  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);

  const result = await bridge.proposeWorkflow("lineage-2", { name: "test", steps: [] });

  assert.equal(result.ok, true, "proposeWorkflow must succeed on south path");
  assert.ok(
    south.calls.some((c: CallLog) => c.method === "proposeWorkflowVersion"),
    "south.proposeWorkflowVersion must be called when no bearer token is present",
  );
  const southCall = south.calls.find((c: CallLog) => c.method === "proposeWorkflowVersion");
  const req = (southCall!.args[0] as Record<string, unknown>);
  assert.equal(req.ownerGrant, "test-grant-value", "ownerGrant must be forwarded to south proposeWorkflowVersion");
  assert.equal(
    north.calls.filter((c: CallLog) => c.method === "proposeWorkflowVersion").length,
    0,
    "north.proposeWorkflowVersion must NOT be called on the south path",
  );
});

// ── 4. IPC round-trip test ────────────────────────────────────────────────────

function withTimeout<T>(p: Promise<T>, ms: number, label: string): Promise<T> {
  return Promise.race([
    p,
    new Promise<never>((_, reject) =>
      setTimeout(() => reject(new Error(`timeout: ${label}`)), ms),
    ),
  ]);
}

async function flushAsync(): Promise<void> {
  for (let i = 0; i < 5; i++) {
    await new Promise((r) => setImmediate(r));
  }
}

const TEST_RUN_ID = "test-run-prop-001";

function makeWiredPair(bridge: BridgeLike): { server: RemoteBridgeServer; child: ChildLink } {
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const server = new RemoteBridgeServer(parentLink, (_identity, _approver) => bridge);
  server.setRunContext(TEST_RUN_ID, { tenantId: "t1", userId: "u1", agentId: "a1" }, async () => true);
  const child = new ChildLink(childSide);
  return { server, child };
}

test("CP20 IPC round-trip: propose-workflow request is dispatched to the bridge and returns a result", async () => {
  // WHY: the IPC seam is the boundary between the forked child and the parent
  // bridge. A missing handler registration means the request is silently lost
  // and the child hangs. This test proves the full round-trip works.
  const spyBridge: BridgeLike = {
    gate: async () => ({ allow: true }),
    execute: async () => ({ ok: true, output: null }),
    delegate: async () => ({ ok: true }),
    saveWorkflow: async () => ({ ok: true }),
    runWorkflow: async () => ({ ok: true, result: null }),
    listWorkflows: async () => ({ ok: true, items: [] }),
    publishWorkflow: async () => ({ ok: true }),
    proposeWorkflow: async (lineageId, def) => {
      return { ok: true, version: 7, _lineageId: lineageId, _def: def } as { ok: true; version: number };
    },
    analyzeImage: async () => ({ ok: true, text: "" }),
    scheduleWorkflow: async () => ({ ok: true }),
  };

  const { child } = makeWiredPair(spyBridge);
  const seq = makeSeq()();

  const req: IpcProposeRequest = {
    kind: "propose-workflow",
    seq,
    runId: TEST_RUN_ID,
    lineageId: "lineage-ipc-test",
    def: { name: "test", steps: [] },
  };

  const replyP = withTimeout(
    child.request<ProposeWorkflowResult>(req, "propose-workflow-result"),
    500,
    "propose-workflow round-trip",
  );

  await flushAsync();
  const reply = await replyP;

  assert.equal(reply.seq, seq, "reply must echo the request seq");
  assert.equal(reply.ok, true, "reply must indicate success");
  assert.equal(reply.version, 7);
});

test("CP20 IPC round-trip: unknown runId returns ok:false without crashing", async () => {
  // WHY: fail-closed invariant. A child sending a fabricated runId must get a
  // clean error reply rather than causing an unhandled rejection in the parent.
  const spyBridge: BridgeLike = {
    gate: async () => ({ allow: true }),
    execute: async () => ({ ok: true, output: null }),
    delegate: async () => ({ ok: true }),
    saveWorkflow: async () => ({ ok: true }),
    runWorkflow: async () => ({ ok: true, result: null }),
    listWorkflows: async () => ({ ok: true, items: [] }),
    publishWorkflow: async () => ({ ok: true }),
    proposeWorkflow: async () => ({ ok: true, version: 1 }),
    analyzeImage: async () => ({ ok: true, text: "" }),
    scheduleWorkflow: async () => ({ ok: true }),
  };

  const { child } = makeWiredPair(spyBridge);
  const seq = makeSeq()();

  const req: IpcProposeRequest = {
    kind: "propose-workflow",
    seq,
    runId: "fabricated-run-id-does-not-exist",
    lineageId: "lineage-x",
    def: {},
  };

  const replyP = withTimeout(
    child.request<ProposeWorkflowResult>(req, "propose-workflow-result"),
    500,
    "unknown runId propose round-trip",
  );

  await flushAsync();
  const reply = await replyP;

  assert.equal(reply.seq, seq);
  assert.equal(reply.ok, false, "unknown runId must fail closed");
  assert.ok(reply.error, "error field must be set");
});
