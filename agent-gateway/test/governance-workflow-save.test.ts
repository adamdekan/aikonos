// Regression test for the live bug: workflow_save Pi tool passes flat params to
// GovernanceBridge.saveWorkflow, which must send a CANONICAL WorkflowDef as
// definitionJson to the broker — not the raw flat params.
//
// WHY: the broker's SaveWorkflow RPC validates that definitionJson contains a
// document with apiVersion == "aikonos.com/v1" and kind == "Workflow". When the
// flat tool params (no envelope) were forwarded directly the broker rejected
// with: "workflow: apiVersion must be 'aikonos.com/v1', got ''".
import { test } from "node:test";
import assert from "node:assert/strict";
import { GovernanceBridge, invalidSkillError } from "../src/broker/governance.js";

// ── Minimal stubs (mirrors governance-south.test.ts pattern) ─────────────────

interface SaveWorkflowCall {
  req: Record<string, unknown>;
}

function makeNorthWithWorkflow() {
  const saveCalls: SaveWorkflowCall[] = [];
  return {
    saveCalls,
    saveWorkflow: (req: Record<string, unknown>, _token: string | undefined) => {
      saveCalls.push({ req });
      return Promise.resolve({ workflowId: "wf-1", lineageId: "lin-1", version: 1 });
    },
    // Unused stubs satisfy the north client interface:
    createTask: () => Promise.resolve({ taskId: "t-1" }),
    approveTask: () => Promise.resolve({ capabilityTokenIds: {} }),
    sendEnvelope: () => Promise.resolve({ envelopeId: "env-1" }),
    getWorkflow: () => Promise.resolve({ definitionJson: "{}" }),
    listWorkflows: () => Promise.resolve({ items: [] }),
    publishWorkflow: () => Promise.resolve({ visibilityKind: "private", groups: [] }),
    proposeWorkflowVersion: () => Promise.resolve({ version: 2 }),
  };
}

function makeSouthStub() {
  return {
    saveWorkflow: () => Promise.resolve({ workflowId: "wf-1", lineageId: "lin-1", version: 1 }),
    createGatewayTask: () => Promise.resolve({ taskId: "t-1" }),
    approveGatewayTask: () => Promise.resolve({ capabilityTokenIds: {} }),
    submitPlan: () => Promise.resolve({ outcome: 1, capabilityTokenIds: { 1: "tok" }, violations: [], steps: [] }),
    invokeTool: () => Promise.resolve({ success: true, result: null, error: "", costUnitsConsumed: 0 }),
    emitStatus: () => Promise.resolve(),
    getWorkflow: () => Promise.resolve({ definitionJson: "{}" }),
    listWorkflows: () => Promise.resolve({ items: [] }),
    publishWorkflow: () => Promise.resolve({ visibilityKind: "private", groups: [] }),
  };
}

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

// ── Regression test ───────────────────────────────────────────────────────────

test("GovernanceBridge.saveWorkflow: definitionJson sent to broker is canonical (has apiVersion + kind)", async () => {
  // WHY: this is the live regression guard. When the Pi tool calls saveWorkflow
  // with flat params the bridge must wrap them into a canonical WorkflowDef before
  // serialising to definitionJson. If the raw flat params are forwarded the broker
  // rejects with "apiVersion must be 'aikonos.com/v1', got ''".
  const north = makeNorthWithWorkflow();
  const south = makeSouthStub();
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;

  const identity = {
    token: "bearer-tok",
    tenantId: "11111111-1111-1111-1111-111111111111",
    userId: "alice@example.com",
    agentId: "alice-agent",
  };

  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);

  // These are the raw flat params the workflow_save Pi tool passes in.
  const flatParams = {
    name: "demo-fetch",
    description: "fetch a URL on a schedule",
    steps: [{ skill: "web.fetch", args: { url: "https://example.com" } }],
    inputs: [{ name: "since", default: "-7d" }],
  };

  const result = await bridge.saveWorkflow(flatParams);

  assert.equal(result.ok, true, `saveWorkflow must succeed, got error: ${result.error}`);
  assert.equal(north.saveCalls.length, 1, "north.saveWorkflow must be called once");

  const req = north.saveCalls[0].req;
  assert.ok(req, "saveWorkflow request must be recorded");

  // The definitionJson field must be a parseable canonical document.
  const defJsonStr = req.definitionJson;
  assert.equal(typeof defJsonStr, "string", "definitionJson must be a string");

  const parsed: unknown = JSON.parse(String(defJsonStr));
  assert.ok(
    parsed !== null && typeof parsed === "object",
    "definitionJson must parse to an object",
  );

  const doc = parsed as Record<string, unknown>;

  // These are the exact fields the broker validator checks.
  assert.equal(
    doc.apiVersion,
    "aikonos.com/v1",
    `definitionJson.apiVersion must be "aikonos.com/v1", got "${doc.apiVersion}" — this is the live bug`,
  );
  assert.equal(
    doc.kind,
    "Workflow",
    `definitionJson.kind must be "Workflow", got "${doc.kind}"`,
  );
});

test("GovernanceBridge.saveWorkflow north: RPC name/description match canonical metadata", async () => {
  // WHY: the RPC-level name and description fields must be consistent with
  // what is inside definitionJson so the broker's own DB columns stay in sync.
  const north = makeNorthWithWorkflow();
  const south = makeSouthStub();
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;

  const identity = {
    token: "bearer-tok",
    tenantId: "11111111-1111-1111-1111-111111111111",
    userId: "alice@example.com",
    agentId: "alice-agent",
  };

  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);

  await bridge.saveWorkflow({
    name: "my-workflow",
    description: "does stuff",
    steps: [{ skill: "web.fetch", args: {} }],
  });

  const req = north.saveCalls[0].req;
  assert.ok(req, "saveWorkflow request must be recorded");
  assert.equal(req.name, "my-workflow", "RPC name must match canonical metadata.name");
  assert.equal(req.description, "does stuff", "RPC description must match canonical metadata.description");
});

test("GovernanceBridge.saveWorkflow north: no-description → empty string in RPC description field", async () => {
  // WHY: the broker's proto field is a non-optional string; when description is
  // absent in the tool params the RPC field must be "" not undefined.
  const north = makeNorthWithWorkflow();
  const south = makeSouthStub();
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;

  const identity = {
    token: "bearer-tok",
    tenantId: "11111111-1111-1111-1111-111111111111",
    userId: "alice@example.com",
    agentId: "alice-agent",
  };

  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);

  await bridge.saveWorkflow({
    name: "no-desc-wf",
    steps: [{ skill: "web.fetch", args: {} }],
  });

  const req = north.saveCalls[0].req;
  assert.ok(req, "saveWorkflow request must be recorded");
  assert.equal(req.description, "", "RPC description must be empty string when not provided");
});

// ── Invented-tool guard ───────────────────────────────────────────────────────

test("GovernanceBridge.saveWorkflow: rejects a step referencing an invented skill, never calls the broker", async () => {
  // WHY the live bug: the model composed workflows from tools that don't exist
  // (data.transform, template.render, chat.output). Persisting them means every
  // such step is denied at run time and the whole workflow is rejected. The bridge
  // must refuse to save a definition whose steps reference unknown skills — and
  // must not reach the broker at all.
  const north = makeNorthWithWorkflow();
  const south = makeSouthStub();
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;

  const identity = {
    token: "bearer-tok",
    tenantId: "11111111-1111-1111-1111-111111111111",
    userId: "alice@example.com",
    agentId: "alice-agent",
  };

  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);

  const result = await bridge.saveWorkflow({
    name: "hallucinated-wf",
    steps: [
      { skill: "doc.read", args: { path: "x.csv" } },
      { skill: "data.transform", args: {} },
      { skill: "template.render", args: {} },
    ],
  });

  assert.equal(result.ok, false, "save must be rejected");
  // The "unknown skill(s): …" clause names only the invented skills; the valid
  // tool-id list that follows is guidance (and legitimately contains doc.read).
  const flagged = result.error?.split("Every step must use")[0] ?? "";
  assert.ok(flagged.includes("data.transform"), `error must name the invented skill; got: ${result.error}`);
  assert.ok(flagged.includes("template.render"), "error must name every invented skill");
  assert.ok(!flagged.includes("doc.read"), "the valid skill must not be flagged as unknown");
  assert.equal(north.saveCalls.length, 0, "broker must NOT be called when a skill is invalid");
});

test("GovernanceBridge.saveWorkflow: rejects a step referencing skill:vision, never calls the broker", async () => {
  // WHY (F7/CP6 gap): analyze_image resolves through mapTool (toolId: "vision")
  // so its own tool_call gets real per-call FGA gating — but that same
  // resolvability makes "vision" look like a valid workflow-step skill too. It
  // has no Tool Proxy registration to route to via InvokeTool, so a saved step
  // referencing it would fail at RUN time instead of being cleanly rejected at
  // authoring time. Must produce the same "unknown skill(s)" error a genuinely
  // invented skill name gets — not a silent pass-through.
  const north = makeNorthWithWorkflow();
  const south = makeSouthStub();
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;

  const identity = {
    token: "bearer-tok",
    tenantId: "11111111-1111-1111-1111-111111111111",
    userId: "alice@example.com",
    agentId: "alice-agent",
  };

  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);

  const result = await bridge.saveWorkflow({
    name: "vision-step-wf",
    steps: [
      { skill: "doc.read", args: { path: "x.csv" } },
      { skill: "vision", args: { path: "references/apple.png" } },
    ],
  });

  assert.equal(result.ok, false, "save must be rejected");
  const flagged = result.error?.split("Every step must use")[0] ?? "";
  assert.ok(flagged.includes("vision"), `error must name "vision" as an unknown skill; got: ${result.error}`);
  assert.ok(!flagged.includes("doc.read"), "the valid skill must not be flagged as unknown");
  assert.equal(north.saveCalls.length, 0, "broker must NOT be called when a step references skill:vision");
});

test("GovernanceBridge.saveWorkflow: a definition of only valid skills still saves", async () => {
  // Guard against the validator over-rejecting: real tool ids must pass through.
  const north = makeNorthWithWorkflow();
  const south = makeSouthStub();
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;

  const identity = {
    token: "bearer-tok",
    tenantId: "11111111-1111-1111-1111-111111111111",
    userId: "alice@example.com",
    agentId: "alice-agent",
  };

  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);

  const result = await bridge.saveWorkflow({
    name: "valid-wf",
    steps: [
      { skill: "doc.read", args: { path: "x.csv" } },
      { skill: "web.fetch", args: { url: "https://example.com" } },
    ],
  });

  assert.equal(result.ok, true, `valid workflow must save; got error: ${result.error}`);
  assert.equal(north.saveCalls.length, 1, "broker must be called for a valid workflow");
});

// ── Reason step (CP-R2) ───────────────────────────────────────────────────────

test("invalidSkillError: skips reason steps, validates tool steps as today", () => {
  const err = invalidSkillError([
    { kind: "tool", skill: "doc.read" },
    { kind: "reason", skill: "" },
    { skill: "web.fetch" },
  ]);
  assert.equal(err, null, "a mixed definition of valid tool steps + a reason step must pass");
});

test("invalidSkillError: an invented tool skill is still rejected, and the message teaches the reason-step escape hatch", () => {
  const err = invalidSkillError([
    { kind: "tool", skill: "doc.read" },
    { kind: "reason", skill: "" },
    { skill: "data.transform" },
  ]);
  assert.ok(err, "invented tool skill must still be rejected");
  assert.ok(err?.includes("data.transform"), "error must name the invented skill");
  assert.ok(
    err?.includes('kind: "reason"'),
    `error must teach the reason-step escape hatch; got: ${err}`,
  );
});

test("GovernanceBridge.saveWorkflow: a definition mixing tool and reason steps saves successfully", async () => {
  const north = makeNorthWithWorkflow();
  const south = makeSouthStub();
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;

  const identity = {
    token: "bearer-tok",
    tenantId: "11111111-1111-1111-1111-111111111111",
    userId: "alice@example.com",
    agentId: "alice-agent",
  };

  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);

  const result = await bridge.saveWorkflow({
    name: "ip-alert",
    steps: [
      { skill: "doc.read", args: { path: "registry.csv" } },
      {
        kind: "reason",
        instruction: "Find the row whose CIDR contains ${inputs.ip}: ${steps.0.output}",
      },
      { skill: "doc.write", args: { path: "alert.txt" } },
    ],
  });

  assert.equal(result.ok, true, `mixed tool+reason workflow must save; got error: ${result.error}`);
  assert.equal(north.saveCalls.length, 1, "broker must be called for a valid mixed workflow");
});

test("GovernanceBridge.proposeWorkflow: a definition mixing tool and reason steps proposes successfully", async () => {
  const north = makeNorthWithWorkflow();
  const south = makeSouthStub();
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;

  const identity = {
    token: "bearer-tok",
    tenantId: "11111111-1111-1111-1111-111111111111",
    userId: "alice@example.com",
    agentId: "alice-agent",
  };

  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);

  const result = await bridge.proposeWorkflow("lin-1", {
    steps: [
      { skill: "doc.read", args: { path: "registry.csv" } },
      { kind: "reason", instruction: "Summarize the file" },
    ],
  });

  assert.equal(result.ok, true, `mixed tool+reason proposal must succeed; got error: ${result.error}`);
});

// ── F9: agent-binding threading ─────────────────────────────────────────────

const BOUND_AGENT_UUID = "550e8400-e29b-41d4-a716-446655440000";

function makeSouthWithSaveRecorder() {
  const saveCalls: SaveWorkflowCall[] = [];
  const south = {
    ...makeSouthStub(),
    saveWorkflow: (req: Record<string, unknown>) => {
      saveCalls.push({ req });
      return Promise.resolve({ workflowId: "wf-1", lineageId: "lin-1", version: 1 });
    },
  };
  return { south, saveCalls };
}

test('GovernanceBridge.saveWorkflow north: threads agentId when identity.agentId is "agent:<uuid>"', async () => {
  // WHY (F9): a named-agent session binds the brand-new workflow to that agent so
  // its runs use the agent's own skills. Identity carries "agent:<uuid>"; the RPC
  // must send the bare UUID.
  const north = makeNorthWithWorkflow();
  const south = makeSouthStub();
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;

  const identity = {
    token: "bearer-tok",
    tenantId: "11111111-1111-1111-1111-111111111111",
    userId: "alice@example.com",
    agentId: `agent:${BOUND_AGENT_UUID}`,
  };

  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);
  await bridge.saveWorkflow({ name: "bound-wf", steps: [{ skill: "web.fetch", args: {} }] });

  assert.equal(north.saveCalls[0].req.agentId, BOUND_AGENT_UUID, "north save must carry the bare agent UUID");
});

test('GovernanceBridge.saveWorkflow north: agentId is "" for a synthetic personal-session id', async () => {
  // WHY (F9): a personal chat's synthetic "alice-agent" id must NEVER bind a
  // workflow — the RPC agentId field must be empty so the broker treats it as
  // personal (unbound).
  const north = makeNorthWithWorkflow();
  const south = makeSouthStub();
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;

  const identity = {
    token: "bearer-tok",
    tenantId: "11111111-1111-1111-1111-111111111111",
    userId: "alice@example.com",
    agentId: "alice-agent",
  };

  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);
  await bridge.saveWorkflow({ name: "personal-wf", steps: [{ skill: "web.fetch", args: {} }] });

  assert.equal(north.saveCalls[0].req.agentId, "", "synthetic personal-session id must not bind (empty agentId)");
});

test("GovernanceBridge.saveWorkflow south: threads the bound agent UUID on the scheduled/grant path", async () => {
  const north = makeNorthWithWorkflow();
  const { south, saveCalls } = makeSouthWithSaveRecorder();
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;

  // No token → the bridge takes the south/grant path.
  const identity = {
    tenantId: "11111111-1111-1111-1111-111111111111",
    userId: "alice@example.com",
    agentId: `agent:${BOUND_AGENT_UUID}`,
    ownerGrant: "grant-x",
  };

  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);
  await bridge.saveWorkflow({ name: "bound-wf", steps: [{ skill: "web.fetch", args: {} }] });

  assert.equal(north.saveCalls.length, 0, "no-token identity must not use the north path");
  assert.equal(saveCalls.length, 1, "south save must be called");
  assert.equal(saveCalls[0].req.agentId, BOUND_AGENT_UUID, "south save must carry the bare agent UUID");
});
