// CP3 tests: owner-grant threading through the south path.
//
// WHY these tests exist: before CP2/CP3 the gateway could assert any owner
// via the free-form ownerUserId field on createGatewayTask. CP2 makes the
// broker require a broker-minted HMAC grant; CP3 makes the gateway carry it.
// These tests encode that the grant flows end-to-end from the Identity through
// the GovernanceBridge into the createGatewayTask call — and that the north
// (bearer) path is unaffected.
import { test } from "node:test";
import assert from "node:assert/strict";
import { GovernanceBridge } from "../src/broker/governance.js";
import { ValidationOutcome } from "../gen/ts/proto/plan";

// ── Stubs ─────────────────────────────────────────────────────────────────────

function taskHandle(taskId = "task-1") {
  return { taskId };
}

function approvedPlan(capToken = "tok-1") {
  return {
    outcome: ValidationOutcome.APPROVED,
    capabilityTokenIds: { 1: capToken },
    violations: [],
    steps: [],
  };
}

interface CallLog {
  method: string;
  args: unknown[];
}

function makeNorth() {
  const calls: CallLog[] = [];
  return {
    calls,
    createTask: (...args: unknown[]) => {
      calls.push({ method: "createTask", args });
      return Promise.resolve(taskHandle());
    },
    approveTask: (...args: unknown[]) => {
      calls.push({ method: "approveTask", args });
      return Promise.resolve({ capabilityTokenIds: { 1: "tok-north" } });
    },
    sendEnvelope: (...args: unknown[]) => {
      calls.push({ method: "sendEnvelope", args });
      return Promise.resolve({ envelopeId: "env-1" });
    },
  };
}

function makeSouth() {
  const calls: CallLog[] = [];
  return {
    calls,
    createGatewayTask: (...args: unknown[]) => {
      calls.push({ method: "createGatewayTask", args });
      return Promise.resolve(taskHandle());
    },
    approveGatewayTask: (...args: unknown[]) => {
      calls.push({ method: "approveGatewayTask", args });
      return Promise.resolve({ capabilityTokenIds: { 1: "tok-south" } });
    },
    submitPlan: (...args: unknown[]) => {
      calls.push({ method: "submitPlan", args });
      return Promise.resolve(approvedPlan());
    },
    invokeTool: (...args: unknown[]) => {
      calls.push({ method: "invokeTool", args });
      return Promise.resolve({ success: true, result: "ok", error: "", costUnitsConsumed: 0 });
    },
    emitStatus: (...args: unknown[]) => {
      calls.push({ method: "emitStatus", args });
      return Promise.resolve();
    },
  };
}

function makeClients(
  north: ReturnType<typeof makeNorth>,
  south: ReturnType<typeof makeSouth>,
) {
  return { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;
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

// ── Tests ─────────────────────────────────────────────────────────────────────

test("CP3 south-mode: ownerGrant from Identity is forwarded to createGatewayTask", async () => {
  // WHY: before CP2/CP3, the broker accepted any asserted owner. After CP2 it
  // requires a broker-minted grant. This test proves the gateway carries the
  // grant through to the createGatewayTask call — a gateway with no grant would
  // receive PermissionDenied from the real broker.
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

  const decision = await bridge.gate("call-1", "web_fetch", { url: "https://example.com" });

  assert.equal(decision.allow, true);

  const createCall = south.calls.find((c: CallLog) => c.method === "createGatewayTask");
  assert.ok(createCall, "createGatewayTask must be called in south-mode");

  const req = (createCall!.args[0] as Record<string, unknown>);
  assert.equal(
    req.ownerGrant,
    "test-grant-value",
    "ownerGrant from Identity must be forwarded to createGatewayTask",
  );
});

test("CP3 south-mode: missing ownerGrant defaults to empty string in createGatewayTask", async () => {
  // Ensures the ?? "" fallback works: a missing grant sends "" (broker then
  // rejects, but the gateway doesn't crash on undefined).
  const north = makeNorth();
  const south = makeSouth();
  const clients = makeClients(north, south);

  const identity = {
    tenantId: "11111111-1111-1111-1111-111111111111",
    userId: "alice@example.com",
    agentId: "alice-agent",
    // ownerGrant intentionally absent
  };
  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);

  await bridge.gate("call-2", "web_fetch", { url: "https://example.com" });

  const createCall = south.calls.find((c: CallLog) => c.method === "createGatewayTask");
  assert.ok(createCall, "createGatewayTask must be called");

  const req = (createCall!.args[0] as Record<string, unknown>);
  assert.equal(req.ownerGrant, "", "missing ownerGrant falls back to empty string");
});

test("CP3 north-mode: bearer token present → createGatewayTask NOT called (grant irrelevant on north path)", async () => {
  // The north path uses createTask (OIDC-bound); ownerGrant is ignored there.
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

  const decision = await bridge.gate("call-3", "web_fetch", { url: "https://example.com" });

  assert.equal(decision.allow, true);
  assert.equal(
    south.calls.filter((c: CallLog) => c.method === "createGatewayTask").length,
    0,
    "createGatewayTask must not be called when a bearer token is present",
  );
  assert.ok(
    north.calls.some((c: CallLog) => c.method === "createTask"),
    "north.createTask must be called when token is present",
  );
});
