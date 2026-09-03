// Route-level pin for routes/agui.ts's `close` handler. Registers the real
// registerAgUiRoutes against a real ChildSupervisor (fake spawn — no real
// process.fork) and a real ApprovalRegistry, drives a real HTTP request against
// a listening Fastify server, waits until the run has started, then aborts the
// CLIENT request to simulate a genuine mid-run disconnect and asserts teardown.
//
// WHY this exists over the pre-existing hitl-drain/unit tests: those test
// ApprovalRegistry.drainForRun in isolation. Nothing previously exercised the
// actual `close` handler registered by registerAgUiRoutes — deleting
// `stream.stopHeartbeat()` (or any of the other three teardown calls) from that
// handler left the full suite green. This test fails on that deletion.
//
// WHY the client abort (and no autoDestroy hook): the handler listens on
// `reply.raw` (the response socket), which emits 'close' only on real
// connection termination — NOT on the spurious req.raw autoDestroy-on-body-read
// that fires before the awaits resolve. So the honest way to exercise teardown
// is a genuine client disconnect: destroy the client request, the server socket
// closes, reply.raw emits 'close'. No internal-stream hook needed.
import { test } from "node:test";
import assert from "node:assert/strict";
import { request as httpRequest, type ClientRequest } from "node:http";
import Fastify from "fastify";
import pino from "pino";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import { registerAgUiRoutes } from "../src/routes/agui.js";
import { BrokerClients } from "../src/broker/clients.js";
import { SouthClient } from "../src/broker/south.js";
import { ApprovalRegistry } from "../src/agui/hitl.js";
import { ChildSupervisor } from "../src/ipc/supervisor.js";
import type { SpawnChildFn, SupervisorDeps, ProviderCredentialResolver } from "../src/ipc/supervisor.js";
import { makePairedChannel, ParentLink, ChildLink } from "../src/ipc/protocol.js";
import type { EgressProxy, RegisterResult, RegisterOptions } from "../src/llm/egress-proxy.js";
import type { Config } from "../src/config.js";
import type { JwksResolver } from "../src/auth/verify.js";

// ── Auth fixtures (mirrors files-move-dir-route.test.ts / workflow-versions.test.ts) ──

async function makeKey() {
  const { publicKey, privateKey } = await generateKeyPair("RS256");
  const jwk: JWK = { ...(await exportJWK(publicKey)), kid: "k1", alg: "RS256", use: "sig" };
  return { privateKey, jwk };
}

async function localResolver(jwk: JWK): Promise<JwksResolver> {
  const key = await importJWK(jwk, "RS256");
  return () => Promise.resolve(key);
}

const VERIFY_OPTS = { issuer: "http://localhost:18080/realms/aikonos", audience: "aikonos-broker" };
const TENANT_ID = "tenant-1";
const USER_SUB = "alice@example.com"; // agentForUser default mapping → "alice-agent"

async function mintToken(privateKey: Awaited<ReturnType<typeof generateKeyPair>>["privateKey"]) {
  return new SignJWT({ sub: USER_SUB, email: USER_SUB, tenant_id: TENANT_ID })
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
    brokerServerName: "", tlsCert: "", tlsKey: "", tlsCa: "", gatewaySpiffeId: "",
    port: 8080, defaultTenantId: TENANT_ID, oidcIssuer: "", oidcJwksUrl: "", oidcAudience: "",
    oidcSubjectClaim: "sub", oidcTenantClaim: "tenant_id", schedulerEnabled: false,
    schedulerTickMs: 30000, schedulerClaimLimit: 10, schedulerRunTimeoutMs: 180000,
    agentForUserOverrides: {}, externalPort: 8090, externalCorsOrigins: [],
    externalRateLimit: 60, threadTtlMs: 1800000, maxChildren: 32, childTtlMs: 1800000,
    natsUrl: "nats://nats:4222", auditSubject: "aikonos.audit.>", egressTimeoutMs: 120000, brokerTimeoutMs: 30000, rateLimitBreakerThreshold: 5, workflowReasonMaxTokens: 2048, maxLlmCallsPerRun: 100, approvalTimeoutMs: 900000, memorySemanticRecall: true, memoryEmbedTimeoutMs: 10000, subagentMaxWidth: 3, subagentBranchTimeoutMs: 180000,
  };
}

// ── Fake child pool (same idiom as agui-supervisor.test.ts, trimmed to what
// getOrSpawn/run actually touch for this route's default-agent, non-auto path) ──

function makeFakeSpawn(): SpawnChildFn {
  return () => {
    const [parentSide] = makePairedChannel();
    const link = new ParentLink(parentSide);
    link.onExit = () => {};
    link.kill = () => {};
    return link;
  };
}

function makeFakeProxy(): EgressProxy {
  return {
    register: (_opts: RegisterOptions): RegisterResult => ({
      childToken: "fake-token",
      childBaseUrl: "http://127.0.0.1:9999/fake-token/v1",
    }),
    unregister: () => {},
    resetRunBudget: () => {},
    listen: async () => 9999,
    close: async () => {},
  } as unknown as EgressProxy;
}

function makeFakeDeps(): SupervisorDeps {
  return {
    south: {
      getLlmProviders: async () => ({ providers: [] }),
      getTenantModel: async () => ({ model: "anthropic/claude-sonnet-4.6" }),
      listUserSkills: async () => ({ skills: [] }),
      listUserAgentSkills: async () => ({ bundles: [] }),
      listAccessibleMcpServersForAgent: async () => ({ connections: [] }),
      listMcpServerToolsSouth: async () => ({ tools: [] }),
      getAgentSpec: async () => ({ found: false }),
    },
    cfg: { llmModel: "anthropic/claude-sonnet-4.6", defaultTenantId: TENANT_ID },
  };
}

function makeFakeCredentials(): ProviderCredentialResolver {
  return async () => ({
    upstreamBaseUrl: "https://openrouter.ai/api/v1",
    apiKey: "dummy",
    modelId: "anthropic/claude-sonnet-4.6",
    modelAllowlist: ["anthropic/claude-sonnet-4.6"],
    fallbacks: [],
  });
}

function makeFakeBridge() {
  return {
    gate: async () => ({ allow: true }),
    execute: async () => ({ ok: true, output: "result" }),
    delegate: async () => ({ ok: true }),
    saveWorkflow: async () => ({ ok: true }),
    runWorkflow: async () => ({ ok: true, result: null }),
    listWorkflows: async () => ({ ok: true, items: [] }),
    publishWorkflow: async () => ({ ok: true }),
    proposeWorkflow: async () => ({ ok: true }),
    analyzeImage: async () => ({ ok: true, text: "" }),
    scheduleWorkflow: async () => ({ ok: true }),
    setToken: () => {},
    setApprover: () => {},
  };
}

// Sends a request to `app`'s real listening server and resolves with the client
// request once the first SSE frame arrives — i.e. once the route handler has
// reached `stream.runStarted()`, which runs strictly after the close listener is
// registered. The caller then destroys the returned request to simulate a
// genuine client disconnect.
async function postAndWaitForRunStarted(port: number, token: string, body: unknown): Promise<ClientRequest> {
  return new Promise<ClientRequest>((resolve, reject) => {
    const payload = JSON.stringify(body);
    const req = httpRequest(
      {
        host: "127.0.0.1",
        port,
        path: "/agui",
        method: "POST",
        headers: {
          authorization: `Bearer ${token}`,
          "content-type": "application/json",
          "content-length": Buffer.byteLength(payload),
        },
      },
      (res) => {
        res.on("error", () => {}); // the test destroys the client request, tearing down this socket
        let buf = "";
        res.on("data", (d: Buffer) => {
          buf += d.toString();
          if (buf.includes("RUN_STARTED")) resolve(req);
        });
      },
    );
    req.on("error", (err) => {
      // The test aborts this request to simulate disconnect; the resulting
      // ECONNRESET is expected. Reject only on an unexpected pre-abort error.
      if ((err as NodeJS.ErrnoException).code === "ECONNRESET") return;
      reject(err);
    });
    req.write(payload);
    req.end();
  });
}

test("POST /agui close handler: stops the ping heartbeat AND runs drainForRun/abortRun/clearRunContext teardown", async () => {
  const { privateKey, jwk } = await makeKey();
  const jwksResolver = await localResolver(jwk);
  const token = await mintToken(privateKey);

  const clients: BrokerClients = Object.create(BrokerClients.prototype);
  const south: SouthClient = Object.create(SouthClient.prototype);
  Object.assign(south, {
    getAgentSpec: async () => ({ found: false }),
    listUserAgentSkills: async () => ({ bundles: [] }),
  });
  clients.south = south;

  const approvals = new ApprovalRegistry();
  const drainCalls: Array<{ runId: string; ok: boolean }> = [];
  const originalDrainForRun = ApprovalRegistry.prototype.drainForRun.bind(approvals);
  approvals.drainForRun = (runId: string, ok = false) => {
    drainCalls.push({ runId, ok });
    originalDrainForRun(runId, ok);
  };

  const supervisor = new ChildSupervisor(
    makeFakeProxy(),
    makeFakeDeps(),
    () => makeFakeBridge(),
    makeFakeSpawn(),
    makeFakeCredentials(),
  );

  const runId = "run-close-teardown";
  const identity = { tenantId: TENANT_ID, userId: USER_SUB, agentId: "alice-agent" };
  const childKey = supervisor.keyFor(identity);
  // Pre-spawn on the same key the route will derive, so the handle instance
  // the route uses below is the one we can instrument here.
  const handle = await supervisor.getOrSpawn(childKey, identity);

  const abortRunCalls: string[] = [];
  const clearRunContextCalls: string[] = [];
  const originalAbortRun = handle.abortRun.bind(handle);
  const originalClearRunContext = handle.clearRunContext.bind(handle);
  handle.abortRun = (rid: string) => {
    abortRunCalls.push(rid);
    originalAbortRun(rid);
  };
  handle.clearRunContext = (rid: string) => {
    clearRunContextCalls.push(rid);
    originalClearRunContext(rid);
  };

  // Spy on the global ping interval (15_000ms, agui/stream.ts's PING_MS) so we
  // can prove it was cleared without waiting 15s in the test. Filters strictly
  // on the 15_000 delay so we don't intercept Fastify's/other unrelated timers.
  const realSetInterval = global.setInterval;
  const realClearInterval = global.clearInterval;
  const pingIntervalIds: NodeJS.Timeout[] = [];
  const clearedIntervalIds: NodeJS.Timeout[] = [];
  global.setInterval = ((fn: (...args: unknown[]) => void, ms?: number, ...rest: unknown[]) => {
    const id = realSetInterval(fn, ms, ...rest);
    if (ms === 15_000) pingIntervalIds.push(id);
    return id;
  }) as typeof setInterval;
  global.clearInterval = ((id: NodeJS.Timeout | undefined) => {
    clearedIntervalIds.push(id as NodeJS.Timeout);
    return realClearInterval(id);
  }) as typeof clearInterval;

  const app = Fastify({ logger: false });
  try {
    registerAgUiRoutes(app, {
      clients,
      jwksResolver,
      verifyOpts: VERIFY_OPTS,
      approvals,
      supervisor,
      cfg: fakeConfig(),
      log: pino({ level: "silent" }),
    });
    await app.listen({ port: 0, host: "127.0.0.1" });
    const address = app.server.address();
    if (address === null || typeof address === "string") {
      throw new Error("expected a bound TCP address");
    }

    const clientReq = await postAndWaitForRunStarted(address.port, token, {
      prompt: "hello",
      threadId: "thread-close-teardown",
      runId,
    });

    // The real close listener is now attached (RUN_STARTED is emitted after
    // it). Simulate a genuine mid-run client disconnect: aborting the client
    // request closes the server socket, firing reply.raw's 'close'.
    clientReq.destroy();

    // Poll briefly — destroy()'s 'close' emission is asynchronous.
    for (let i = 0; i < 40 && drainCalls.length === 0; i++) {
      await new Promise((resolve) => setTimeout(resolve, 25));
    }

    assert.equal(pingIntervalIds.length, 1, "AGUIStream must have started exactly one 15s ping interval");
    assert.ok(
      clearedIntervalIds.includes(pingIntervalIds[0]),
      "the ping interval must be cleared by the close handler's stream.stopHeartbeat() call",
    );
    assert.deepEqual(drainCalls, [{ runId, ok: false }], "close handler must call approvals.drainForRun(runId, false)");
    assert.deepEqual(abortRunCalls, [runId], "close handler must call handle.abortRun(runId)");
    assert.ok(clearRunContextCalls.includes(runId), "close handler must call handle.clearRunContext(runId)");
  } finally {
    global.setInterval = realSetInterval;
    global.clearInterval = realClearInterval;
    // The hijacked SSE socket is outside Fastify's normal connection tracking;
    // force-close it so app.close() doesn't wait out its keep-alive timeout.
    app.server.closeAllConnections();
    await app.close();
    supervisor.dispose();
  }
});

// CP6: the close handler must also sweep this
// run's subagent branch children — a separate ChildSupervisor call from the
// three above, since the parent chat `handle` never reaches a branch's own
// handle. This is the route-level pin for that wiring; supervisor.test.ts's
// "CP6 the real risk" test proves the sweep itself cannot hang.
test("POST /agui close handler: sweeps this run's subagent branch children via supervisor.evictBranchesForRun(runId, …)", async () => {
  const { privateKey, jwk } = await makeKey();
  const jwksResolver = await localResolver(jwk);
  const token = await mintToken(privateKey);

  const clients: BrokerClients = Object.create(BrokerClients.prototype);
  const south: SouthClient = Object.create(SouthClient.prototype);
  Object.assign(south, {
    getAgentSpec: async () => ({ found: false }),
    listUserAgentSkills: async () => ({ bundles: [] }),
  });
  clients.south = south;

  const approvals = new ApprovalRegistry();
  const supervisor = new ChildSupervisor(
    makeFakeProxy(),
    makeFakeDeps(),
    () => makeFakeBridge(),
    makeFakeSpawn(),
    makeFakeCredentials(),
  );

  const runId = "run-close-teardown-branches";
  const identity = { tenantId: TENANT_ID, userId: USER_SUB, agentId: "alice-agent" };
  await supervisor.getOrSpawn(supervisor.keyFor(identity), identity);

  const evictBranchesForRunCalls: { runId: string; reason: string }[] = [];
  const originalEvictBranchesForRun = supervisor.evictBranchesForRun.bind(supervisor);
  supervisor.evictBranchesForRun = (rid: string, reason: string) => {
    evictBranchesForRunCalls.push({ runId: rid, reason });
    originalEvictBranchesForRun(rid, reason);
  };

  const app = Fastify({ logger: false });
  try {
    registerAgUiRoutes(app, {
      clients,
      jwksResolver,
      verifyOpts: VERIFY_OPTS,
      approvals,
      supervisor,
      cfg: fakeConfig(),
      log: pino({ level: "silent" }),
    });
    await app.listen({ port: 0, host: "127.0.0.1" });
    const address = app.server.address();
    if (address === null || typeof address === "string") {
      throw new Error("expected a bound TCP address");
    }

    const clientReq = await postAndWaitForRunStarted(address.port, token, {
      prompt: "hello",
      threadId: "thread-close-teardown-branches",
      runId,
    });

    clientReq.destroy();

    for (let i = 0; i < 40 && evictBranchesForRunCalls.length === 0; i++) {
      await new Promise((resolve) => setTimeout(resolve, 25));
    }

    assert.deepEqual(
      evictBranchesForRunCalls,
      [{ runId, reason: "run teardown" }],
      "close handler must call supervisor.evictBranchesForRun scoped to THIS run's id, exactly once",
    );
  } finally {
    app.server.closeAllConnections();
    await app.close();
    supervisor.dispose();
  }
});
