// Per-user chat instructions (webui settings modal → /agui → child system prompt).
//
// Pins two contracts:
//  1. appendUserInstructions folds the user's text into the system prompt as a
//     clearly-labeled, governance-subordinate block — and is a no-op for
//     empty/whitespace input.
//  2. POST /agui rejects an oversized or non-string userInstructions field with
//     a 400 BEFORE the SSE hijack and before any child is forked. The webui
//     textarea caps at the same limit, so an overflow here is a hand-crafted
//     request — fail loud, never truncate silently.
import { test } from "node:test";
import assert from "node:assert/strict";
import { request as httpRequest } from "node:http";
import Fastify from "fastify";
import pino from "pino";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import {
  appendUserInstructions,
  USER_INSTRUCTIONS_MAX_CHARS,
} from "../src/pi/system-prompt.js";
import { registerAgUiRoutes } from "../src/routes/agui.js";
import { BrokerClients } from "../src/broker/clients.js";
import { SouthClient } from "../src/broker/south.js";
import { ApprovalRegistry } from "../src/agui/hitl.js";
import { ChildSupervisor } from "../src/ipc/supervisor.js";
import type { SpawnChildFn, SupervisorDeps, ProviderCredentialResolver } from "../src/ipc/supervisor.js";
import type { EgressProxy, RegisterResult, RegisterOptions } from "../src/llm/egress-proxy.js";
import type { Config } from "../src/config.js";
import type { JwksResolver } from "../src/auth/verify.js";

// ── appendUserInstructions unit contract ────────────────────────────────────

test("appendUserInstructions: empty or whitespace instructions leave the prompt unchanged", () => {
  const prompt = "base prompt";
  assert.equal(appendUserInstructions(prompt), prompt);
  assert.equal(appendUserInstructions(prompt, ""), prompt);
  assert.equal(appendUserInstructions(prompt, "   \n  "), prompt);
});

test("appendUserInstructions: appends a labeled block with the trimmed text and a governance-subordination note", () => {
  const out = appendUserInstructions("base prompt", "  answer in German  ");
  assert.ok(out.startsWith("base prompt"), "base prompt must be preserved verbatim at the front");
  assert.ok(out.includes("--- User instructions (user-provided preferences) ---"));
  assert.ok(out.includes("answer in German"));
  assert.ok(!out.includes("  answer in German  "), "instructions must be trimmed");
  assert.ok(
    out.includes("do not override the governance rules"),
    "the block must be explicitly subordinate to governance guidance",
  );
});

test("appendUserInstructions: caps instructions at USER_INSTRUCTIONS_MAX_CHARS even if the route check is bypassed", () => {
  const oversized = "x".repeat(USER_INSTRUCTIONS_MAX_CHARS + 500);
  const out = appendUserInstructions("base", oversized);
  assert.ok(out.includes("x".repeat(USER_INSTRUCTIONS_MAX_CHARS)));
  assert.ok(!out.includes("x".repeat(USER_INSTRUCTIONS_MAX_CHARS + 1)), "must be sliced to the cap");
});

// ── /agui route validation (fixtures mirror agui-spawn-failure.test.ts) ─────

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

test("POST /agui: oversized or non-string userInstructions is a 400 before any child spawn", async () => {
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

    const oversized = await postAgui(address.port, token, {
      prompt: "hello",
      threadId: "thread-ui-1",
      runId: "run-ui-1",
      userInstructions: "x".repeat(USER_INSTRUCTIONS_MAX_CHARS + 1),
    });
    assert.equal(oversized.status, 400, "oversized instructions must be a clean 400");
    assert.match(oversized.body, /2000/, "the error must name the limit");

    const nonString = await postAgui(address.port, token, {
      prompt: "hello",
      threadId: "thread-ui-2",
      runId: "run-ui-2",
      userInstructions: { evil: true },
    });
    assert.equal(nonString.status, 400, "non-string instructions must be a clean 400");

    assert.equal(spawnCount[0], 0, "no child may be forked for a request rejected by validation");
  } finally {
    app.server.closeAllConnections();
    await app.close();
    supervisor.dispose();
  }
});
