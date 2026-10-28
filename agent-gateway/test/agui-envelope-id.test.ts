// AG-17: /agui envelopeId — the inbox "Send to agent" / "New session" chat
// carries the envelope it came from; the route validates it as a UUID and
// binds it on the run identity so the bridge's north CreateTask forwards it
// (governance-agent-id.test.ts pins that last hop).
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
import {
  ChildSupervisor,
  type SpawnChildFn,
  type SupervisorDeps,
  type ProviderCredentialResolver,
} from "../src/ipc/supervisor.js";
import type { RunIdentity } from "../src/ipc/bridge-server.js";
import { makePairedChannel, ParentLink, ChildLink } from "../src/ipc/protocol.js";
import type { PromptMessage } from "../src/ipc/protocol.js";
import type { EgressProxy, RegisterResult, RegisterOptions } from "../src/llm/egress-proxy.js";
import type { Config } from "../src/config.js";
import type { JwksResolver } from "../src/auth/verify.js";
import type { IncomingMessage } from "node:http";

// See agui-skill-match.test.ts for why autoDestroy must be disabled on req.raw.
interface ReadableWithAutoDestroy {
  _readableState: { autoDestroy: boolean };
}

function hasReadableState(v: unknown): v is ReadableWithAutoDestroy {
  return typeof v === "object" && v !== null && "_readableState" in v;
}

interface FakeChild {
  parentLink: ParentLink;
  childLink: ChildLink;
  promptsReceived: PromptMessage[];
}

function makeFakeChild(): FakeChild {
  const [parentSide, childSide] = makePairedChannel();
  const link = new ParentLink(parentSide);
  link.onExit = () => {};
  link.kill = () => {};

  const childLink = new ChildLink(childSide);
  const promptsReceived: PromptMessage[] = [];
  childLink.on("prompt", (msg) => {
    promptsReceived.push(msg);
    childLink.send({ kind: "text_delta", runId: msg.runId, delta: "ok" });
    childLink.send({ kind: "done", runId: msg.runId });
  });

  return { parentLink: link, childLink, promptsReceived };
}

function makeFakeSpawn(): { spawnFn: SpawnChildFn; children: FakeChild[] } {
  const children: FakeChild[] = [];
  const spawnFn: SpawnChildFn = () => {
    const child = makeFakeChild();
    children.push(child);
    return child.parentLink;
  };
  return { spawnFn, children };
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

const dummyCredentials: ProviderCredentialResolver = async () => ({
  upstreamBaseUrl: "https://openrouter.ai/api/v1",
  apiKey: "dummy",
  modelId: "anthropic/claude-sonnet-4.6",
  modelAllowlist: ["anthropic/claude-sonnet-4.6"],
  fallbacks: [],
});

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
    natsUrl: "nats://nats:4222", auditSubject: "aikonos.audit.>", egressTimeoutMs: 120000,
    brokerTimeoutMs: 30000, rateLimitBreakerThreshold: 5, workflowReasonMaxTokens: 2048,
    maxLlmCallsPerRun: 100, approvalTimeoutMs: 900000, memorySemanticRecall: true, memoryEmbedTimeoutMs: 10000, subagentMaxWidth: 3, subagentBranchTimeoutMs: 180000,
  };
}

function postAgui(port: number, token: string, body: unknown): Promise<string> {
  return new Promise((resolve, reject) => {
    const payload = JSON.stringify(body);
    const req = httpRequest(
      {
        host: "127.0.0.1", port, path: "/agui", method: "POST",
        headers: {
          authorization: `Bearer ${token}`,
          "content-type": "application/json",
          "content-length": Buffer.byteLength(payload),
          connection: "close",
        },
      },
      (res) => {
        let buf = "";
        res.on("data", (d: Buffer) => { buf += d.toString(); });
        res.on("end", () => resolve(buf));
      },
    );
    req.on("error", reject);
    req.write(payload);
    req.end();
  });
}

const ENVELOPE_ID = "55555555-5555-4555-8555-555555555555";

async function run(extra: Record<string, unknown>): Promise<{ body: string; identities: RunIdentity[]; spawned: number }> {
  const { privateKey, jwk } = await makeKey();
  const jwksResolver = await localResolver(jwk);
  const token = await mintToken(privateKey);

  const clients: BrokerClients = Object.create(BrokerClients.prototype);
  const south: SouthClient = Object.create(SouthClient.prototype);
  Object.assign(south, {
    getAgentSpec: async () => ({ found: false }),
    listUserAgentSkills: async () => ({ bundles: [] }),
    getOrgSettings: async () => ({ settings: { unattendedAllowed: true } }),
    listMemoryConceptsSouth: async () => ({ concepts: [] }),
  });
  clients.south = south;

  const identities: RunIdentity[] = [];
  const spawn = makeFakeSpawn();
  const supervisor = new ChildSupervisor(
    makeFakeProxy(),
    makeFakeDeps(),
    (identity) => {
      identities.push(identity);
      return {
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
      };
    },
    spawn.spawnFn,
    dummyCredentials,
  );

  let lastRawReq: IncomingMessage | undefined;
  const app = Fastify({ logger: false });
  app.addHook("onRequest", (req, _reply, done) => {
    lastRawReq = req.raw;
    if (hasReadableState(req.raw)) req.raw._readableState.autoDestroy = false;
    done();
  });
  registerAgUiRoutes(app, {
    clients, jwksResolver, verifyOpts: VERIFY_OPTS,
    approvals: new ApprovalRegistry(), supervisor, cfg: fakeConfig(),
    log: pino({ level: "silent" }),
  });
  try {
    await app.listen({ port: 0, host: "127.0.0.1" });
    const address = app.server.address();
    if (address === null || typeof address === "string") throw new Error("expected a bound TCP address");
    const body = await postAgui(address.port, token, { prompt: "hello", threadId: "thread-1", runId: "run-1", ...extra });
    lastRawReq?.destroy();
    return { body, identities, spawned: spawn.children.length };
  } finally {
    app.server.closeAllConnections();
    await app.close();
    supervisor.dispose();
  }
}

test("POST /agui: a UUID envelopeId is bound on the run identity", async () => {
  const { identities } = await run({ envelopeId: ENVELOPE_ID });
  assert.equal(identities.length, 1, "one run context must be bound");
  assert.equal(identities[0].envelopeId, ENVELOPE_ID);
});

test("POST /agui: no envelopeId leaves the run identity without one", async () => {
  const { identities } = await run({});
  assert.equal(identities.length, 1);
  assert.equal(identities[0].envelopeId, undefined);
});

for (const bad of ["not-a-uuid", 42, { id: ENVELOPE_ID }]) {
  test(`POST /agui: envelopeId ${JSON.stringify(bad)} is a 400 before any child spawn`, async () => {
    const { body, identities, spawned } = await run({ envelopeId: bad });
    assert.match(body, /envelopeId must be a UUID/);
    assert.equal(identities.length, 0);
    assert.equal(spawned, 0);
  });
}
