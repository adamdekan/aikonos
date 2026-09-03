// Tests for the external REST adapter routes.
// Covers: approvalMode:auto → 200 + SSE stream; needs_approval → 409 pre-session;
// prompt over size cap → 413; body over cap → 413.
import { test } from "node:test";
import assert from "node:assert/strict";
import { buildExternalApp } from "../../src/external/server.js";
import type { Config } from "../../src/config.js";
import type { ChildSupervisor } from "../../src/ipc/supervisor.js";

// ── Minimal stubs ─────────────────────────────────────────────────────────────

const TENANT = "11111111-1111-1111-1111-111111111111";
const AUTO_AGENT_ID = "aaaaaaaa-1111-1111-1111-aaaaaaaaaaaa";
const MANUAL_AGENT_ID = "bbbbbbbb-2222-2222-2222-bbbbbbbbbbbb";
const RAW_KEY_AUTO = "tk_auto_key_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx";
const RAW_KEY_MANUAL = "tk_manual_key_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx";

function makeResolve(agentId: string) {
  return async () => ({
    valid: true,
    agentId,
    tenantId: TENANT,
    principal: `svc-${agentId}`,
  });
}

function makeInvalidResolve() {
  return async () => ({ valid: false, agentId: "", tenantId: "", principal: "" });
}

function makeSouth(agentId: string, approvalMode: "auto" | "needs_approval" = "auto", gatewayEnabled = true) {
  const calls: unknown[] = [];
  return {
    calls,
    resolveAgentApiKey: makeResolve(agentId),
    getAgentSpec: async () => ({
      found: true,
      name: "test-agent",
      approvalMode,
      skills: ["doc.read"],
      llmModel: "",
      gatewayEnabled,
    }),
  };
}

function makeClients(south: ReturnType<typeof makeSouth>) {
  return { south } as unknown as import("../../src/broker/clients.js").BrokerClients;
}

const baseCfg: Config = {
  openrouterApiKey: "",
  llmModel: "anthropic/claude-sonnet-4.6",
  brokerNorthAddr: "127.0.0.1:9090",
  brokerSouthAddr: "127.0.0.1:9091",
  brokerServerName: "broker",
  tlsCert: "",
  tlsKey: "",
  tlsCa: "",
  gatewaySpiffeId: "spiffe://aikonos.com/agent-gateway",
  port: 8080,
  defaultTenantId: TENANT,
  oidcIssuer: "",
  oidcJwksUrl: "",
  oidcAudience: "aikonos-broker",
  oidcSubjectClaim: "sub",
  oidcTenantClaim: "tenant_id",
  schedulerEnabled: false,
  schedulerTickMs: 30000,
  schedulerClaimLimit: 10,
  schedulerRunTimeoutMs: 180000,
  agentForUserOverrides: {},
  externalPort: 8090,
  externalCorsOrigins: [],
  externalRateLimit: 60,
  threadTtlMs: 1800000,
  maxChildren: 32,
  childTtlMs: 1800000,
  natsUrl: "nats://nats:4222",
  auditSubject: "aikonos.audit.>",
  egressTimeoutMs: 120000, brokerTimeoutMs: 30000, rateLimitBreakerThreshold: 5, workflowReasonMaxTokens: 2048, maxLlmCallsPerRun: 100, approvalTimeoutMs: 900000, memorySemanticRecall: true, memoryEmbedTimeoutMs: 10000, subagentMaxWidth: 3, subagentBranchTimeoutMs: 180000,
};

const log = {
  info: () => {},
  warn: () => {},
  error: () => {},
  debug: () => {},
} as unknown as import("../../src/log.js").Logger;

// Minimal fake supervisor — the rest.test.ts tests exercise 401/409/413 guards
// that fire before any supervisor call. A real supervisor is not needed here;
// this stub satisfies the type so the app can be constructed.
const fakeSupervisor: ChildSupervisor = {
  keyFor: () => "__single__",
  getOrSpawn: async () => { throw new Error("should not be called in these tests"); },
  run: async () => { throw new Error("should not be called in these tests"); },
  markBusy: () => {},
  markIdle: () => {},
  dispose: () => {},
} as unknown as ChildSupervisor;

test("gateway_enabled=false → 403 before any session", async () => {
  const south = makeSouth(AUTO_AGENT_ID, "auto", false);
  const clients = makeClients(south);
  const app = buildExternalApp(baseCfg, clients, log, fakeSupervisor);
  await app.ready();

  const resp = await app.inject({
    method: "POST",
    url: `/v1/agents/${AUTO_AGENT_ID}/invoke`,
    headers: {
      authorization: `Bearer ${RAW_KEY_AUTO}`,
      "content-type": "application/json",
    },
    body: JSON.stringify({ prompt: "hello" }),
  });

  assert.equal(resp.statusCode, 403);
  const body = JSON.parse(resp.body) as { error: string };
  assert.match(body.error, /external access not enabled/);
  await app.close();
});

test("needs_approval agent → 409, no session built", async () => {
  const south = makeSouth(MANUAL_AGENT_ID, "needs_approval", true);
  const clients = makeClients(south);
  const app = buildExternalApp(baseCfg, clients, log, fakeSupervisor);
  await app.ready();

  const resp = await app.inject({
    method: "POST",
    url: `/v1/agents/${MANUAL_AGENT_ID}/invoke`,
    headers: {
      authorization: `Bearer ${RAW_KEY_MANUAL}`,
      "content-type": "application/json",
    },
    body: JSON.stringify({ prompt: "hello" }),
  });

  assert.equal(resp.statusCode, 409);
  await app.close();
});

test("prompt over 16 KB size cap → 413", async () => {
  const south = makeSouth(AUTO_AGENT_ID, "auto");
  const clients = makeClients(south);
  const app = buildExternalApp(baseCfg, clients, log, fakeSupervisor);
  await app.ready();

  const hugePrompt = "x".repeat(17 * 1024);
  const resp = await app.inject({
    method: "POST",
    url: `/v1/agents/${AUTO_AGENT_ID}/invoke`,
    headers: {
      authorization: `Bearer ${RAW_KEY_AUTO}`,
      "content-type": "application/json",
    },
    body: JSON.stringify({ prompt: hugePrompt }),
  });

  assert.equal(resp.statusCode, 413);
  await app.close();
});

test("missing auth header → 401 (no key)", async () => {
  const south = makeSouth(AUTO_AGENT_ID, "auto");
  const clients = makeClients(south);
  const app = buildExternalApp(baseCfg, clients, log, fakeSupervisor);
  await app.ready();

  const resp = await app.inject({
    method: "POST",
    url: `/v1/agents/${AUTO_AGENT_ID}/invoke`,
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ prompt: "hello" }),
  });

  assert.equal(resp.statusCode, 401);
  await app.close();
});

test("history forwarded to the run message", async () => {
  const south = makeSouth(AUTO_AGENT_ID, "auto");
  const clients = makeClients(south);

  let capturedPrompt: { runId: string; threadId: string; text: string; history?: unknown } | undefined;
  const supervisor = {
    keyFor: () => "__single__",
    getOrSpawn: async () => ({
      setRunContext: () => {},
      clearRunContext: () => {},
      abortRun: () => {},
    }),
    run: async (
      _handle: unknown,
      prompt: { runId: string; threadId: string; text: string; history?: unknown },
      onEvent: (evt: { kind: string; runId: string }) => void,
    ) => {
      capturedPrompt = prompt;
      onEvent({ kind: "done", runId: prompt.runId });
    },
    markBusy: () => {},
    markIdle: () => {},
    dispose: () => {},
  } as unknown as ChildSupervisor;

  const app = buildExternalApp(baseCfg, clients, log, supervisor);
  await app.ready();

  const history = [
    { role: "user", content: "hi" },
    { role: "assistant", content: "hello" },
  ];
  const resp = await app.inject({
    method: "POST",
    url: `/v1/agents/${AUTO_AGENT_ID}/invoke`,
    headers: {
      authorization: `Bearer ${RAW_KEY_AUTO}`,
      "content-type": "application/json",
    },
    body: JSON.stringify({ prompt: "hello", history }),
  });

  assert.equal(resp.statusCode, 200);
  assert.deepEqual(capturedPrompt?.history, history);
  await app.close();
});

test("history not an array → 400", async () => {
  const south = makeSouth(AUTO_AGENT_ID, "auto");
  const clients = makeClients(south);
  const app = buildExternalApp(baseCfg, clients, log, fakeSupervisor);
  await app.ready();

  const resp = await app.inject({
    method: "POST",
    url: `/v1/agents/${AUTO_AGENT_ID}/invoke`,
    headers: {
      authorization: `Bearer ${RAW_KEY_AUTO}`,
      "content-type": "application/json",
    },
    body: JSON.stringify({ prompt: "hello", history: "not-an-array" }),
  });

  assert.equal(resp.statusCode, 400);
  await app.close();
});

test("history entry with bad role → 400", async () => {
  const south = makeSouth(AUTO_AGENT_ID, "auto");
  const clients = makeClients(south);
  const app = buildExternalApp(baseCfg, clients, log, fakeSupervisor);
  await app.ready();

  const resp = await app.inject({
    method: "POST",
    url: `/v1/agents/${AUTO_AGENT_ID}/invoke`,
    headers: {
      authorization: `Bearer ${RAW_KEY_AUTO}`,
      "content-type": "application/json",
    },
    body: JSON.stringify({ prompt: "hello", history: [{ role: "system", content: "x" }] }),
  });

  assert.equal(resp.statusCode, 400);
  await app.close();
});

test("history entry with non-string content → 400", async () => {
  const south = makeSouth(AUTO_AGENT_ID, "auto");
  const clients = makeClients(south);
  const app = buildExternalApp(baseCfg, clients, log, fakeSupervisor);
  await app.ready();

  const resp = await app.inject({
    method: "POST",
    url: `/v1/agents/${AUTO_AGENT_ID}/invoke`,
    headers: {
      authorization: `Bearer ${RAW_KEY_AUTO}`,
      "content-type": "application/json",
    },
    body: JSON.stringify({ prompt: "hello", history: [{ role: "user", content: 42 }] }),
  });

  assert.equal(resp.statusCode, 400);
  await app.close();
});

test("history over 100 turns → 400", async () => {
  const south = makeSouth(AUTO_AGENT_ID, "auto");
  const clients = makeClients(south);
  const app = buildExternalApp(baseCfg, clients, log, fakeSupervisor);
  await app.ready();

  const history = Array.from({ length: 101 }, () => ({ role: "user", content: "x" }));
  const resp = await app.inject({
    method: "POST",
    url: `/v1/agents/${AUTO_AGENT_ID}/invoke`,
    headers: {
      authorization: `Bearer ${RAW_KEY_AUTO}`,
      "content-type": "application/json",
    },
    body: JSON.stringify({ prompt: "hello", history }),
  });

  assert.equal(resp.statusCode, 400);
  await app.close();
});

test("history content bytes over 200 KiB total → 400", async () => {
  const south = makeSouth(AUTO_AGENT_ID, "auto");
  const clients = makeClients(south);
  const app = buildExternalApp(baseCfg, clients, log, fakeSupervisor);
  await app.ready();

  const bigContent = "x".repeat(201 * 1024);
  const resp = await app.inject({
    method: "POST",
    url: `/v1/agents/${AUTO_AGENT_ID}/invoke`,
    headers: {
      authorization: `Bearer ${RAW_KEY_AUTO}`,
      "content-type": "application/json",
    },
    body: JSON.stringify({ prompt: "hello", history: [{ role: "user", content: bigContent }] }),
  });

  assert.equal(resp.statusCode, 400);
  await app.close();
});

test("oversized body (> 256 KiB) → 413 from Fastify's bodyLimit", async () => {
  const south = makeSouth(AUTO_AGENT_ID, "auto");
  const clients = makeClients(south);
  const app = buildExternalApp(baseCfg, clients, log, fakeSupervisor);
  await app.ready();

  const bigContent = "x".repeat(257 * 1024);
  const resp = await app.inject({
    method: "POST",
    url: `/v1/agents/${AUTO_AGENT_ID}/invoke`,
    headers: {
      authorization: `Bearer ${RAW_KEY_AUTO}`,
      "content-type": "application/json",
    },
    body: JSON.stringify({ prompt: "hello", history: [{ role: "user", content: bigContent }] }),
  });

  assert.equal(resp.statusCode, 413);
  await app.close();
});

test("catch-path error frame carries the trimmed message, not a raw error shape", async () => {
  const south = makeSouth(AUTO_AGENT_ID, "auto");
  const clients = makeClients(south);

  // getOrSpawn throws before the generator's own try/catch begins, so the
  // exception propagates to rest.ts's catch path — the surface under test.
  const supervisor = {
    keyFor: () => "__single__",
    getOrSpawn: async () => {
      throw new Error("internal service boom: stack trace garbage at file.ts:123");
    },
    run: async () => {},
    markBusy: () => {},
    markIdle: () => {},
    dispose: () => {},
  } as unknown as ChildSupervisor;

  const app = buildExternalApp(baseCfg, clients, log, supervisor);
  await app.ready();

  const resp = await app.inject({
    method: "POST",
    url: `/v1/agents/${AUTO_AGENT_ID}/invoke`,
    headers: {
      authorization: `Bearer ${RAW_KEY_AUTO}`,
      "content-type": "application/json",
    },
    body: JSON.stringify({ prompt: "hello" }),
  });

  assert.equal(resp.statusCode, 200);
  assert.doesNotMatch(resp.body, /stack trace garbage/);
  assert.doesNotMatch(resp.body, /internal service boom/);
  await app.close();
});

test("revoked key → 401", async () => {
  const south = {
    calls: [] as unknown[],
    resolveAgentApiKey: makeInvalidResolve(),
    getAgentSpec: async () => ({
      found: true,
      name: "test-agent",
      approvalMode: "auto",
      skills: [] as string[],
      llmModel: "",
    }),
  };
  const clients = makeClients(south as unknown as ReturnType<typeof makeSouth>);
  const app = buildExternalApp(baseCfg, clients, log, fakeSupervisor);
  await app.ready();

  const resp = await app.inject({
    method: "POST",
    url: `/v1/agents/${AUTO_AGENT_ID}/invoke`,
    headers: {
      authorization: `Bearer ${RAW_KEY_AUTO}`,
      "content-type": "application/json",
    },
    body: JSON.stringify({ prompt: "hello" }),
  });

  assert.equal(resp.statusCode, 401);
  await app.close();
});
