// Batch-2 route tests:
//   Task 6 — GET /workflows and GET /workflows/:id/versions forward optional
//     limit/cursor query params and echo next_cursor (+ shared_unavailable on
//     the list route); a bad limit is a 400. Defaults (absent params) are
//     unchanged: limit 0, cursor "".
//   Task 7 — POST /workflows/:id/run?stream=1 responds as SSE: one `step` event
//     per settled step, then a terminal `result` event carrying the exact JSON
//     the blocking path returns. Without ?stream=1 the blocking JSON is unchanged.
//
// Registers the real registerWorkflowRoutes against fake broker clients, so a
// mutation to the production route fails this test.
import { test } from "node:test";
import assert from "node:assert/strict";
import Fastify from "fastify";
import pino from "pino";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import { registerWorkflowRoutes } from "../src/routes/workflows.js";
import { BrokerClients } from "../src/broker/clients.js";
import { NorthClient } from "../src/broker/north.js";
import { SouthClient } from "../src/broker/south.js";
import type { Config } from "../src/config.js";
import type { JwksResolver } from "../src/auth/verify.js";
import type { RateLimitChecker } from "../src/llm/egress-proxy.js";

const VERIFY_OPTS = { issuer: "http://localhost:18080/realms/aikonos", audience: "aikonos-broker" };

async function makeKey() {
  const { publicKey, privateKey } = await generateKeyPair("RS256");
  const jwk: JWK = { ...(await exportJWK(publicKey)), kid: "k1", alg: "RS256", use: "sig" };
  return { privateKey, jwk };
}
async function localResolver(jwk: JWK): Promise<JwksResolver> {
  const key = await importJWK(jwk, "RS256");
  return () => Promise.resolve(key);
}
async function mintToken(privateKey: Awaited<ReturnType<typeof generateKeyPair>>["privateKey"]) {
  return new SignJWT({ sub: "alice@example.com", email: "alice@example.com", tenant_id: "aikonos-dev" })
    .setProtectedHeader({ alg: "RS256", kid: "k1" })
    .setIssuer(VERIFY_OPTS.issuer)
    .setAudience(VERIFY_OPTS.audience)
    .setIssuedAt()
    .setExpirationTime("1h")
    .sign(privateKey);
}

function fakeConfig(): Config {
  return {
    openrouterApiKey: "", llmModel: "", brokerNorthAddr: "", brokerSouthAddr: "",
    brokerServerName: "", tlsCert: "", tlsKey: "", tlsCa: "",
    gatewaySpiffeId: "spiffe://aikonos.com/agent-gateway",
    port: 8080, defaultTenantId: "aikonos-dev", oidcIssuer: "", oidcJwksUrl: "", oidcAudience: "",
    oidcSubjectClaim: "sub", oidcTenantClaim: "tenant_id", schedulerEnabled: false,
    schedulerTickMs: 30000, schedulerClaimLimit: 10, schedulerRunTimeoutMs: 180000,
    agentForUserOverrides: {}, externalPort: 8090, externalCorsOrigins: [],
    externalRateLimit: 60, threadTtlMs: 1800000, maxChildren: 32, childTtlMs: 1800000,
    natsUrl: "nats://nats:4222", auditSubject: "aikonos.audit.>", egressTimeoutMs: 120000,
    brokerTimeoutMs: 30000, rateLimitBreakerThreshold: 5, workflowReasonMaxTokens: 2048, maxLlmCallsPerRun: 100, approvalTimeoutMs: 900000, memorySemanticRecall: true, memoryEmbedTimeoutMs: 10000, subagentMaxWidth: 3, subagentBranchTimeoutMs: 180000,
  };
}

// buildApp wires the real routes over injectable north/south overrides.
async function buildApp(
  northOverrides: Record<string, unknown>,
  southOverrides: Record<string, unknown> = {},
  rateLimitChecker?: RateLimitChecker,
) {
  const { privateKey, jwk } = await makeKey();
  const jwksResolver = await localResolver(jwk);
  const token = await mintToken(privateKey);

  const app = Fastify({ logger: false });
  const clients: BrokerClients = Object.create(BrokerClients.prototype);
  const north: NorthClient = Object.create(NorthClient.prototype);
  const south: SouthClient = Object.create(SouthClient.prototype);
  Object.assign(north, northOverrides);
  Object.assign(south, southOverrides);
  clients.north = north;
  clients.south = south;

  registerWorkflowRoutes(app, {
    clients, jwksResolver, verifyOpts: VERIFY_OPTS, cfg: fakeConfig(), log: pino({ level: "silent" }), rateLimitChecker,
  });
  return { app, token };
}

// ── Task 6: GET /workflows pagination + passthrough ────────────────────────────

test("GET /workflows: defaults forward limit 0 / cursor '' and echo nextCursor + sharedUnavailable", async () => {
  const seen: unknown[] = [];
  const { app, token } = await buildApp({
    listWorkflows: (req: unknown) => {
      seen.push(req);
      return Promise.resolve({ items: [{ lineageId: "l1" }], nextCursor: "CUR", sharedUnavailable: true });
    },
  });
  await app.ready();

  const res = await app.inject({ method: "GET", url: "/workflows", headers: { authorization: `Bearer ${token}` } });
  assert.equal(res.statusCode, 200);
  const body = JSON.parse(res.body) as { workflows: unknown[]; nextCursor: string; sharedUnavailable: boolean };
  assert.equal(body.workflows.length, 1);
  assert.equal(body.nextCursor, "CUR");
  assert.equal(body.sharedUnavailable, true);
  assert.deepEqual(seen[0], { tenantId: "aikonos-dev", ownerGrant: "", userId: "alice@example.com", limit: 0, cursor: "" });
  await app.close();
});

test("GET /workflows: limit + cursor query params are forwarded verbatim", async () => {
  const seen: Array<{ limit: number; cursor: string }> = [];
  const { app, token } = await buildApp({
    listWorkflows: (req: { limit: number; cursor: string }) => {
      seen.push(req);
      return Promise.resolve({ items: [], nextCursor: "", sharedUnavailable: false });
    },
  });
  await app.ready();

  const res = await app.inject({
    method: "GET",
    url: "/workflows?limit=5&cursor=abc",
    headers: { authorization: `Bearer ${token}` },
  });
  assert.equal(res.statusCode, 200);
  assert.equal(seen[0]?.limit, 5);
  assert.equal(seen[0]?.cursor, "abc");
  await app.close();
});

test("GET /workflows: a non-integer/negative limit is a 400 (never hits the broker)", async () => {
  let called = false;
  const { app, token } = await buildApp({
    listWorkflows: () => {
      called = true;
      return Promise.resolve({ items: [], nextCursor: "", sharedUnavailable: false });
    },
  });
  await app.ready();

  for (const bad of ["abc", "-1", "1.5"]) {
    const res = await app.inject({ method: "GET", url: `/workflows?limit=${bad}`, headers: { authorization: `Bearer ${token}` } });
    assert.equal(res.statusCode, 400, `limit=${bad} must be rejected`);
  }
  assert.equal(called, false, "a bad limit must never reach the broker");
  await app.close();
});

test("GET /workflows/:id/versions: forwards limit/cursor and returns nextCursor", async () => {
  const seen: Array<{ limit: number; cursor: string }> = [];
  const { app, token } = await buildApp({
    listWorkflowVersions: (req: { limit: number; cursor: string }) => {
      seen.push(req);
      return Promise.resolve({ items: [{ version: 1 }], nextCursor: "NX" });
    },
  });
  await app.ready();

  const res = await app.inject({
    method: "GET",
    url: "/workflows/lin-1/versions?limit=3&cursor=zz",
    headers: { authorization: `Bearer ${token}` },
  });
  assert.equal(res.statusCode, 200);
  const body = JSON.parse(res.body) as { versions: unknown[]; nextCursor: string };
  assert.equal(body.nextCursor, "NX");
  assert.equal(seen[0]?.limit, 3);
  assert.equal(seen[0]?.cursor, "zz");
  await app.close();
});

// ── Task 7: POST /workflows/:id/run streaming ──────────────────────────────────

// A north/south pair that runs a real 2-step (doc.read) workflow to completion
// through the GovernanceBridge gate→execute path.
function runClients() {
  const def = {
    apiVersion: "aikonos.com/v1",
    kind: "Workflow",
    metadata: { name: "wf", visibility: { kind: "private" } },
    inputs: [],
    steps: [
      { skill: "doc.read", args: { path: "a.txt" } },
      { skill: "doc.read", args: { path: "b.txt" } },
    ],
  };
  const north = {
    beginWorkflowRun: () => Promise.resolve({ ownerGrant: "", boundAgentId: "" }),
    getWorkflow: () => Promise.resolve({ definitionJson: JSON.stringify(def), version: 1 }),
    createTask: () => Promise.resolve({ taskId: "t1" }),
  };
  const south = {
    submitPlan: () => Promise.resolve({ outcome: 1, capabilityTokenIds: { 1: "tok" }, violations: [], steps: [] }),
    invokeTool: () => Promise.resolve({ success: true, result: { content: "hi" }, error: "", costUnitsConsumed: 0 }),
    emitStatus: () => Promise.resolve({}),
  };
  return { north, south };
}

// Parse SSE frames from the raw response body into { event, data } records.
function parseSse(body: string): Array<{ event: string; data: unknown }> {
  const frames: Array<{ event: string; data: unknown }> = [];
  for (const block of body.split("\n\n")) {
    const lines = block.split("\n");
    const eventLine = lines.find((l) => l.startsWith("event: "));
    const dataLine = lines.find((l) => l.startsWith("data: "));
    if (eventLine && dataLine) {
      frames.push({ event: eventLine.slice("event: ".length), data: JSON.parse(dataLine.slice("data: ".length)) });
    }
  }
  return frames;
}

test("POST run?stream=1: emits a step event per step, then a terminal result event", async () => {
  const { north, south } = runClients();
  const { app, token } = await buildApp(north, south);
  await app.ready();

  const res = await app.inject({
    method: "POST",
    url: "/workflows/lin-1/run?stream=1",
    headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
    payload: { inputs: {} },
  });

  assert.equal(res.statusCode, 200);
  assert.match(res.headers["content-type"] as string, /text\/event-stream/);
  const frames = parseSse(res.body);
  const steps = frames.filter((f) => f.event === "step");
  const results = frames.filter((f) => f.event === "result");

  assert.equal(steps.length, 2, "one step event per step");
  assert.deepEqual(
    steps.map((s) => (s.data as { index: number }).index),
    [0, 1],
    "step events in order",
  );
  assert.equal((steps[0].data as { ok: boolean }).ok, true);
  assert.equal(results.length, 1, "exactly one terminal result event");
  // Every step event must precede the result event.
  const lastStepIdx = frames.map((f) => f.event).lastIndexOf("step");
  const resultIdx = frames.map((f) => f.event).indexOf("result");
  assert.ok(lastStepIdx < resultIdx, "all step events precede the result event");
  // The result frame carries the same wrapper the blocking path returns.
  const rd = results[0].data as { ok: boolean; result?: { halted: boolean; steps: unknown[] } };
  assert.equal(rd.ok, true);
  assert.equal(rd.result?.halted, false);
  assert.equal(rd.result?.steps.length, 2);
  await app.close();
});

test("POST run (no stream): unchanged blocking JSON response", async () => {
  const { north, south } = runClients();
  const { app, token } = await buildApp(north, south);
  await app.ready();

  const res = await app.inject({
    method: "POST",
    url: "/workflows/lin-1/run",
    headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
    payload: { inputs: {} },
  });

  assert.equal(res.statusCode, 200);
  assert.ok(!(res.headers["content-type"] as string).includes("event-stream"));
  const body = JSON.parse(res.body) as { ok: boolean; result: { halted: boolean; steps: unknown[] } };
  assert.equal(body.ok, true);
  assert.equal(body.result.halted, false);
  assert.equal(body.result.steps.length, 2);
  await app.close();
});

// ── Spend-caps CP3 finding 2: the route-built GovernanceBridge (both the
// blocking and the SSE run paths) must receive ctx.rateLimitChecker, or a
// webui-run workflow's reason steps are never pre-gated ────────────────────

function reasonWorkflowClients() {
  const def = {
    apiVersion: "aikonos.com/v1",
    kind: "Workflow",
    metadata: { name: "wf", visibility: { kind: "private" } },
    inputs: [],
    steps: [{ kind: "reason", skill: "", args: {}, instruction: "summarize" }],
  };
  const chatProvider = {
    id: "openai",
    name: "openai",
    endpoint: "https://api.openai.com/v1",
    api: "openai-completions",
    apiKey: "sk-test",
    enabled: true,
    isDefault: true,
    models: [{ id: "gpt-4o" }],
  };
  const north = {
    beginWorkflowRun: () => Promise.resolve({ ownerGrant: "", boundAgentId: "" }),
    getWorkflow: () => Promise.resolve({ definitionJson: JSON.stringify(def), version: 1 }),
  };
  const south = {
    getLlmProviders: () => Promise.resolve({ providers: [chatProvider] }),
    emitLlmUsage: () => Promise.resolve(),
  };
  return { north, south };
}

test("POST run (no stream): ctx.rateLimitChecker reaches the bridge and denies a reason step", async () => {
  const { north, south } = reasonWorkflowClients();
  const rateLimitCalls: unknown[] = [];
  const denyChecker = async (tenantId: string, agentId: string, provider: string) => {
    rateLimitCalls.push([tenantId, agentId, provider]);
    throw new Error("rate limit exceeded: spend_agent");
  };
  const { app, token } = await buildApp(north, south, denyChecker);
  await app.ready();

  const res = await app.inject({
    method: "POST",
    url: "/workflows/lin-1/run",
    headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
    payload: { inputs: {} },
  });

  assert.equal(res.statusCode, 200);
  const body = JSON.parse(res.body) as { ok: boolean; result: { halted: boolean; haltReason?: string } };
  assert.equal(body.ok, true);
  assert.equal(body.result.halted, true, "the reason step's pre-gate denial must halt the run");
  assert.match(body.result.haltReason ?? "", /rate limit exceeded: spend_agent/);
  assert.equal(rateLimitCalls.length, 1, "the blocking-run bridge must invoke the injected rate-limit checker");
  await app.close();
});

test("POST run?stream=1: ctx.rateLimitChecker reaches the SSE bridge and denies a reason step", async () => {
  const { north, south } = reasonWorkflowClients();
  const rateLimitCalls: unknown[] = [];
  const denyChecker = async (tenantId: string, agentId: string, provider: string) => {
    rateLimitCalls.push([tenantId, agentId, provider]);
    throw new Error("rate limit exceeded: spend_agent");
  };
  const { app, token } = await buildApp(north, south, denyChecker);
  await app.ready();

  const res = await app.inject({
    method: "POST",
    url: "/workflows/lin-1/run?stream=1",
    headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
    payload: { inputs: {} },
  });

  assert.equal(res.statusCode, 200);
  const frames = parseSse(res.body);
  const results = frames.filter((f) => f.event === "result");
  assert.equal(results.length, 1);
  const rd = results[0].data as { ok: boolean; result?: { halted: boolean; haltReason?: string } };
  assert.equal(rd.result?.halted, true, "the reason step's pre-gate denial must halt the run");
  assert.match(rd.result?.haltReason ?? "", /rate limit exceeded: spend_agent/);
  assert.equal(rateLimitCalls.length, 1, "the SSE-run bridge must invoke the injected rate-limit checker");
  await app.close();
});
