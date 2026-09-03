// POST /agui history[] validation. Mirrors agui-user-instructions.test.ts's fixtures: a malformed
// or oversized history must be a 400 BEFORE the SSE hijack and before any
// child is forked — the same caps the external :8090 surface already enforces
// (src/history-validation.ts, shared by both).
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

function makeCountingSpawn(): { spawnFn: SpawnChildFn; count: number[] } {
  const count = [0];
  const spawnFn: SpawnChildFn = () => {
    count[0]++;
    throw new Error("must not be reached — validation must reject before any spawn");
  };
  return { spawnFn, count };
}

const dummyCredentials: ProviderCredentialResolver = async () => {
  throw new Error("must not be reached — validation must reject before credential resolution");
};

function postAgui(port: number, token: string, body: unknown): Promise<{ status: number; body: string }> {
  return new Promise((resolve, reject) => {
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
        res.on("end", () => resolve({ status: res.statusCode ?? 0, body: buf }));
      },
    );
    req.on("error", reject);
    req.write(payload);
    req.end();
  });
}

test("POST /agui: malformed or oversized history is a 400 before any child spawn", async () => {
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
    dummyCredentials,
  );

  const app = Fastify({ logger: false });
  try {
    registerAgUiRoutes(app, {
      clients,
      jwksResolver,
      verifyOpts: VERIFY_OPTS,
      approvals: new ApprovalRegistry(),
      supervisor,
      cfg: fakeConfig(),
      log: pino({ level: "silent" }),
    });
    await app.listen({ port: 0, host: "127.0.0.1" });
    const address = app.server.address();
    if (address === null || typeof address === "string") {
      throw new Error("expected a bound TCP address");
    }

    const notAnArray = await postAgui(address.port, token, {
      prompt: "hello", threadId: "t1", runId: "r1", history: "not-an-array",
    });
    assert.equal(notAnArray.status, 400, "non-array history must be a clean 400");

    const badRole = await postAgui(address.port, token, {
      prompt: "hello", threadId: "t2", runId: "r2",
      history: [{ role: "system", content: "x" }],
    });
    assert.equal(badRole.status, 400, "an invalid role must be a clean 400");

    const tooManyTurns = await postAgui(address.port, token, {
      prompt: "hello", threadId: "t3", runId: "r3",
      history: Array.from({ length: 101 }, () => ({ role: "user", content: "x" })),
    });
    assert.equal(tooManyTurns.status, 400, "over 100 turns must be a clean 400");
    assert.match(tooManyTurns.body, /100 turns/);

    const overByteCap = await postAgui(address.port, token, {
      prompt: "hello", threadId: "t4", runId: "r4",
      history: [{ role: "user", content: "x".repeat(201 * 1024) }],
    });
    assert.equal(overByteCap.status, 400, "over 200 KiB of content must be a clean 400");

    assert.equal(spawnCount[0], 0, "no child may be forked for a request rejected by history validation");
  } finally {
    app.server.closeAllConnections();
    await app.close();
    supervisor.dispose();
  }
});
