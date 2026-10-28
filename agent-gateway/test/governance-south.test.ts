// Tests for GovernanceBridge south-mode:
// - When identity.token is ABSENT → gate() uses south.createGatewayTask, not north.createTask
// - When identity.token is ABSENT and NEEDS_HUMAN + approver approves → uses south.approveGatewayTask
// - When identity.token is PRESENT → gate() uses north.createTask (existing path)
import { test } from "node:test";
import assert from "node:assert/strict";
import { GovernanceBridge } from "../src/broker/governance.js";
import { ValidationOutcome } from "../gen/ts/proto/plan";
import { TaskStatus } from "../gen/ts/proto/broker";

// ── Minimal stubs ─────────────────────────────────────────────────────────────

function taskHandle(taskId = "task-1") {
  return { taskId };
}

function approvalResponse(capToken = "tok-1") {
  return { newStatus: TaskStatus.APPROVED, capabilityTokenIds: { 1: capToken } };
}

// The broker's approval row as GetGatewayApproval reports it (AG-07 slice 2).
function gatewayApproval(over: Partial<{ status: TaskStatus; ownerMayApprove: boolean; capabilityTokenIds: Record<number, string> }> = {}) {
  return { status: TaskStatus.AWAITING_APPROVAL, requiresN: 1, approvals: 0, stepUp: false, ownerMayApprove: true, capabilityTokenIds: {}, ...over };
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
    argsDigest: "digest-1",
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

function makeSouth(
  planResult: ReturnType<typeof approvedPlan> | ReturnType<typeof needsHumanPlan>,
  // Successive GetGatewayApproval replies; the last one repeats.
  approvalStates: ReturnType<typeof gatewayApproval>[] = [gatewayApproval()],
) {
  const calls: CallLog[] = [];
  return {
    calls,
    getGatewayApproval: (...args: unknown[]) => {
      calls.push({ method: "getGatewayApproval", args });
      return Promise.resolve(approvalStates.length > 1 ? approvalStates.shift()! : approvalStates[0]);
    },
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

  let shownDigest = "";
  const alwaysApprove = async (info: { argsDigest: string }) => {
    shownDigest = info.argsDigest;
    return true;
  };

  const bridge = new GovernanceBridge(cfg, clients, identity, alwaysApprove, log);
  const decision = await bridge.gate("call-2", "web_fetch", { url: "https://example.com" });

  assert.equal(decision.allow, true);
  // AG-07: the approver is shown the broker's digest and the approve call carries it back.
  assert.equal(shownDigest, "digest-1", "approver should have been called with the plan's args digest");
  const approveCall = south.calls.find((c: CallLog) => c.method === "approveGatewayTask")!;
  assert.equal((approveCall.args[0] as { argsDigest: string }).argsDigest, "digest-1");

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

// ── AG-07 slice 2: the gate resumes from broker state ──────────────────────────

const southIdentity = {
  tenantId: "11111111-1111-1111-1111-111111111111",
  userId: "alice@example.com",
  agentId: "alice-agent",
};

test("2-of-n gate the owner may not vote on: waits, then runs on the others' approval", async () => {
  const south = makeSouth(needsHumanPlan(), [
    gatewayApproval({ ownerMayApprove: false }),
    gatewayApproval({ ownerMayApprove: false }),
    gatewayApproval({ status: TaskStatus.APPROVED, capabilityTokenIds: { 1: "tok-others" } }),
  ]);
  let shown: { ownerMayApprove: boolean } | undefined;
  let released = false;
  // The chat dialog: shows "waiting for approvers", never answers itself.
  const waitingDialog = (info: { ownerMayApprove: boolean }, decided?: AbortSignal) => {
    shown = info;
    return new Promise<boolean>((resolve) => decided?.addEventListener("abort", () => { released = true; resolve(false); }));
  };
  const bridge = new GovernanceBridge(cfg, makeClients(makeNorth(), south), { ...southIdentity }, waitingDialog, log);
  bridge.approvalPollMs = 1;

  const decision = await bridge.gate("call-sod", "web_fetch", { url: "https://example.com" });

  assert.equal(decision.allow, true);
  assert.equal(shown?.ownerMayApprove, false, "dialog is told the owner cannot vote");
  assert.equal(released, true, "dialog released once the broker decided");
  assert.equal(south.calls.filter((c) => c.method === "approveGatewayTask").length, 0, "owner never votes");
});

test("a denial cast elsewhere (Inbox) ends the wait with a refusal", async () => {
  const south = makeSouth(needsHumanPlan(), [gatewayApproval(), gatewayApproval({ status: TaskStatus.DENIED })]);
  const neverAnswers = (_i: unknown, decided?: AbortSignal) =>
    new Promise<boolean>((resolve) => decided?.addEventListener("abort", () => resolve(false)));
  const bridge = new GovernanceBridge(cfg, makeClients(makeNorth(), south), { ...southIdentity }, neverAnswers, log);
  bridge.approvalPollMs = 1;

  const decision = await bridge.gate("call-denied", "web_fetch", { url: "https://example.com" });
  assert.deepEqual(decision, { allow: false, reason: "approval declined" });
});

test("closing the run's bridge ends a pending wait", async () => {
  const south = makeSouth(needsHumanPlan(), [gatewayApproval({ ownerMayApprove: false })]);
  const bridge = new GovernanceBridge(cfg, makeClients(makeNorth(), south), { ...southIdentity }, async () => true, log);
  bridge.approvalPollMs = 1;

  const pending = bridge.gate("call-closed", "web_fetch", { url: "https://example.com" });
  setTimeout(() => bridge.close(), 10);
  assert.equal((await pending).allow, false);
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

// AG-13: the run's own vote on a step-up approval is refused for want of a
// fresh MFA sign-in; the run keeps waiting and resumes on the vote the
// approver casts with one (dialog or Inbox, north ApproveTask).
test("step-up approval: refused run vote keeps waiting, then runs on the MFA vote", async () => {
  const south = makeSouth(needsHumanPlan(), [
    gatewayApproval({ stepUp: true } as never),
    gatewayApproval(),
    gatewayApproval({ status: TaskStatus.APPROVED, capabilityTokenIds: { 1: "tok-mfa" } }),
  ]);
  const { status, Metadata } = await import("@grpc/grpc-js");
  south.approveGatewayTask = (...args: unknown[]) => {
    south.calls.push({ method: "approveGatewayTask", args });
    return Promise.reject(Object.assign(new Error("step_up_required"), {
      code: status.PERMISSION_DENIED, details: "step_up_required", metadata: new Metadata(),
    }));
  };
  let shown: { taskId?: string; stepUp: boolean } | undefined;
  const yes = async (info: { taskId?: string; stepUp: boolean }) => { shown = info; return true; };
  const bridge = new GovernanceBridge(cfg, makeClients(makeNorth(), south), { ...southIdentity }, yes, log);
  bridge.approvalPollMs = 1;

  const decision = await bridge.gate("call-stepup", "web_fetch", { url: "https://example.com" });

  assert.equal(decision.allow, true);
  assert.equal(shown?.taskId, "task-1", "dialog gets the task id to decide with a fresh sign-in");
  assert.equal(shown?.stepUp, true);
  assert.equal(south.calls.filter((c) => c.method === "approveGatewayTask").length, 1);
});
