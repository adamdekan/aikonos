// Subagent timeline: /agui route-level integration test
//.
//
// WHY this test exists: subagent-run.test.ts proves the runner emits the right
// SubagentEvent objects; it does NOT prove routes/agui.ts's onSubagentEvent
// closure maps those objects into the correct AIKONOS_SUBAGENT_SPAWNED /
// AIKONOS_SUBAGENT_COMPLETED SSE CUSTOM frames (name + payload, including
// cost) — that mapping lives one layer up, at the route, and is the actual
// wire contract CP9 consumes. Mirrors agui-skill-match.test.ts's rig: a real
// registerAgUiRoutes wiring with a fake forked child and a bridge factory
// whose spawnSubagents stub calls the sink directly (real runSubagents
// machinery is out of scope here — that's subagent-run.test.ts's job).
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
import type { BridgeFactory } from "../src/ipc/bridge-server.js";
import { makePairedChannel, ParentLink, ChildLink } from "../src/ipc/protocol.js";
import { AIKONOS_SUBAGENT_SPAWNED, AIKONOS_SUBAGENT_COMPLETED } from "../src/agui/events.js";
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

// ── fake child + spawn (mirrors agui-skill-match.test.ts) ───────────────────
//
// On receiving the prompt, the fake child first fires a spawn-subagents IPC
// request (exactly what the real Pi loop's spawn_subagents tool call sends,
// bridge-server.ts's dispatch table), then finishes the turn normally once
// the reply lands — proving the SSE frames were emitted from the REAL
// setRunContext → bridgeServer → makeBridge → bridge.spawnSubagents path, not
// a shortcut.

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
    childLink.send({
      kind: "spawn-subagents",
      seq: 1,
      runId: msg.runId,
      branches: [{ task: "research A", role: "writer" }],
      aggregatorInstruction: "combine the findings",
    });
  });
  childLink.on("spawn-subagents-result", () => {
    const runId = promptsReceived[0]?.runId ?? "";
    childLink.send({ kind: "text_delta", runId, delta: "ok" });
    childLink.send({ kind: "done", runId });
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
      // subagents is granted because the fake child below sends a real
      // spawn-subagents IPC op. Since CP2
      // the parent refuses that op unless spawn_subagents is in the child's own
      // plan-derived tool set, so an empty skill list would refuse the fan-out
      // this test's SSE-frame mapping depends on.
      listUserSkills: async () => ({ skills: ["subagents"] }),
      listUserAgentSkills: async () => ({ bundles: [] }),
      listAccessibleMcpServersForAgent: async () => ({ connections: [] }),
      listMcpServerToolsSouth: async () => ({ tools: [] }),
      getAgentSpec: async () => ({ found: false }),
    },
    cfg: { llmModel: "anthropic/claude-sonnet-4.6", defaultTenantId: TENANT_ID },
  };
}

// bridgeFactory captures the injected onSubagentEvent sink (the 6th
// BridgeFactory param, threaded from routes/agui.ts's setRunContext call) and
// its spawnSubagents stub calls it directly with a representative
// spawned+completed pair — the property under test is routes/agui.ts's own
// mapping of those events into SSE frames, not the runner's outcome logic.
const bridgeFactory: BridgeFactory = (_identity, _approver, _budget, _runId, _sessionId, onSubagentEvent) => ({
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
  spawnSubagents: async () => {
    onSubagentEvent?.({ kind: "spawned", index: 0, task: "research A", role: "writer" });
    onSubagentEvent?.({ kind: "completed", index: 0, task: "research A", role: "writer", ok: true, cost: 0.05 });
    return { ok: true, branches: [], synthesis: "combined" };
  },
});

// ── auth fixtures (mirrors agui-skill-match.test.ts) ────────────────────────

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

interface Rig {
  app: ReturnType<typeof Fastify>;
  port: number;
  token: string;
  spawn: ReturnType<typeof makeFakeSpawn>;
  supervisor: ChildSupervisor;
  destroyLastRequest: () => void;
}

async function makeRig(): Promise<Rig> {
  const { privateKey, jwk } = await makeKey();
  const jwksResolver = await localResolver(jwk);
  const token = await mintToken(privateKey);

  const clients: BrokerClients = Object.create(BrokerClients.prototype);
  const south: SouthClient = Object.create(SouthClient.prototype);
  Object.assign(south, {
    getAgentSpec: async () => ({ found: false }),
    listUserAgentSkills: async () => ({ bundles: [] }),
    getOrgSettings: async () => ({ settings: { unattendedAllowed: true } }),
  });
  clients.south = south;

  const spawn = makeFakeSpawn();
  const supervisor = new ChildSupervisor(
    makeFakeProxy(),
    makeFakeDeps(),
    bridgeFactory,
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
  await app.listen({ port: 0, host: "127.0.0.1" });
  const address = app.server.address();
  if (address === null || typeof address === "string") {
    throw new Error("expected a bound TCP address");
  }

  return {
    app, port: address.port, token, spawn, supervisor,
    destroyLastRequest: () => lastRawReq?.destroy(),
  };
}

async function teardown(rig: Rig): Promise<void> {
  rig.app.server.closeAllConnections();
  await rig.app.close();
  rig.supervisor.dispose();
}

// ── tests ─────────────────────────────────────────────────────────────────────

test("POST /agui: a spawn_subagents branch event emits AIKONOS_SUBAGENT_SPAWNED then AIKONOS_SUBAGENT_COMPLETED carrying cost", async () => {
  const rig = await makeRig();
  try {
    const body = await postAgui(rig.port, rig.token, {
      prompt: "fan out a research subtask", threadId: "thread-1", runId: "run-1",
    });
    rig.destroyLastRequest();

    assert.ok(body.includes(AIKONOS_SUBAGENT_SPAWNED), "spawned frame must be present");
    assert.ok(body.includes(AIKONOS_SUBAGENT_COMPLETED), "completed frame must be present");

    const spawnedIdx = body.indexOf(AIKONOS_SUBAGENT_SPAWNED);
    const completedIdx = body.indexOf(AIKONOS_SUBAGENT_COMPLETED);
    assert.ok(spawnedIdx < completedIdx, "spawned must precede completed on the wire");

    const spawnedLine = body.split("\n").find((l) => l.startsWith("data:") && l.includes(AIKONOS_SUBAGENT_SPAWNED));
    assert.ok(spawnedLine, "expected a data: line carrying the spawned event");
    const spawnedEvt = JSON.parse(spawnedLine!.slice("data:".length).trim());
    assert.equal(spawnedEvt.name, AIKONOS_SUBAGENT_SPAWNED);
    assert.deepEqual(spawnedEvt.value, { index: 0, task: "research A", role: "writer" });

    const completedLine = body.split("\n").find((l) => l.startsWith("data:") && l.includes(AIKONOS_SUBAGENT_COMPLETED));
    assert.ok(completedLine, "expected a data: line carrying the completed event");
    const completedEvt = JSON.parse(completedLine!.slice("data:".length).trim());
    assert.equal(completedEvt.name, AIKONOS_SUBAGENT_COMPLETED);
    assert.deepEqual(completedEvt.value, { index: 0, task: "research A", role: "writer", ok: true, cost: 0.05 });
  } finally {
    await teardown(rig);
  }
});
