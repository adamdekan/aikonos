// CP1: a spawn failure (e.g. the
// resolved LLM provider has no API key) must reach the /agui caller as a
// prompt, caller-visible error — never a hang, never silence. getOrSpawn is
// called BEFORE the SSE socket is hijacked (src/routes/agui.ts), so a spawn
// rejection here is routed through the existing sendError mapper as a normal
// HTTP error response rather than an SSE RUN_ERROR frame; the webui's
// runAgui() (src/api/agui.js) already treats any non-2xx /agui response as a
// caller-visible error via handlers.onError — this test pins that the gateway
// side of that contract holds: the request settles promptly with a non-2xx
// status and a non-empty error body, and the child process is never forked.
import { test } from "node:test";
import assert from "node:assert/strict";
import { request as httpRequest } from "node:http";
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
import type { EgressProxy, RegisterResult, RegisterOptions } from "../src/llm/egress-proxy.js";
import type { Config } from "../src/config.js";
import type { JwksResolver } from "../src/auth/verify.js";
import { failedPreconditionError } from "../src/http-errors.js";

// ── Auth fixtures (mirrors agui-close-teardown.test.ts) ────────────────────

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
const USER_SUB = "alice@example.com";

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

// spawnFn that records how many times it was invoked — must stay 0 across
// this whole test: the credential resolver rejects before any fork happens.
function makeCountingSpawn(): { spawnFn: SpawnChildFn; count: number[] } {
  const count = [0];
  const spawnFn: SpawnChildFn = () => {
    count[0]++;
    throw new Error("must not be reached — spawn should fail before forking");
  };
  return { spawnFn, count };
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

// The credential resolver that reproduces CP1 Case A/B: throws the loud
// spawn-failure error (carrying the FAILED_PRECONDITION code the real
// resolveProviderCredentials attaches, so http-errors.ts surfaces the message
// instead of collapsing it to "internal error") instead of ever returning a
// usable credential set.
function makeRejectingCredentials(): ProviderCredentialResolver {
  return async () => {
    throw failedPreconditionError(
      'llm credentials unavailable: provider "openai-prod" has no API key available — re-enter it in Admin → LLM Providers',
    );
  };
}

// Posts to /agui and races the response against a timeout — this IS the
// "no hang" proof: if getOrSpawn's rejection were swallowed or left the
// request pending, this promise would only ever resolve via the timeout branch.
function postWithTimeout(port: number, token: string, body: unknown, timeoutMs: number): Promise<{ status: number; body: string }> {
  return new Promise((resolve, reject) => {
    let settled = false;
    const timer = setTimeout(() => {
      if (settled) return;
      settled = true;
      reject(new Error(`/agui did not respond within ${timeoutMs}ms — spawn failure was swallowed or hung`));
    }, timeoutMs);

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
        let buf = "";
        res.on("data", (d: Buffer) => { buf += d.toString(); });
        res.on("end", () => {
          if (settled) return;
          settled = true;
          clearTimeout(timer);
          resolve({ status: res.statusCode ?? 0, body: buf });
        });
      },
    );
    req.on("error", (err) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      reject(err);
    });
    req.write(payload);
    req.end();
  });
}

test("POST /agui: getOrSpawn rejection (spawn failure) settles the request with a caller-visible error — no hang, no child forked", async () => {
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
  const { spawnFn, count: spawnCount } = makeCountingSpawn();

  const supervisor = new ChildSupervisor(
    makeFakeProxy(),
    makeFakeDeps(),
    () => ({
      gate: async () => ({ allow: true }),
      execute: async () => ({ ok: true, output: null }),
      delegate: async () => ({ ok: true }),
      saveWorkflow: async () => ({ ok: true }),
      runWorkflow: async () => ({ ok: true, result: null }),
      listWorkflows: async () => ({ ok: true, items: [] }),
      publishWorkflow: async () => ({ ok: true }),
      proposeWorkflow: async () => ({ ok: true }),
      analyzeImage: async () => ({ ok: true, text: "" }),
      scheduleWorkflow: async () => ({ ok: true }),
    }),
    spawnFn,
    makeRejectingCredentials(),
  );

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

    const result = await postWithTimeout(
      address.port,
      token,
      { prompt: "hello", threadId: "thread-spawn-failure", runId: "run-spawn-failure" },
      5000,
    );

    assert.notEqual(result.status, 200, "a spawn failure must not report success");
    assert.ok(result.status >= 400, `expected an error status, got ${result.status}`);
    assert.ok(result.body.length > 0, "the error response body must not be empty (silence is the bug this pins against)");
    assert.match(
      result.body,
      /openai-prod/,
      "the response body must name the broken provider so the caller knows what to fix",
    );
    assert.match(
      result.body,
      /re-enter/i,
      "the response body must carry the remediation text — previously collapsed to a bare 'internal error' 500",
    );
    assert.equal(spawnCount[0], 0, "the child process must never be forked when credential resolution fails");
  } finally {
    app.server.closeAllConnections();
    await app.close();
    supervisor.dispose();
  }
});
