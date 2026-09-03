// CP5c tests: workflow_publish Pi tool — gating, bridge wiring, IPC round-trip.
//
// WHY these tests exist:
//   1. Gating: allowedPiToolNames with 'workflows' skill must include
//      workflow_publish (alongside save/run/list). Without it the tool is
//      unreachable from chat regardless of skill grant.
//   2. Tool→bridge wiring: workflow_publish must call bridge.publishWorkflow
//      with exactly lineageId, groupIds, version. A mismatch here means the
//      broker receives a wrong or incomplete publish request.
//   3. NORTH/SOUTH routing: GovernanceBridge.publishWorkflow must call the OIDC
//      north client when a bearer token is present, and the SPIFFE south client
//      (with ownerGrant) when absent. Mirrors the saveWorkflow routing test.
//   4. IPC round-trip: a publish-workflow request sent over the fake paired
//      channel must arrive at the RemoteBridgeServer, be dispatched to the
//      bound bridge, and produce a publish-workflow-result reply.
//
// Intent encoded: "publishing a workflow is reachable from chat as a gated tool
// and routes north under OIDC, south under ownerGrant."
import { test } from "node:test";
import assert from "node:assert/strict";

import { allowedPiToolNames, computeActiveToolNames } from "../src/pi/session.js";
import { makeTools, TOOL_NAMES } from "../src/pi/tools.js";
import type { ToolDefinition } from "@earendil-works/pi-coding-agent";
import type { BridgeClientLike } from "../src/ipc/bridge-client.js";
import type { Approver } from "../src/broker/governance.js";
import { GovernanceBridge } from "../src/broker/governance.js";
import { makePairedChannel, makeSeq, ParentLink, ChildLink } from "../src/ipc/protocol.js";
import type { PublishWorkflowRequest as IpcPublishRequest, PublishWorkflowResult } from "../src/ipc/protocol.js";
import { RemoteBridgeServer } from "../src/ipc/bridge-server.js";
import type { BridgeLike } from "../src/ipc/bridge-server.js";

// ── Fake bridge (mirrors WorkflowFakeBridge in workflow-tools.test.ts) ─────────

interface PublishFakeBridge extends BridgeClientLike {
  publishWorkflowCalls: Array<{ lineageId: string; groupIds: string[]; version: number }>;
}

function makePublishFakeBridge(): PublishFakeBridge {
  const bridge: PublishFakeBridge = {
    publishWorkflowCalls: [],

    async gate() { return { allow: true }; },
    async execute() { return { ok: true, output: null }; },
    async delegate() { return { ok: true }; },
    setApprover(_a: Approver) {},
    setToken(_t?: string) {},
    usageIdentity() { return { tenantId: "", userId: "", agentId: "" }; },

    async saveWorkflow() { return { ok: true }; },
    async runWorkflow() { return { ok: true, result: null }; },
    async listWorkflows() { return { ok: true, items: [] }; },

    async publishWorkflow(lineageId: string, groupIds: string[], version: number) {
      bridge.publishWorkflowCalls.push({ lineageId, groupIds, version });
      return { ok: true, visibilityKind: "group", groups: groupIds };
    },
    async proposeWorkflow() { return { ok: true }; },
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

test("CP5c gating: allowedPiToolNames WITH 'workflows' skill includes workflow_publish", () => {
  // WHY: workflow_publish must surface alongside save/run/list when the user
  // holds the workflows skill — it is part of the same skill grant.
  const allowed = allowedPiToolNames(["workflows"]);
  assert.equal(
    allowed.has("workflow_publish"),
    true,
    "workflow_publish must be in allowed set when 'workflows' skill is held",
  );
});

test("CP5c gating: allowedPiToolNames WITHOUT 'workflows' skill does not include workflow_publish", () => {
  // WHY: deny-by-default. A user without the workflows skill must not see
  // workflow_publish.
  const allowed = allowedPiToolNames(["web.fetch"]);
  assert.equal(
    allowed.has("workflow_publish"),
    false,
    "workflow_publish must NOT be in allowed set without 'workflows' skill",
  );
});

test("CP5c gating: computeActiveToolNames with workflows skill includes workflow_publish", () => {
  // WHY: computeActiveToolNames is what buildSession uses for the final tool
  // name list. All four workflow tools must propagate.
  const active = computeActiveToolNames(TOOL_NAMES, [], undefined, ["workflows"]);
  assert.equal(
    active.includes("workflow_publish"),
    true,
    "workflow_publish must appear in computeActiveToolNames result with 'workflows' skill",
  );
});

test("CP5c gating: TOOL_NAMES includes workflow_publish", () => {
  // WHY: TOOL_NAMES is the authoritative list fed to computeActiveToolNames as
  // allStaticNames. If absent here it can never appear in any session.
  assert.equal(
    TOOL_NAMES.includes("workflow_publish"),
    true,
    "workflow_publish must be present in TOOL_NAMES",
  );
});

// ── 2. Tool→bridge wiring tests ───────────────────────────────────────────────

test("CP5c wiring: workflow_publish is present in makeTools() output", () => {
  const bridge = makePublishFakeBridge();
  const tools = makeTools(bridge);
  const publishTool = tools.find((t) => t.name === "workflow_publish");
  assert.ok(publishTool, "workflow_publish tool must be in makeTools() output");
});

test("CP5c wiring: workflow_publish execute() calls bridge.publishWorkflow with lineageId, groupIds, version", async () => {
  // WHY: the tool definition must forward lineageId, groupIds, and version to
  // the bridge exactly. A mismatch means the broker gets an incomplete request.
  const bridge = makePublishFakeBridge();
  const tools = makeTools(bridge);
  const publishTool = tools.find((t) => t.name === "workflow_publish");
  assert.ok(publishTool, "workflow_publish tool must be present");

  await exec(publishTool, "tc-pub-1", {
    lineageId: "lineage-abc",
    groupIds: ["security-team", "ops-team"],
    version: 3,
  });

  assert.equal(bridge.publishWorkflowCalls.length, 1, "bridge.publishWorkflow must be called once");
  const call = bridge.publishWorkflowCalls[0];
  assert.ok(call, "publishWorkflow call must be recorded");
  assert.equal(call.lineageId, "lineage-abc");
  assert.deepEqual(call.groupIds, ["security-team", "ops-team"]);
  assert.equal(call.version, 3);
});

test("CP5c wiring: workflow_publish execute() uses version 0 when omitted", async () => {
  // WHY: version is optional in the tool schema. The bridge must receive 0
  // (the broker interprets 0 as current-latest) rather than undefined.
  const bridge = makePublishFakeBridge();
  const tools = makeTools(bridge);
  const publishTool = tools.find((t) => t.name === "workflow_publish");
  assert.ok(publishTool, "workflow_publish tool must be present");

  await exec(publishTool, "tc-pub-2", {
    lineageId: "lineage-xyz",
    groupIds: ["security-team"],
  });

  const call = bridge.publishWorkflowCalls[0];
  assert.ok(call, "publishWorkflow call must be recorded");
  assert.equal(call.version, 0, "omitted version must default to 0");
});

test("CP5c wiring: workflow_publish returns formatted success text", async () => {
  // WHY: the result text is what the LLM reads back. It must mention which
  // groups the workflow was published to and not start with ERROR:.
  const bridge = makePublishFakeBridge();
  const tools = makeTools(bridge);
  const publishTool = tools.find((t) => t.name === "workflow_publish");
  assert.ok(publishTool, "workflow_publish tool must be present");

  const result = await exec(publishTool, "tc-pub-ok", {
    lineageId: "lineage-1",
    groupIds: ["security-team"],
    version: 1,
  });

  assert.ok(result.content.length > 0, "result must have content");
  const first = result.content[0];
  assert.ok(first, "result must have at least one content item");
  const text = first.type === "text" ? (first.text ?? "") : "";
  assert.ok(text.length > 0, "result text must be non-empty");
  assert.ok(!text.startsWith("ERROR:"), `result must not be an error on success, got: ${text}`);
});

test("CP5c wiring: workflow_publish returns ERROR text on bridge failure", async () => {
  // WHY: the model must see an ERROR: prefix so it can report failure rather
  // than silently treating an empty/undefined response as success.
  const bridge = makePublishFakeBridge();
  // Override publishWorkflow to return failure
  bridge.publishWorkflow = async () => ({ ok: false, error: "not a member of that group" });
  const tools = makeTools(bridge);
  const publishTool = tools.find((t) => t.name === "workflow_publish");
  assert.ok(publishTool, "workflow_publish tool must be present");

  const result = await exec(publishTool, "tc-pub-fail", {
    lineageId: "lineage-1",
    groupIds: ["unknown-group"],
    version: 1,
  });

  const first = result.content[0];
  assert.ok(first, "result must have at least one content item");
  const text = first.type === "text" ? (first.text ?? "") : "";
  assert.ok(text.startsWith("ERROR:"), `result must start with ERROR: on failure, got: ${text}`);
});

// ── 3. North/south routing tests ──────────────────────────────────────────────
//
// Mirror the patterns from governance-owner-grant.test.ts.

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
  };
}

function makeClients(
  north: ReturnType<typeof makeNorth>,
  south: ReturnType<typeof makeSouth>,
) {
  return { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;
}

test("CP5c routing: publishWorkflow uses the NORTH client when a bearer token is present", async () => {
  // WHY: publishWorkflow is a user-initiated action in interactive chat. When
  // a token is available the broker needs the OIDC-bound subject to verify
  // group membership (FGA) — so north.publishWorkflow must be called.
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

  const result = await bridge.publishWorkflow("lineage-1", ["security-team"], 2);

  assert.equal(result.ok, true, "publishWorkflow must succeed");
  assert.ok(
    north.calls.some((c: CallLog) => c.method === "publishWorkflow"),
    "north.publishWorkflow must be called when a bearer token is present",
  );
  assert.equal(
    south.calls.filter((c: CallLog) => c.method === "publishWorkflow").length,
    0,
    "south.publishWorkflow must NOT be called on the north path",
  );
});

test("CP5c routing: publishWorkflow uses the SOUTH client with ownerGrant when no token", async () => {
  // WHY: scheduled/unattended runs have no OIDC bearer. The south path uses
  // SPIFFE mTLS + ownerGrant as the trust anchor — same as saveWorkflow.
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

  const result = await bridge.publishWorkflow("lineage-2", ["ops-team"], 0);

  assert.equal(result.ok, true, "publishWorkflow must succeed on south path");
  assert.ok(
    south.calls.some((c: CallLog) => c.method === "publishWorkflow"),
    "south.publishWorkflow must be called when no bearer token is present",
  );
  const southCall = south.calls.find((c: CallLog) => c.method === "publishWorkflow");
  const req = (southCall!.args[0] as Record<string, unknown>);
  assert.equal(req.ownerGrant, "test-grant-value", "ownerGrant must be forwarded to south publishWorkflow");
  assert.equal(
    north.calls.filter((c: CallLog) => c.method === "publishWorkflow").length,
    0,
    "north.publishWorkflow must NOT be called on the south path",
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

const TEST_RUN_ID = "test-run-pub-001";

function makeWiredPair(bridge: BridgeLike): { server: RemoteBridgeServer; child: ChildLink } {
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const server = new RemoteBridgeServer(parentLink, (_identity, _approver) => bridge);
  server.setRunContext(TEST_RUN_ID, { tenantId: "t1", userId: "u1", agentId: "a1" }, async () => true);
  const child = new ChildLink(childSide);
  return { server, child };
}

test("CP5c IPC round-trip: publish-workflow request is dispatched to the bridge and returns a result", async () => {
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
    publishWorkflow: async (lineageId, groupIds, version) => {
      return { ok: true, visibilityKind: "group", groups: groupIds, _lineageId: lineageId, _version: version };
    },
    proposeWorkflow: async () => ({ ok: true }),
    analyzeImage: async () => ({ ok: true, text: "a red apple" }),
    scheduleWorkflow: async () => ({ ok: true }),
  };

  const { child } = makeWiredPair(spyBridge);
  const seq = makeSeq()();

  const req: IpcPublishRequest = {
    kind: "publish-workflow",
    seq,
    runId: TEST_RUN_ID,
    lineageId: "lineage-ipc-test",
    groupIds: ["security-team"],
    version: 5,
  };

  const replyP = withTimeout(
    child.request<PublishWorkflowResult>(req, "publish-workflow-result"),
    500,
    "publish-workflow round-trip",
  );

  await flushAsync();
  const reply = await replyP;

  assert.equal(reply.seq, seq, "reply must echo the request seq");
  assert.equal(reply.ok, true, "reply must indicate success");
  assert.equal(reply.visibilityKind, "group");
  assert.deepEqual(reply.groups, ["security-team"]);
});

test("CP5c IPC round-trip: unknown runId returns ok:false without crashing", async () => {
  // WHY: fail-closed invariant. A child sending a fabricated runId must get a
  // clean error reply rather than causing an unhandled rejection in the parent.
  const spyBridge: BridgeLike = {
    gate: async () => ({ allow: true }),
    execute: async () => ({ ok: true, output: null }),
    delegate: async () => ({ ok: true }),
    saveWorkflow: async () => ({ ok: true }),
    runWorkflow: async () => ({ ok: true, result: null }),
    listWorkflows: async () => ({ ok: true, items: [] }),
    publishWorkflow: async () => ({ ok: true, visibilityKind: "group", groups: [] }),
    proposeWorkflow: async () => ({ ok: true }),
    analyzeImage: async () => ({ ok: true, text: "a red apple" }),
    scheduleWorkflow: async () => ({ ok: true }),
  };

  const { child } = makeWiredPair(spyBridge);
  const seq = makeSeq()();

  const req: IpcPublishRequest = {
    kind: "publish-workflow",
    seq,
    runId: "fabricated-run-id-does-not-exist",
    lineageId: "lineage-x",
    groupIds: [],
    version: 0,
  };

  const replyP = withTimeout(
    child.request<PublishWorkflowResult>(req, "publish-workflow-result"),
    500,
    "unknown runId publish round-trip",
  );

  await flushAsync();
  const reply = await replyP;

  assert.equal(reply.seq, seq);
  assert.equal(reply.ok, false, "unknown runId must fail closed");
  assert.ok(reply.error, "error field must be set");
});
