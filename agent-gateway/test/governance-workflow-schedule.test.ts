// CP3 tests: GovernanceBridge.scheduleWorkflow.
//
// WHY these tests exist:
//   1. North-only: CreateScheduledRun has no south twin (spec non-goal —
//      scheduled/unattended runs never create schedules). A token-less
//      identity must fail cleanly, never attempt (and crash on) a
//      nonexistent south call.
//   2. Wiring: the RPC request sent to north.createScheduledRun must carry
//      the recurrence (kind/cronExpr/runAt), lineageId, and inputs exactly —
//      and always an empty approved_tools (no such param exists for a
//      workflow schedule).
//   4. F52: a schedule is a standing consent, so it exists only after a gate()
//      decision (a human card, OPA NEEDS_HUMAN) for these exact arguments and
//      the workflow version the card showed.
//   3. Missing-inputs warning: when the supplied inputs don't cover the
//      workflow's required (no-default) inputs, the create must still
//      succeed (ok:true) and the missing names come back for the tool to
//      surface as a warning — never a hard failure.
import { test } from "node:test";
import assert from "node:assert/strict";
import { GovernanceBridge } from "../src/broker/governance.js";
import { ScheduleKind, TaskStatus } from "../gen/ts/proto/broker";
import { ValidationOutcome } from "../gen/ts/proto/plan";
import type { ApprovalInfo } from "../src/broker/governance.js";

interface CreateScheduledRunCall {
  req: Record<string, unknown>;
}

function makeNorth(definitionJson = "{}") {
  const createScheduledRunCalls: CreateScheduledRunCall[] = [];
  const getWorkflowCalls: Record<string, unknown>[] = [];
  const north = {
    createScheduledRunCalls,
    getWorkflowCalls,
    // The lineage's current version; a test bumps it to model a re-save.
    version: 3,
    getWorkflow: (req: Record<string, unknown>) => {
      getWorkflowCalls.push(req);
      return Promise.resolve({ definitionJson, version: north.version });
    },
    createScheduledRun: (req: Record<string, unknown>) => {
      createScheduledRunCalls.push({ req });
      return Promise.resolve({ run: { id: "sched-1" } });
    },
    createTask: () => Promise.resolve({ taskId: "t-1" }),
    approveTask: () => Promise.resolve({ newStatus: TaskStatus.APPROVED, capabilityTokenIds: {} }),
  };
  return north;
}

// OPA's answer for a WRITE_EXTERNAL plan: NEEDS_HUMAN (F52).
function makeSouthStub() {
  return {
    submitPlan: () =>
      Promise.resolve({ outcome: ValidationOutcome.NEEDS_HUMAN, capabilityTokenIds: {}, violations: [], steps: [], argsDigest: "d" }),
    getGatewayApproval: () =>
      Promise.resolve({ status: TaskStatus.AWAITING_APPROVAL, requiresN: 1, approvals: 0, stepUp: false, ownerMayApprove: true }),
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

// approver stands in for the chat card: it records what the human was shown.
function makeBridge(
  north: unknown,
  identityOverrides: Record<string, unknown> = {},
  approver: (info: ApprovalInfo) => Promise<boolean> = async () => true,
) {
  const south = makeSouthStub();
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;
  const identity = {
    token: "bearer-tok",
    tenantId: "11111111-1111-1111-1111-111111111111",
    userId: "alice@example.com",
    agentId: "alice-agent",
    ...identityOverrides,
  };
  return new GovernanceBridge(cfg, clients, identity, approver, log);
}

type Recurrence = { kind: "cron" | "once"; cronExpr?: string; runAt?: string };

// The Pi loop's order: the tool_call gate, then the tool's execute.
async function gateThenSchedule(
  bridge: GovernanceBridge,
  lineageId: string,
  inputs: Record<string, string>,
  r: Recurrence,
) {
  const decision = await bridge.gate("tc-1", "workflow_schedule", { lineageId, inputs, ...r });
  assert.equal(decision.allow, true, `the gate must allow after a human yes; got ${decision.reason}`);
  return bridge.scheduleWorkflow(lineageId, inputs, r);
}

test("scheduleWorkflow: no-token identity fails cleanly, never calls the broker", async () => {
  const north = makeNorth();
  const bridge = makeBridge(north, { token: undefined });

  const result = await bridge.scheduleWorkflow("lin-1", {}, { kind: "cron", cronExpr: "0 8 * * 1" });

  assert.equal(result.ok, false, "no-token schedule create must fail");
  assert.match(
    result.error ?? "",
    /interactive|no token|OIDC/i,
    `error must clearly say scheduled runs can't create schedules; got: ${result.error}`,
  );
  assert.equal(north.createScheduledRunCalls.length, 0, "broker must not be called without a token");
});

test("scheduleWorkflow: sends kind/cronExpr/runAt/lineageId/inputs and always empty approvedTools", async () => {
  const north = makeNorth();
  const bridge = makeBridge(north);

  const result = await gateThenSchedule(bridge, "lin-1", { since: "-7d" }, { kind: "cron", cronExpr: "0 8 * * 1" });

  assert.equal(result.ok, true, `schedule create must succeed; got error: ${result.error}`);
  assert.equal(result.scheduleId, "sched-1");
  assert.equal(north.createScheduledRunCalls.length, 1, "createScheduledRun must be called once");
  const req = north.createScheduledRunCalls[0].req;
  assert.equal(req.workflowLineageId, "lin-1");
  assert.deepEqual(req.workflowInputs, { since: "-7d" });
  assert.equal(req.kind, ScheduleKind.SCHEDULE_KIND_CRON);
  assert.equal(req.cronExpr, "0 8 * * 1");
  assert.deepEqual(req.approvedTools, [], "workflow schedules never carry an approved-tools list");
  assert.equal(req.prompt, "", "workflow-mode create must leave prompt empty");
});

test("scheduleWorkflow: kind 'once' maps to SCHEDULE_KIND_ONCE and forwards runAt as a Date", async () => {
  const north = makeNorth();
  const bridge = makeBridge(north);

  const runAt = new Date(Date.now() + 60_000).toISOString();
  await gateThenSchedule(bridge, "lin-1", {}, { kind: "once", runAt });

  const req = north.createScheduledRunCalls[0].req;
  assert.equal(req.kind, ScheduleKind.SCHEDULE_KIND_ONCE);
  assert.ok(req.runAt instanceof Date, "runAt must be forwarded as a Date");
  assert.equal((req.runAt as Date).toISOString(), runAt);
});

test("scheduleWorkflow: missing required inputs are returned but the create still succeeds", async () => {
  const definitionJson = JSON.stringify({
    apiVersion: "aikonos.com/v1",
    kind: "Workflow",
    metadata: { name: "weekly-report", visibility: { kind: "private" } },
    inputs: [{ name: "region" }, { name: "since", default: "-7d" }],
    steps: [{ skill: "web.fetch", args: { url: "${inputs.region}" } }],
  });
  const north = makeNorth(definitionJson);
  const bridge = makeBridge(north);

  const result = await gateThenSchedule(bridge, "lin-1", {}, { kind: "cron", cronExpr: "0 8 * * 1" });

  assert.equal(result.ok, true, "missing required inputs must never hard-fail the create");
  assert.deepEqual(
    result.missingInputs,
    ["region"],
    "only the input without a default, and not supplied, must be flagged",
  );
  assert.equal(north.createScheduledRunCalls.length, 1, "the schedule must still be created");
});

test("scheduleWorkflow: no missing inputs when every required input is supplied", async () => {
  const definitionJson = JSON.stringify({
    apiVersion: "aikonos.com/v1",
    kind: "Workflow",
    metadata: { name: "weekly-report", visibility: { kind: "private" } },
    inputs: [{ name: "region" }],
    steps: [{ skill: "web.fetch", args: { url: "${inputs.region}" } }],
  });
  const north = makeNorth(definitionJson);
  const bridge = makeBridge(north);

  const result = await gateThenSchedule(bridge, "lin-1", { region: "eu" }, { kind: "cron", cronExpr: "0 8 * * 1" });

  assert.equal(result.ok, true);
  assert.deepEqual(result.missingInputs, []);
});

// ── F52: a schedule is a human decision ───────────────────────────────────────

const WRITE_WORKFLOW = JSON.stringify({
  apiVersion: "aikonos.com/v1",
  kind: "Workflow",
  metadata: { name: "exfil", visibility: { kind: "private" } },
  steps: [
    { skill: "web.fetch", args: { url: "https://example.test/a" } },
    { kind: "reason", prompt: "summarise" },
    { skill: "gdrive.write", args: { path: "out.txt", content: "${steps.0.output}" } },
  ],
});

test("F52: the approval card shows the workflow's version and tool steps (skill + static args)", async () => {
  const north = makeNorth(WRITE_WORKFLOW);
  const shown: ApprovalInfo[] = [];
  const bridge = makeBridge(north, {}, async (info) => { shown.push(info); return true; });

  const result = await gateThenSchedule(bridge, "lin-1", {}, { kind: "cron", cronExpr: "0 8 * * 1" });

  assert.equal(result.ok, true);
  assert.equal(shown.length, 1, "creating a schedule must put a card in front of the user");
  assert.equal(shown[0].toolId, "scheduler");
  assert.equal(shown[0].args.workflowVersion, 3);
  assert.deepEqual(shown[0].args.toolSteps, [
    { skill: "web.fetch", args: { url: "https://example.test/a" } },
    { skill: "gdrive.write", args: { path: "out.txt", content: "${steps.0.output}" } },
  ]);
});

test("F52: scheduleWorkflow with no gate decision creates nothing (a child that skips the gate gets no schedule)", async () => {
  const north = makeNorth(WRITE_WORKFLOW);
  const bridge = makeBridge(north);

  const result = await bridge.scheduleWorkflow("lin-1", {}, { kind: "cron", cronExpr: "0 8 * * 1" });

  assert.equal(result.ok, false);
  assert.equal(north.createScheduledRunCalls.length, 0);
});

test("F52: a declined card creates nothing", async () => {
  const north = makeNorth(WRITE_WORKFLOW);
  const bridge = makeBridge(north, {}, async () => false);

  const decision = await bridge.gate("tc-1", "workflow_schedule", { lineageId: "lin-1", kind: "cron", cronExpr: "0 8 * * 1" });
  const result = await bridge.scheduleWorkflow("lin-1", {}, { kind: "cron", cronExpr: "0 8 * * 1" });

  assert.equal(decision.allow, false);
  assert.equal(result.ok, false);
  assert.equal(north.createScheduledRunCalls.length, 0);
});

test("F52: the approval binds the arguments — a different recurrence than the card showed is refused", async () => {
  const north = makeNorth(WRITE_WORKFLOW);
  const bridge = makeBridge(north);

  await bridge.gate("tc-1", "workflow_schedule", { lineageId: "lin-1", kind: "once", runAt: "2099-01-01T00:00:00Z" });
  const result = await bridge.scheduleWorkflow("lin-1", {}, { kind: "cron", cronExpr: "* * * * *" });

  assert.equal(result.ok, false);
  assert.equal(north.createScheduledRunCalls.length, 0);
});

test("F52: the approval binds the version — a workflow re-saved after the card was shown is refused", async () => {
  const north = makeNorth(WRITE_WORKFLOW);
  const bridge = makeBridge(north);

  await bridge.gate("tc-1", "workflow_schedule", { lineageId: "lin-1", kind: "cron", cronExpr: "0 8 * * 1" });
  north.version = 4; // the model saves a new version between the card and the create
  const result = await bridge.scheduleWorkflow("lin-1", {}, { kind: "cron", cronExpr: "0 8 * * 1" });

  assert.equal(result.ok, false);
  assert.match(result.error ?? "", /changed since it was approved/);
  assert.equal(north.createScheduledRunCalls.length, 0);
});

test("F52: an approval is single-use — a second create with the same arguments needs a new card", async () => {
  const north = makeNorth(WRITE_WORKFLOW);
  const bridge = makeBridge(north);

  await gateThenSchedule(bridge, "lin-1", {}, { kind: "cron", cronExpr: "0 8 * * 1" });
  const again = await bridge.scheduleWorkflow("lin-1", {}, { kind: "cron", cronExpr: "0 8 * * 1" });

  assert.equal(again.ok, false);
  assert.equal(north.createScheduledRunCalls.length, 1);
});
