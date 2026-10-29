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
import { BranchQuotaError, ChildSupervisor } from "../src/ipc/supervisor.js";
import type { SpawnChildFn, SupervisorDeps, ProviderCredentialResolver } from "../src/ipc/supervisor.js";
import type { EgressProxy, RegisterResult, RegisterOptions } from "../src/llm/egress-proxy.js";
import type { Config } from "../src/config.js";
import type { JwksResolver } from "../src/auth/verify.js";
import { failedPreconditionError } from "../src/http-errors.js";
import type { AgentSpec } from "../src/pi/session.js";

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
    externalPort: 8090, externalCorsOrigins: [],
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
    resolveRunAgent: async () => ({ agentId: "00000000-0000-4000-8000-0000000000de", isDefault: true }), // AG-01 tenant default agent
    listUserAgentSkills: async () => ({ bundles: [] }),
  });
  clients.south = south;
  // F171: /agui mints the session grant its south reads present.
  clients.north = { mintSessionGrant: async () => ({ ownerGrant: "grant-test" }) } as unknown as BrokerClients["north"];

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

// ── F129 / F64: who the run is for ────────────────────────────────────────────

// postAgui runs one POST /agui against the given south fake and config.
async function postAgui(
  southFns: Record<string, unknown>,
  cfg: Config,
  credentials: ProviderCredentialResolver = makeRejectingCredentials(),
): Promise<{ status: number; body: string }> {
  const { privateKey, jwk } = await makeKey();
  const jwksResolver = await localResolver(jwk);
  const token = await mintToken(privateKey);
  const clients: BrokerClients = Object.create(BrokerClients.prototype);
  const south: SouthClient = Object.create(SouthClient.prototype);
  Object.assign(south, southFns);
  clients.south = south;
  // F171: /agui mints the session grant its south reads present.
  clients.north = { mintSessionGrant: async () => ({ ownerGrant: "grant-test" }) } as unknown as BrokerClients["north"];
  const supervisor = new ChildSupervisor(makeFakeProxy(), makeFakeDeps(), () => {
    throw new Error("no bridge expected");
  }, makeCountingSpawn().spawnFn, credentials);
  const app = Fastify({ logger: false });
  try {
    registerAgUiRoutes(app, {
      clients, jwksResolver, verifyOpts: VERIFY_OPTS, approvals: new ApprovalRegistry(), supervisor, cfg,
      log: pino({ level: "silent" }),
    });
    await app.listen({ port: 0, host: "127.0.0.1" });
    const address = app.server.address();
    if (address === null || typeof address === "string") throw new Error("expected a bound TCP address");
    return await postWithTimeout(address.port, token, { prompt: "hello", threadId: "t", runId: "r" }, 5000);
  } finally {
    app.server.closeAllConnections();
    await app.close();
    supervisor.dispose();
  }
}

test("F129: a user the broker refuses as blocked gets 403 and no chat — not a silently skipped skill/memory read", async () => {
  // WHY: the south reads of a chat turn were treated as optional (warn and go
  // on), so a blocked user with an unexpired token still got a conversation.
  const blocked = Object.assign(new Error("7 PERMISSION_DENIED: user is blocked"), { code: 7, details: "user is blocked" });
  const result = await postAgui({
    listUserAgentSkills: async () => { throw blocked; },
    resolveRunAgent: async () => ({ agentId: "00000000-0000-4000-8000-0000000000de", isDefault: true }),
    getAgentSpec: async () => ({ found: false }),
  }, fakeConfig());

  assert.equal(result.status, 403);
  assert.match(result.body, /user is blocked/);
});

test("F64: the run's south reads use the token's tenant, never the config default", async () => {
  const seen: string[] = [];
  await postAgui({
    listUserAgentSkills: async (req: { tenantId: string }) => { seen.push(req.tenantId); return { bundles: [] }; },
    resolveRunAgent: async (req: { tenantId: string }) => {
      seen.push(req.tenantId);
      return { agentId: "00000000-0000-4000-8000-0000000000de", isDefault: true };
    },
    getAgentSpec: async () => ({ found: false }),
  }, { ...fakeConfig(), defaultTenantId: "some-other-tenant" });

  assert.ok(seen.length >= 2, "the turn must have reached its south reads");
  assert.deepEqual([...new Set(seen)], [TENANT_ID]);
});

// ── F203: the named agent's provider pin reaches the child ──────────────────

const NAMED_AGENT = "00000000-0000-4000-8000-0000000000aa";

test("F203: /agui spawns a named agent's child with its spec — the allowedProviders pin reaches credential resolution", async () => {
  // WHY: getOrSpawn was called without the spec, so a pinned agent's chat ran
  // on the tenant default provider. Reverting `}, spec);` must fail this test.
  const seen: (AgentSpec | undefined)[] = [];
  await postAgui({
    listUserAgentSkills: async () => ({ bundles: [] }),
    resolveRunAgent: async () => ({ agentId: NAMED_AGENT, isDefault: false }),
    getAgentSpec: async () => ({ found: true, approvalMode: "needs_approval", skills: [], allowedProviders: ["onprem"], preferredProvider: "onprem", llmModel: "m" }),
  }, fakeConfig(), async (_identity, spec) => {
    seen.push(spec);
    throw failedPreconditionError("stop after resolution");
  });

  assert.equal(seen.length, 1, "the child spawn must reach credential resolution once");
  assert.deepEqual(seen[0]?.allowedProviders, ["onprem"]);
  assert.equal(seen[0]?.preferredProvider, "onprem");
});

test("F203: a getAgentSpec failure for a named agent refuses the run — no unpinned fall-back spawn", async () => {
  let resolved = 0;
  const result = await postAgui({
    listUserAgentSkills: async () => ({ bundles: [] }),
    resolveRunAgent: async () => ({ agentId: NAMED_AGENT, isDefault: false }),
    getAgentSpec: async () => { throw Object.assign(new Error("14 UNAVAILABLE: broker down"), { code: 14, details: "broker down" }); },
  }, fakeConfig(), async () => {
    resolved++;
    throw failedPreconditionError("must not be reached");
  });

  assert.equal(result.status, 502);
  assert.doesNotMatch(result.body, /RUN_STARTED/, "no stream is opened");
  assert.equal(resolved, 0, "no child is spawned without the agent's spec");
});

test("F58: a per-user share refusal (BranchQuotaError) reaches the caller as 429, not the pool-wide 503", async () => {
  const result = await postAgui({
    listUserAgentSkills: async () => ({ bundles: [] }),
    resolveRunAgent: async () => ({ agentId: "00000000-0000-4000-8000-0000000000de", isDefault: true }),
    getAgentSpec: async () => ({ found: false }),
  }, fakeConfig(), async () => { throw new BranchQuotaError(4, "chats and subtasks"); });

  assert.equal(result.status, 429);
  assert.match(result.body, /too many concurrent chats and subtasks/);
});
