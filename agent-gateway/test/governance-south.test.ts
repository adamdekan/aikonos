// Tests for GovernanceBridge south-mode:
// - When identity.token is ABSENT → gate() uses south.createGatewayTask, not north.createTask
// - When identity.token is ABSENT and NEEDS_HUMAN + approver approves → uses south.approveGatewayTask
// - When identity.token is PRESENT → gate() uses north.createTask (existing path)
import { test } from "node:test";
import assert from "node:assert/strict";
import { GovernanceBridge } from "../src/broker/governance.js";
import { ValidationOutcome } from "../gen/ts/proto/plan";

// ── Minimal stubs ─────────────────────────────────────────────────────────────

function taskHandle(taskId = "task-1") {
  return { taskId };
}

function approvalResponse(capToken = "tok-1") {
  return { capabilityTokenIds: { 1: capToken } };
}

function approvedPlan(capToken = "tok-1") {
  return {
    outcome: ValidationOutcome.APPROVED,
    capabilityTokenIds: { 1: capToken },
    violations: [],
    steps: [],
  };
}

function needsHumanPlan() {
  return {
    outcome: ValidationOutcome.NEEDS_HUMAN,
    capabilityTokenIds: {},
    violations: [],
    steps: [],
  };
}

interface CallLog {
  method: string;
  args: unknown[];
}

function makeNorth(plan: "approved" | "needs-human" = "approved") {
  const calls: CallLog[] = [];
  return {
    calls,
    createTask: (...args: unknown[]) => {
      calls.push({ method: "createTask", args });
      return Promise.resolve(taskHandle());
    },
    approveTask: (...args: unknown[]) => {
      calls.push({ method: "approveTask", args });
      return Promise.resolve(approvalResponse());
    },
    sendEnvelope: (...args: unknown[]) => {
      calls.push({ method: "sendEnvelope", args });
      return Promise.resolve({ envelopeId: "env-1" });
    },
  };
}

function makeSouth(planResult: ReturnType<typeof approvedPlan> | ReturnType<typeof needsHumanPlan>) {
  const calls: CallLog[] = [];
  return {
    calls,
    createGatewayTask: (...args: unknown[]) => {
      calls.push({ method: "createGatewayTask", args });
      return Promise.resolve(taskHandle());
    },
    approveGatewayTask: (...args: unknown[]) => {
      calls.push({ method: "approveGatewayTask", args });
      return Promise.resolve(approvalResponse());
    },
    submitPlan: (...args: unknown[]) => {
      calls.push({ method: "submitPlan", args });
      return Promise.resolve(planResult);
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

test("GovernanceBridge south-mode: no token → createGatewayTask called, NOT createTask", async () => {
  const north = makeNorth("approved");
  const south = makeSouth(approvedPlan());
  const clients = makeClients(north, south);

  const identity = {
    // token intentionally absent
    tenantId: "11111111-1111-1111-1111-111111111111",
    userId: "alice@example.com",
    agentId: "alice-agent",
  };
  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);

  const decision = await bridge.gate("call-1", "web_fetch", { url: "https://example.com" });

  assert.equal(decision.allow, true);

  // south.createGatewayTask was called
  assert.ok(
    south.calls.some((c: CallLog) => c.method === "createGatewayTask"),
    "expected south.createGatewayTask to be called",
  );
  // north.createTask was NOT called
  assert.equal(
    north.calls.filter((c: CallLog) => c.method === "createTask").length,
    0,
    "north.createTask must not be called in south-mode",
  );
});

test("GovernanceBridge south-mode: NEEDS_HUMAN + approver yes → approveGatewayTask called, NOT approveTask", async () => {
  const north = makeNorth("needs-human");
  const south = makeSouth(needsHumanPlan());
  const clients = makeClients(north, south);

  const identity = {
    tenantId: "11111111-1111-1111-1111-111111111111",
    userId: "alice@example.com",
    agentId: "alice-agent",
  };

  let approverCalled = false;
  const alwaysApprove = async () => {
    approverCalled = true;
    return true;
  };

  const bridge = new GovernanceBridge(cfg, clients, identity, alwaysApprove, log);
  const decision = await bridge.gate("call-2", "web_fetch", { url: "https://example.com" });

  assert.equal(decision.allow, true);
  assert.ok(approverCalled, "approver should have been called for NEEDS_HUMAN");

  // south.approveGatewayTask was called
  assert.ok(
    south.calls.some((c: CallLog) => c.method === "approveGatewayTask"),
    "expected south.approveGatewayTask to be called",
  );
  // north.approveTask was NOT called
  assert.equal(
    north.calls.filter((c: CallLog) => c.method === "approveTask").length,
    0,
    "north.approveTask must not be called in south-mode",
  );
});

test("GovernanceBridge north-mode: token present → north.createTask called, NOT createGatewayTask", async () => {
  const north = makeNorth("approved");
  const south = makeSouth(approvedPlan());
  const clients = makeClients(north, south);

  const identity = {
    token: "bearer-token-value",
    tenantId: "11111111-1111-1111-1111-111111111111",
    userId: "alice@example.com",
    agentId: "alice-agent",
  };
  const bridge = new GovernanceBridge(cfg, clients, identity, async () => true, log);

  const decision = await bridge.gate("call-3", "web_fetch", { url: "https://example.com" });

  assert.equal(decision.allow, true);

  // north.createTask was called
  assert.ok(
    north.calls.some((c: CallLog) => c.method === "createTask"),
    "expected north.createTask to be called in north-mode",
  );
  // south.createGatewayTask was NOT called
  assert.equal(
    south.calls.filter((c: CallLog) => c.method === "createGatewayTask").length,
    0,
    "south.createGatewayTask must not be called when token is present",
  );
});

test("GovernanceBridge south-mode: NEEDS_HUMAN + approver declines → allow=false, approveGatewayTask not called", async () => {
  const north = makeNorth("needs-human");
  const south = makeSouth(needsHumanPlan());
  const clients = makeClients(north, south);

  const identity = {
    tenantId: "11111111-1111-1111-1111-111111111111",
    userId: "alice@example.com",
    agentId: "alice-agent",
  };
  const bridge = new GovernanceBridge(cfg, clients, identity, async () => false, log);

  const decision = await bridge.gate("call-4", "web_fetch", { url: "https://example.com" });

  assert.equal(decision.allow, false);
  assert.equal(
    south.calls.filter((c: CallLog) => c.method === "approveGatewayTask").length,
    0,
    "approveGatewayTask must not be called when approver declines",
  );
});

// ── Grep gate (no tokenForUser / x-aikonos-user / demoPassword in src) ─────────

test("grep gate: no tokenForUser references in agent-gateway/src", async () => {
  const { execSync } = await import("node:child_process");
  const { fileURLToPath } = await import("node:url");
  const path = await import("node:path");
  // Resolved from this file so the gate runs wherever the repo is checked out.
  // A hardcoded absolute path made grep find nothing and the assertion vacuous.
  const src = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../src");
  const result = execSync(
    `grep -rn 'tokenForUser\\|x-aikonos-user\\|demoPassword' ${src} 2>/dev/null || true`,
    { encoding: "utf8" },
  );
  assert.equal(
    result.trim(),
    "",
    `Found forbidden references in src:\n${result}`,
  );
});
