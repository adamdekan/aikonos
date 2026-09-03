// auto-skill-loading CP2: /agui route-level integration tests.
//
// WHY these tests exist: skill-match.test.ts proves the matcher is pure and
// correct in isolation; these prove the route actually wires it in —
//   1. A matching prompt activates the eligible bundle(s) for the turn
//      (IPC-level: the child's PromptMessage carries activateSkillNames).
//   2. The AIKONOS_SKILLS_LOADED CUSTOM SSE event fires with the right payload
//      BEFORE any assistant text delta.
//   3. A disable_model_invocation bundle that matches is reported suppressed,
//      never activated.
//   4. The /command path (body.skillName) is unaffected — no auto-matching,
//      no AIKONOS_SKILLS_LOADED event, activateSkillName (singular) still set.
//
// All tests use a fake forked child (in-memory paired channel), mirroring
// test/agui-supervisor.test.ts. No real process.fork, no real LLM.
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
import { makePairedChannel, ParentLink, ChildLink } from "../src/ipc/protocol.js";
import { AIKONOS_SKILLS_LOADED } from "../src/agui/events.js";
import type { PromptMessage } from "../src/ipc/protocol.js";
import type { EgressProxy, RegisterResult, RegisterOptions } from "../src/llm/egress-proxy.js";
import type { Config } from "../src/config.js";
import type { JwksResolver } from "../src/auth/verify.js";
import type { IncomingMessage } from "node:http";

// _readableState is a Node internal (not in @types/node) — narrowed via a
// type guard rather than cast, per repo convention (never `as`). Mirrors
// test/agui-close-teardown.test.ts's documented workaround: Fastify's JSON
// body parser auto-destroys req.raw the instant its (already-buffered) body
// finishes, which would consume the 'close' event before our route's real
// listener attaches — leaving AGUIStream's ping interval to leak past test
// completion and hang the process. Disabling autoDestroy defers 'close' to
// the genuine socket teardown that happens when the response ends.
interface ReadableWithAutoDestroy {
  _readableState: { autoDestroy: boolean };
}

function hasReadableState(v: unknown): v is ReadableWithAutoDestroy {
  return typeof v === "object" && v !== null && "_readableState" in v;
}

// ── fake child + spawn (mirrors agui-supervisor.test.ts) ────────────────────

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
  // Immediately settle the run on receiving a prompt — these tests only
  // assert on the prompt message itself and the SSE frames written before
  // the LLM turn would have run, not on any real Pi loop behavior.
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

// ── granted bundles used across tests ────────────────────────────────────────
// "deployer": granted, keyword "deploy" — should auto-load.
// "vault": granted, disable_model_invocation, keyword "secret" — matched but
//   never activated; reported suppressed.
const GRANTED_BUNDLES = [
  {
    id: "b-deployer", name: "deployer", description: "Deploys the service",
    body: "## Deployer", allowedTools: ["web.fetch"], contextFork: false,
    disableModelInvocation: false, keywords: ["deploy"], filePaths: [],
  },
  {
    id: "b-vault", name: "vault", description: "Handles secrets",
    body: "## Vault", allowedTools: ["doc.write"], contextFork: false,
    disableModelInvocation: true, keywords: ["secret"], filePaths: [],
  },
];

function makeFakeDeps(): SupervisorDeps {
  return {
    south: {
      getLlmProviders: async () => ({ providers: [] }),
      getTenantModel: async () => ({ model: "anthropic/claude-sonnet-4.6" }),
      listUserSkills: async () => ({ skills: [] }),
      listUserAgentSkills: async () => ({ bundles: GRANTED_BUNDLES }),
      listAccessibleMcpServersForAgent: async () => ({ connections: [] }),
      listMcpServerToolsSouth: async () => ({ tools: [] }),
      getAgentSpec: async () => ({ found: false }),
    },
    cfg: { llmModel: "anthropic/claude-sonnet-4.6", defaultTenantId: TENANT_ID },
  };
}

// ── auth fixtures (mirrors agui-user-instructions.test.ts) ──────────────────

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

// postAgui reads the FULL SSE response body as text (the fake child ends the
// run immediately via "done", so the stream closes quickly).
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
          // Force the socket closed after this response — the SSE handler sets
          // "connection: keep-alive" unconditionally, and without this the
          // underlying socket lingers past the response, req.raw's 'close'
          // handler never fires, and AGUIStream's ping interval keeps the
          // process alive past test completion.
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

// ── rig ───────────────────────────────────────────────────────────────────────

interface Rig {
  app: ReturnType<typeof Fastify>;
  port: number;
  token: string;
  spawn: ReturnType<typeof makeFakeSpawn>;
  supervisor: ChildSupervisor;
  // Destroys the most recently received request's raw socket, firing the
  // route's `req.raw.on("close", ...)` teardown (stream.stopHeartbeat()) —
  // see the ReadableWithAutoDestroy comment above for why this is needed
  // instead of relying on the client's "connection: close" header alone.
  destroyLastRequest(): void;
}

// southOverrides:
// lets a test add/override south methods — e.g. listPersonalSkillsSouth — on
// top of the default fake, without every existing call site needing to know
// about it.
async function makeRig(southOverrides: Record<string, unknown> = {}): Promise<Rig> {
  const { privateKey, jwk } = await makeKey();
  const jwksResolver = await localResolver(jwk);
  const token = await mintToken(privateKey);

  const clients: BrokerClients = Object.create(BrokerClients.prototype);
  const south: SouthClient = Object.create(SouthClient.prototype);
  Object.assign(south, {
    getAgentSpec: async () => ({ found: false }),
    listUserAgentSkills: async () => ({ bundles: GRANTED_BUNDLES }),
    getOrgSettings: async () => ({ settings: { unattendedAllowed: true } }),
    ...southOverrides,
  });
  clients.south = south;

  const spawn = makeFakeSpawn();
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

test("POST /agui: a matching message activates the eligible bundle and emits AIKONOS_SKILLS_LOADED before the assistant stream", async () => {
  const rig = await makeRig();
  try {
    const body = await postAgui(rig.port, rig.token, {
      prompt: "please deploy this and don't leak the secret",
      threadId: "thread-1", runId: "run-1",
    });
    rig.destroyLastRequest();

    // IPC-level: the child's prompt carries activateSkillNames — the eligible
    // ("deployer") bundle activated, the flag-blocked ("vault") one did not.
    assert.equal(rig.spawn.children.length, 1);
    const prompt = rig.spawn.children[0].promptsReceived[0];
    assert.deepEqual(prompt.activateSkillNames, ["deployer"]);
    assert.equal(prompt.activateSkillName, undefined, "no /command was submitted");

    // SSE-level: the CUSTOM event carries both entries with correct status.
    assert.ok(body.includes(AIKONOS_SKILLS_LOADED));
    const skillsIdx = body.indexOf(AIKONOS_SKILLS_LOADED);
    const textStartIdx = body.indexOf("TEXT_MESSAGE_START");
    assert.ok(skillsIdx >= 0, "skills event must be present");
    assert.ok(textStartIdx >= 0, "an assistant text frame must be present (fake child sends one)");
    assert.ok(skillsIdx < textStartIdx, "AIKONOS_SKILLS_LOADED must be emitted before the assistant stream starts");

    // Extract the CUSTOM event's data line and parse its payload.
    const dataLine = body
      .split("\n")
      .find((line) => line.startsWith("data:") && line.includes(AIKONOS_SKILLS_LOADED));
    assert.ok(dataLine, "expected a data: line carrying the skills event");
    const evt = JSON.parse(dataLine!.slice("data:".length).trim());
    assert.equal(evt.name, AIKONOS_SKILLS_LOADED);
    assert.deepEqual(
      evt.value.skills.sort((a: { name: string }, b: { name: string }) => a.name.localeCompare(b.name)),
      [
        { name: "deployer", description: "Deploys the service", status: "loaded" },
        { name: "vault", description: "Handles secrets", status: "suppressed", reason: "disabled for automatic loading" },
      ].sort((a, b) => a.name.localeCompare(b.name)),
    );
  } finally {
    await teardown(rig);
  }
});

test("POST /agui: a non-matching message activates nothing and emits no AIKONOS_SKILLS_LOADED event", async () => {
  const rig = await makeRig();
  try {
    const body = await postAgui(rig.port, rig.token, {
      prompt: "what is the weather like today",
      threadId: "thread-2", runId: "run-2",
    });
    rig.destroyLastRequest();

    const prompt = rig.spawn.children[0].promptsReceived[0];
    assert.equal(prompt.activateSkillNames, undefined);
    assert.ok(!body.includes(AIKONOS_SKILLS_LOADED), "no skills event when nothing matched");
  } finally {
    await teardown(rig);
  }
});

test("POST /agui: /command (skillName) path is unaffected — no auto-matching, activateSkillName (singular) still set", async () => {
  const rig = await makeRig();
  try {
    const body = await postAgui(rig.port, rig.token, {
      prompt: "please deploy this and don't leak the secret",
      threadId: "thread-3", runId: "run-3",
      skillName: "vault", // disable_model_invocation bundles ARE reachable via explicit /command
    });
    rig.destroyLastRequest();

    const prompt = rig.spawn.children[0].promptsReceived[0];
    assert.equal(prompt.activateSkillName, "vault", "explicit /command activation must still use the singular field");
    assert.equal(prompt.activateSkillNames, undefined, "auto-matching must be skipped entirely when /command is used");
    assert.ok(!body.includes(AIKONOS_SKILLS_LOADED), "/command path must not also emit the auto-load timeline event");
  } finally {
    await teardown(rig);
  }
});

// ── CP5: auto-load must
// operate on the union of admin bundles + the caller's own personal skills ──

test("POST /agui: a personal skill matching a keyword auto-loads under its qualified 'personal:<name>' name", async () => {
  const rig = await makeRig({
    listPersonalSkillsSouth: async () => ({
      skills: [{
        name: "my-notes", description: "Keeps notes tidy", keywords: ["notes"],
        allowedTools: ["web.fetch"], disableModelInvocation: false, valid: true, warning: "",
      }],
    }),
  });
  try {
    const body = await postAgui(rig.port, rig.token, {
      prompt: "please check my notes",
      threadId: "thread-6", runId: "run-6",
    });
    rig.destroyLastRequest();

    const prompt = rig.spawn.children[0].promptsReceived[0];
    assert.deepEqual(prompt.activateSkillNames, ["personal:my-notes"]);

    assert.ok(body.includes(AIKONOS_SKILLS_LOADED));
    const dataLine = body
      .split("\n")
      .find((line) => line.startsWith("data:") && line.includes(AIKONOS_SKILLS_LOADED));
    assert.ok(dataLine, "expected a data: line carrying the skills event");
    const evt = JSON.parse(dataLine!.slice("data:".length).trim());
    assert.deepEqual(evt.value.skills, [
      { name: "personal:my-notes", description: "Keeps notes tidy", status: "loaded" },
    ]);
  } finally {
    await teardown(rig);
  }
});

test("POST /agui: a listPersonalSkillsSouth failure still auto-loads a matching admin bundle (fail-open)", async () => {
  const rig = await makeRig({
    listPersonalSkillsSouth: async () => {
      throw new Error("south unavailable");
    },
  });
  try {
    const body = await postAgui(rig.port, rig.token, {
      prompt: "please deploy this",
      threadId: "thread-7", runId: "run-7",
    });
    rig.destroyLastRequest();

    const prompt = rig.spawn.children[0].promptsReceived[0];
    assert.deepEqual(prompt.activateSkillNames, ["deployer"], "a broken personal-skill fetch must not block the admin-bundle auto-load path");
    assert.ok(body.includes(AIKONOS_SKILLS_LOADED));
  } finally {
    await teardown(rig);
  }
});
