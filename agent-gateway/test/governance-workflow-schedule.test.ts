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
//   3. Missing-inputs warning: when the supplied inputs don't cover the
//      workflow's required (no-default) inputs, the create must still
//      succeed (ok:true) and the missing names come back for the tool to
//      surface as a warning — never a hard failure.
import { test } from "node:test";
import assert from "node:assert/strict";
import { GovernanceBridge } from "../src/broker/governance.js";
import { ScheduleKind } from "../gen/ts/proto/broker";

interface CreateScheduledRunCall {
  req: Record<string, unknown>;
}

function makeNorth(definitionJson = "{}") {
  const createScheduledRunCalls: CreateScheduledRunCall[] = [];
  const getWorkflowCalls: Record<string, unknown>[] = [];
  return {
    createScheduledRunCalls,
    getWorkflowCalls,
    getWorkflow: (req: Record<string, unknown>) => {
      getWorkflowCalls.push(req);
      return Promise.resolve({ definitionJson });
    },
    createScheduledRun: (req: Record<string, unknown>) => {
      createScheduledRunCalls.push({ req });
      return Promise.resolve({ run: { id: "sched-1" } });
    },
  };
}

function makeSouthStub() {
  return {};
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

function makeBridge(north: unknown, identityOverrides: Record<string, unknown> = {}) {
  const south = makeSouthStub();
  const clients = { north, south } as unknown as import("../src/broker/clients.js").BrokerClients;
  const identity = {
    token: "bearer-tok",
    tenantId: "11111111-1111-1111-1111-111111111111",
    userId: "alice@example.com",
    agentId: "alice-agent",
    ...identityOverrides,
  };
  return new GovernanceBridge(cfg, clients, identity, async () => true, log);
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

  const result = await bridge.scheduleWorkflow("lin-1", { since: "-7d" }, { kind: "cron", cronExpr: "0 8 * * 1" });

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
  await bridge.scheduleWorkflow("lin-1", {}, { kind: "once", runAt });

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

  const result = await bridge.scheduleWorkflow("lin-1", {}, { kind: "cron", cronExpr: "0 8 * * 1" });

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

  const result = await bridge.scheduleWorkflow("lin-1", { region: "eu" }, { kind: "cron", cronExpr: "0 8 * * 1" });

  assert.equal(result.ok, true);
  assert.deepEqual(result.missingInputs, []);
});
