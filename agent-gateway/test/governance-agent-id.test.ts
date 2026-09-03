// CP2 TS test: GovernanceBridge south-mode passes agentId to createGatewayTask.
//
// When identity.token is absent (south / unattended mode), gate() calls
// south.createGatewayTask.  CP2 requires the request to carry the agentId
// from the identity so the broker can bind the agent_id column on the task
// and enforce the skill boundary at SubmitPlan.
import { test } from "node:test";
import assert from "node:assert/strict";
import { GovernanceBridge } from "../src/broker/governance.js";
import { ValidationOutcome } from "../gen/ts/proto/plan";
import type { BrokerClients } from "../src/broker/clients.js";

// ── Minimal stubs ─────────────────────────────────────────────────────────────

function approvedPlan(capToken = "tok-1") {
  return {
    outcome: ValidationOutcome.APPROVED,
    capabilityTokenIds: { 1: capToken },
    violations: [],
    steps: [],
  };
}

function makeStubClients() {
  const gatewayTaskCalls: unknown[] = [];
  const south = {
    createGatewayTask: (req: unknown) => {
      gatewayTaskCalls.push(req);
      return Promise.resolve({ taskId: "task-1" });
    },
    submitPlan: () => Promise.resolve(approvedPlan()),
    invokeTool: () =>
      Promise.resolve({ success: true, result: "ok", error: "", costUnitsConsumed: 0 }),
    emitStatus: () => Promise.resolve(),
    approveGatewayTask: () =>
      Promise.resolve({ capabilityTokenIds: { 1: "tok-1" } }),
  };
  const north = {
    createTask: () => Promise.resolve({ taskId: "task-1" }),
    approveTask: () => Promise.resolve({ capabilityTokenIds: { 1: "tok-1" } }),
    sendEnvelope: () => Promise.resolve({ envelopeId: "env-1" }),
  };
  return { clients: { north, south } as unknown as BrokerClients, gatewayTaskCalls };
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

// ── Test ──────────────────────────────────────────────────────────────────────

test("GovernanceBridge south-mode: createGatewayTask receives agentId from identity", async () => {
  const { clients, gatewayTaskCalls } = makeStubClients();

  const identity = {
    // token absent → south-mode
    tenantId: "11111111-1111-1111-1111-111111111111",
    userId: "svc-aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
    agentId: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
  };

  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);
  const decision = await bridge.gate("call-1", "web_fetch", { url: "https://example.com" });

  assert.equal(decision.allow, true, "gate should allow an approved plan");
  assert.equal(gatewayTaskCalls.length, 1, "createGatewayTask must be called once");

  const req = gatewayTaskCalls[0] as Record<string, unknown>;
  assert.equal(
    req.agentId,
    "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
    "agentId must be forwarded to createGatewayTask so the broker can bind it on the task",
  );
});
