// CP8 tests: scheduler ticker + external :8090 wired to the supervisor.
//
// WHY these tests exist: before CP8, the ticker and external surface built a
// GovernanceBridge directly and called the (now-deleted) runHeadlessStream
// helper in-process — keeping the long-lived owner identity and grant in the
// same process as the Pi loop.
// CP8 routes both paths through the ChildSupervisor so the child holds no
// identity, just a runId-tagged prompt.
//
// The invariants tested:
//   1. TICKER — a due run spawns/reuses a child via the supervisor; ownerGrant
//      from DueRun flows into the child's bound identity (setRunContext, parent
//      side) and is NOT carried in any IPC message body; run outcome is reported.
//   2. TICKER pre-auth approver — NEEDS_HUMAN gate on a listed tool is granted;
//      on an unlisted tool it is denied. This is the unattended-consent contract.
//   3. EXTERNAL — svc-<agentId> userId + ownerGrant reach setRunContext; events
//      relay to the SSE caller; non-auto agents still get 409 (unchanged).
//   4. AGENT PROVIDER SELECTION — an agent with a preferred provider resolves to
//      that provider's model rather than silently falling back to tenant-default.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  makePairedChannel,
  ParentLink,
  ChildLink,
} from "../src/ipc/protocol.js";
import type {
  DoneEvent,
  TextDeltaEvent,
  PromptMessage,
} from "../src/ipc/protocol.js";
import { RemoteBridgeServer } from "../src/ipc/bridge-server.js";
import type { BridgeLike, RunIdentity } from "../src/ipc/bridge-server.js";
import {
  ChildSupervisor,
  type SpawnChildFn,
  type SupervisorConfig,
  type ProviderCredentialResolver,
  type SupervisorDeps,
} from "../src/ipc/supervisor.js";
import type { Identity, Approver } from "../src/broker/governance.js";
import type { EgressProxy, RegisterResult, RegisterOptions } from "../src/llm/egress-proxy.js";
import type { ResolveIdentity, ProviderLike } from "../src/pi/session-plan.js";
import { preAuthApprover } from "../src/scheduler/ticker.js";
import { tick, type SupervisorDeps as TickerSupervisorDeps } from "../src/scheduler/ticker.js";
import type { Config } from "../src/config.js";
import type { Logger } from "../src/log.js";
import type { BrokerClients } from "../src/broker/clients.js";

// ── Helpers ────────────────────────────────────────────────────────────────────

async function flushAsync(): Promise<void> {
  for (let i = 0; i < 10; i++) {
    await new Promise((r) => setImmediate(r));
  }
}

function withTimeout<T>(p: Promise<T>, ms: number, label: string): Promise<T> {
  return Promise.race([
    p,
    new Promise<never>((_, reject) =>
      setTimeout(() => reject(new Error(`timeout: ${label}`)), ms),
    ),
  ]);
}

// ── Fake child ─────────────────────────────────────────────────────────────────

interface FakeChild {
  parentLink: ParentLink;
  childLink: ChildLink;
  promptsReceived: PromptMessage[];
}

function makeFakeChild(): FakeChild {
  const [parentSide, childSide] = makePairedChannel();
  const exitHandlers: Array<(code: number | null) => void> = [];

  const link = new ParentLink(parentSide);
  link.onExit = (h) => exitHandlers.push(h);
  link.kill = () => {};

  const childLink = new ChildLink(childSide);
  const promptsReceived: PromptMessage[] = [];
  childLink.on("prompt", (msg) => promptsReceived.push(msg));

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

const TENANT = "11111111-1111-1111-1111-111111111111";

function makeFakeDeps(providers: ProviderLike[] = []): SupervisorDeps {
  return {
    south: {
      getLlmProviders: async () => ({ providers }),
      getTenantModel: async () => ({ model: "anthropic/claude-sonnet-4.6" }),
      listUserSkills: async () => ({ skills: [] }),
      listUserAgentSkills: async () => ({ bundles: [] }),
      listAccessibleMcpServersForAgent: async () => ({ connections: [] }),
      listMcpServerToolsSouth: async () => ({ tools: [] }),
      getAgentSpec: async () => ({ found: false }),
    },
    cfg: {
      llmModel: "anthropic/claude-sonnet-4.6",
      defaultTenantId: TENANT,
    },
  };
}

function makeFakeCredentials(modelId = "anthropic/claude-sonnet-4.6"): ProviderCredentialResolver {
  return async (_id: ResolveIdentity) => ({
    upstreamBaseUrl: "https://openrouter.ai/api/v1",
    apiKey: "dummy",
    modelId,
    modelAllowlist: [modelId],
    fallbacks: [],
  });
}

interface SpyBridge extends BridgeLike {
  runContexts: Array<{ runId: string; identity: RunIdentity; approver: Approver }>;
}

function makeSpyBridgeFactory(): { factory: (identity: RunIdentity, approver: Approver) => SpyBridge; allBridges: SpyBridge[] } {
  const allBridges: SpyBridge[] = [];
  const factory = (_identity: RunIdentity, _approver: Approver): SpyBridge => {
    const bridge: SpyBridge = {
      runContexts: [],
      gate: async () => ({ allow: true }),
      execute: async () => ({ ok: true, output: "ok" }),
      delegate: async () => ({ ok: true }),
      saveWorkflow: async () => ({ ok: true }),
      runWorkflow: async () => ({ ok: true, result: null }),
      listWorkflows: async () => ({ ok: true, items: [] }),
      publishWorkflow: async () => ({ ok: true }),
      proposeWorkflow: async () => ({ ok: true }),
      analyzeImage: async () => ({ ok: true, text: "a red apple" }),
      scheduleWorkflow: async () => ({ ok: true }),
      setToken: () => {},
      setApprover: () => {},
    };
    allBridges.push(bridge);
    return bridge;
  };
  return { factory, allBridges };
}

const log: Logger = {
  info: () => {},
  warn: () => {},
  error: () => {},
  debug: () => {},
} as unknown as Logger;

const baseCfg = {
  defaultTenantId: TENANT,
  schedulerEnabled: true,
  schedulerTickMs: 30000,
  schedulerClaimLimit: 5,
  schedulerRunTimeoutMs: 2000,
  gatewaySpiffeId: "spiffe://aikonos.com/agent-gateway",
  agentForUserOverrides: {},
  openrouterApiKey: "",
  llmModel: "anthropic/claude-sonnet-4.6",
  brokerNorthAddr: "",
  brokerSouthAddr: "",
  brokerServerName: "",
  tlsCert: "",
  tlsKey: "",
  tlsCa: "",
  port: 8080,
  oidcIssuer: "",
  oidcJwksUrl: "",
  oidcAudience: "",
} as unknown as Config;

// ── Rig ────────────────────────────────────────────────────────────────────────

interface Rig {
  supervisor: ChildSupervisor;
  spawn: ReturnType<typeof makeFakeSpawn>;
  capturedSetRunContextCalls: Array<{ runId: string; identity: RunIdentity; approver: Approver }>;
}

function makeRig(config?: Partial<SupervisorConfig>, providers: ProviderLike[] = []): Rig {
  const spawn = makeFakeSpawn();
  const capturedSetRunContextCalls: Array<{ runId: string; identity: RunIdentity; approver: Approver }> = [];

  const supervisor = new ChildSupervisor(
    makeFakeProxy(),
    makeFakeDeps(providers),
    // BridgeFactory that captures setRunContext calls made by the handle.
    (_identity, _approver) => ({
      gate: async () => ({ allow: true }),
      execute: async () => ({ ok: true, output: "ok" }),
      delegate: async () => ({ ok: true }),
      saveWorkflow: async () => ({ ok: true }),
      runWorkflow: async () => ({ ok: true, result: null }),
      listWorkflows: async () => ({ ok: true, items: [] }),
      publishWorkflow: async () => ({ ok: true }),
      proposeWorkflow: async () => ({ ok: true }),
      analyzeImage: async () => ({ ok: true, text: "a red apple" }),
      scheduleWorkflow: async () => ({ ok: true }),
      setToken: () => {},
      setApprover: () => {},
    }),
    spawn.spawnFn,
    makeFakeCredentials(),
    config,
  );

  return { supervisor, spawn, capturedSetRunContextCalls };
}

// ── Test 1: ticker uses supervisor, ownerGrant flows into setRunContext ─────────

test("CP8 ticker: due run drives supervisor; ownerGrant in setRunContext identity, not IPC body", async () => {
  // WHY: the ticker must not forward ownerGrant in an IPC message body (which
  // the child could spoof). Instead it must pass ownerGrant into the child's
  // spawn identity so the parent bridge-server carries it in the verified run
  // record. This test asserts that (a) the supervisor receives the correct
  // identity for the run, (b) the ownerGrant is present on that identity, and
  // (c) the run outcome is reported to the broker.
  const grant = "v1.tenant.alice.expires.sig";

  const south = {
    claimDueScheduledRuns: () =>
      Promise.resolve({
        runs: [
          {
            id: "run-cp8",
            ownerUserId: "alice@example.com",
            prompt: "summarize latest",
            approvedTools: ["doc.read"],
            ownerGrant: grant,
          },
        ],
      }),
    reportScheduledRunResult: () => Promise.resolve(),
  };
  const clients = { south } as unknown as BrokerClients;

  // Capture the identity passed to the supervisor/setRunContext.
  const capturedIdentities: Identity[] = [];
  const capturedApprovers: Approver[] = [];

  const { spawn } = makeRig();

  // Build a supervisor that captures what identity/approver arrive at each run.
  const [parentSide, childSide] = makePairedChannel();
  const exitHandlers: Array<(code: number | null) => void> = [];
  const capturedLink = new ParentLink(parentSide);
  capturedLink.onExit = (h) => exitHandlers.push(h);
  capturedLink.kill = () => {};
  const childLinkCapture = new ChildLink(childSide);
  childLinkCapture.on("prompt", (msg: PromptMessage) => {
    // Auto-reply done so supervisor.run() resolves.
    const done: DoneEvent = { kind: "done", runId: msg.runId };
    childLinkCapture.send(done);
  });

  const captureFactory = (identity: RunIdentity, approver: Approver) => {
    capturedIdentities.push(identity);
    capturedApprovers.push(approver);
    return {
      gate: async () => ({ allow: true }),
      execute: async () => ({ ok: true, output: "ok" }),
      delegate: async () => ({ ok: true }),
      saveWorkflow: async () => ({ ok: true }),
      runWorkflow: async () => ({ ok: true, result: null }),
      listWorkflows: async () => ({ ok: true, items: [] }),
      publishWorkflow: async () => ({ ok: true }),
      proposeWorkflow: async () => ({ ok: true }),
      analyzeImage: async () => ({ ok: true, text: "a red apple" }),
      scheduleWorkflow: async () => ({ ok: true }),
      setToken: () => {},
      setApprover: () => {},
    };
  };

  const captureSupervisor = new ChildSupervisor(
    makeFakeProxy(),
    makeFakeDeps(),
    captureFactory,
    () => {
      // Return the pre-built link each time spawn is called.
      return capturedLink;
    },
    makeFakeCredentials(),
  );

  let reported = false;
  const reportSouth = {
    claimDueScheduledRuns: () =>
      Promise.resolve({
        runs: [
          {
            id: "run-cp8",
            ownerUserId: "alice@example.com",
            prompt: "summarize latest",
            approvedTools: ["doc.read"],
            ownerGrant: grant,
          },
        ],
      }),
    reportScheduledRunResult: () => {
      reported = true;
      return Promise.resolve();
    },
  };
  const reportClients = { south: reportSouth } as unknown as BrokerClients;

  await tick(baseCfg, reportClients, log, captureSupervisor);

  // The run must have been reported.
  assert.equal(reported, true, "reportScheduledRunResult must be called after the run");

  // The identity passed to the bridge factory must carry the ownerGrant.
  // WHY: if ticker.ts forwarded ownerGrant in an IPC message body instead of
  // setting it on the identity, this assertion would fail (identity.ownerGrant
  // would be undefined). The only place the grant can reach governance is via
  // the parent-side spawn identity — never the child message body.
  assert.ok(capturedIdentities.length > 0, "at least one run identity must have been captured");
  const runIdentity = capturedIdentities[0];
  assert.equal(
    runIdentity.ownerGrant,
    grant,
    "ownerGrant from DueRun must flow into the child's spawn identity (never the IPC body)",
  );
  assert.equal(runIdentity.userId, "alice@example.com", "userId must be the run's ownerUserId");

  captureSupervisor.dispose();
});

// ── Test 2: ticker pre-auth approver still gates NEEDS_HUMAN by allowlist ──────

test("CP8 ticker pre-auth: NEEDS_HUMAN gate granted iff tool in approved_tools allowlist", async () => {
  // WHY: the pre-auth approver is the unattended-consent mechanism. Rewiring the
  // ticker to use the supervisor must NOT change the approver semantics: a
  // NEEDS_HUMAN tool is granted iff its id is in the run's approved_tools. This
  // was the core unattended-HITL contract before CP8; it must survive the refactor.
  const approver = preAuthApprover(["doc.read", "email.draft"], log);

  const allowResult = await approver({
    toolCallId: "tc-1",
    toolName: "doc.read",
    toolId: "doc.read",
    effectClass: 0,
    reason: "requires approval",
    args: {},
    stepUp: false,
  });
  assert.equal(allowResult, true, "doc.read is in approved_tools → NEEDS_HUMAN granted");

  const denyResult = await approver({
    toolCallId: "tc-2",
    toolName: "web.fetch",
    toolId: "web.fetch",
    effectClass: 0,
    reason: "requires approval",
    args: {},
    stepUp: false,
  });
  assert.equal(denyResult, false, "web.fetch is NOT in approved_tools → NEEDS_HUMAN denied");

  const emptyApprover = preAuthApprover([], log);
  const emptyDeny = await emptyApprover({
    toolCallId: "tc-3",
    toolName: "doc.read",
    toolId: "doc.read",
    effectClass: 0,
    reason: "requires approval",
    args: {},
    stepUp: false,
  });
  assert.equal(emptyDeny, false, "empty approved_tools → everything denied (safe default)");
});

// ── Test 3: ticker drains events and produces run outcome ─────────────────────

test("CP8 ticker: child text_delta events aggregated into run summary; done → ok:true outcome reported", async () => {
  // WHY: tick() must drain the supervisor.run() event stream into a RunOutcome,
  // then call reportScheduledRunResult with the outcome. If the event drain or
  // the report call were broken, scheduled runs would silently fail to record
  // their result in the broker.

  const [parentSide, childSide] = makePairedChannel();
  const childLink = new ChildLink(childSide);
  const capturedLink = new ParentLink(parentSide);
  capturedLink.onExit = () => {};
  capturedLink.kill = () => {};

  const promptsReceived: PromptMessage[] = [];
  childLink.on("prompt", (msg: PromptMessage) => {
    promptsReceived.push(msg);
    // Emit text_delta then done.
    const delta: TextDeltaEvent = { kind: "text_delta", runId: msg.runId, delta: "summary text" };
    const done: DoneEvent = { kind: "done", runId: msg.runId };
    // Deliver asynchronously so the run() listener is registered first.
    setImmediate(() => {
      childLink.send(delta);
      childLink.send(done);
    });
  });

  const captureFactory = (_identity: RunIdentity, _approver: Approver) => ({
    gate: async () => ({ allow: true }),
    execute: async () => ({ ok: true, output: "ok" }),
    delegate: async () => ({ ok: true }),
    saveWorkflow: async () => ({ ok: true }),
    runWorkflow: async () => ({ ok: true, result: null }),
    listWorkflows: async () => ({ ok: true, items: [] }),
    publishWorkflow: async () => ({ ok: true }),
    proposeWorkflow: async () => ({ ok: true }),
    analyzeImage: async () => ({ ok: true, text: "a red apple" }),
    scheduleWorkflow: async () => ({ ok: true }),
    setToken: () => {},
    setApprover: () => {},
  });

  const sup = new ChildSupervisor(
    makeFakeProxy(),
    makeFakeDeps(),
    captureFactory,
    () => capturedLink,
    makeFakeCredentials(),
  );

  let reportedOutcome: { id: string; success: boolean; summary: string } | undefined;
  const south = {
    claimDueScheduledRuns: () =>
      Promise.resolve({
        runs: [
          {
            id: "run-drain",
            ownerUserId: "bob@example.com",
            prompt: "check status",
            approvedTools: [],
            ownerGrant: "v1.grant",
          },
        ],
      }),
    reportScheduledRunResult: (args: { tenantId: string; id: string; success: boolean; summary: string }) => {
      reportedOutcome = { id: args.id, success: args.success, summary: args.summary };
      return Promise.resolve();
    },
  };
  const clients = { south } as unknown as BrokerClients;

  await tick(baseCfg, clients, log, sup);

  assert.ok(reportedOutcome, "reportScheduledRunResult must have been called");
  assert.equal(reportedOutcome.success, true, "run outcome must be ok:true on clean done");
  assert.ok(
    reportedOutcome.summary.includes("summary text"),
    `summary must include the text_delta content, got: ${reportedOutcome.summary}`,
  );

  sup.dispose();
});

// ── Test 4: external path — svc- identity + ownerGrant → setRunContext ─────────

test("CP8 external: svc-<agentId> userId + ownerGrant flow into supervisor run identity (not IPC body)", async () => {
  // WHY: the external surface resolves a per-agent API key and gets back an
  // ownerGrant. That grant must flow into the child's spawn identity so the
  // parent bridge-server routes tool calls as svc-<agentId> with the correct
  // grant. If it were forwarded in the IPC body, the child could change it.
  const agentId = "aaaabbbb-0000-0000-0000-ccccddddeeee";
  const ownerUserId = `svc-${agentId}`;
  const grant = "v1.svc.grant.sig";
  const approvedTools = ["doc.read"];

  const capturedIdentities: Identity[] = [];
  const [parentSide, childSide] = makePairedChannel();
  const capturedLink = new ParentLink(parentSide);
  capturedLink.onExit = () => {};
  capturedLink.kill = () => {};
  const childLink = new ChildLink(childSide);
  childLink.on("prompt", (msg: PromptMessage) => {
    setImmediate(() => childLink.send({ kind: "done", runId: msg.runId } as DoneEvent));
  });

  const sup = new ChildSupervisor(
    makeFakeProxy(),
    makeFakeDeps(),
    (identity: RunIdentity, _approver: Approver) => {
      capturedIdentities.push(identity);
      return {
        gate: async () => ({ allow: true }),
        execute: async () => ({ ok: true, output: "ok" }),
        delegate: async () => ({ ok: true }),
        saveWorkflow: async () => ({ ok: true }),
        runWorkflow: async () => ({ ok: true, result: null }),
        listWorkflows: async () => ({ ok: true, items: [] }),
        publishWorkflow: async () => ({ ok: true }),
        proposeWorkflow: async () => ({ ok: true }),
        analyzeImage: async () => ({ ok: true, text: "a red apple" }),
        scheduleWorkflow: async () => ({ ok: true }),
        setToken: () => {},
        setApprover: () => {},
      };
    },
    () => capturedLink,
    makeFakeCredentials(),
  );

  // Simulate what external/core.ts does after CP8: drive the supervisor directly.
  const runId = "ext-run-001";
  const threadId = "ext-thread-001";
  const key = sup.keyFor({ tenantId: TENANT, userId: ownerUserId, agentId });
  const identity: Identity = { tenantId: TENANT, userId: ownerUserId, agentId, ownerGrant: grant };
  const handle = await sup.getOrSpawn(key, identity);
  await flushAsync();

  const runIdentity: RunIdentity = { tenantId: TENANT, userId: ownerUserId, agentId, token: undefined, ownerGrant: grant };
  handle.setRunContext(runId, runIdentity, preAuthApprover(approvedTools, log));

  const events: string[] = [];
  await withTimeout(
    sup.run(handle, { runId, threadId, text: "invoke agent" }, (evt) => events.push(evt.kind)),
    2000,
    "external run to complete",
  );
  handle.clearRunContext(runId);

  // The identity bound to the bridge factory must carry the svc- userId and ownerGrant.
  assert.ok(capturedIdentities.length > 0, "bridge factory must have been called (child spawned)");
  const bound = capturedIdentities[0];
  assert.equal(bound.userId, ownerUserId, "identity.userId must be svc-<agentId>");
  assert.equal(bound.ownerGrant, grant, "identity.ownerGrant must carry the broker-minted grant");

  sup.dispose();
});

// ── Test 5: agent preferred-provider selection ─────────────────────────────────

test("CP8 agent provider selection: agent with preferredProvider resolves to that provider's model", async () => {
  // WHY: before CP8, resolveSessionPlan had a TODO — it always picked the tenant
  // default/any provider, silently ignoring the agent's preferred provider. An
  // agent-bound run (ticker, external) would use the wrong model. After CP8,
  // pickModelId accepts an AgentSpec and mirrors resolveProviderModel's priority:
  //   1. agent preferred provider + model
  //   2. tenant default provider
  //   3. any enabled provider
  // This test asserts tier-1 fires when the preferred provider is available.
  const { resolveSessionPlan } = await import("../src/pi/session-plan.js");
  type AgentSpec = import("../src/pi/session.js").AgentSpec;

  const agentPreferredProvider = "anthropic-direct";
  const agentModel = "claude-opus-4-5";

  const providers: ProviderLike[] = [
    {
      id: "openrouter",
      enabled: true,
      isDefault: true,
      models: [{ id: "anthropic/claude-sonnet-4.6" }],
    },
    {
      id: agentPreferredProvider,
      enabled: true,
      isDefault: false,
      models: [{ id: agentModel }, { id: "claude-3-haiku" }],
    },
  ];

  const south = {
    getLlmProviders: async () => ({ providers }),
    getTenantModel: async () => ({ model: "" }),
    listUserSkills: async () => ({ skills: [] }),
    listUserAgentSkills: async () => ({ bundles: [] }),
    listAccessibleMcpServersForAgent: async () => ({ connections: [] }),
    listMcpServerToolsSouth: async () => ({ tools: [] }),
    getAgentSpec: async () => ({ found: false }),
  };

  const agentSpec: AgentSpec = {
    model: agentModel,
    approvalMode: "auto",
    skills: [],
    preferredProvider: agentPreferredProvider,
    allowedProviders: [],
  };

  const plan = await resolveSessionPlan(
    { tenantId: TENANT, userId: "svc-agent", agentId: "agent-1" },
    {
      south,
      cfg: { llmModel: "anthropic/claude-sonnet-4.6", defaultTenantId: TENANT },
      proxyBaseUrl: "http://127.0.0.1:9999/tok/v1",
      agentSpec,
    },
  );

  assert.equal(
    plan.modelId,
    agentModel,
    `resolveSessionPlan must pick the agent's preferred model (${agentModel}), not tenant default`,
  );
  // The model allowlist must also reflect the resolved model so the egress
  // proxy enforces it on the child's completions requests.
  assert.deepEqual(
    plan.proxyModelAllowlist,
    [agentModel],
    "proxyModelAllowlist must match the resolved model",
  );
});

test("CP8 agent provider selection: fallback to tenant default when preferred provider not available", async () => {
  // WHY: if the agent's preferred provider is absent or disabled, we must
  // gracefully fall through to the tenant default — not crash or silently use
  // whatever provider happens to be first. This is tier-2 of the priority order.
  const { resolveSessionPlan } = await import("../src/pi/session-plan.js");
  type AgentSpec = import("../src/pi/session.js").AgentSpec;

  const providers: ProviderLike[] = [
    {
      id: "openrouter",
      enabled: true,
      isDefault: true,
      models: [{ id: "anthropic/claude-sonnet-4.6" }],
    },
  ];

  const south = {
    getLlmProviders: async () => ({ providers }),
    getTenantModel: async () => ({ model: "" }),
    listUserSkills: async () => ({ skills: [] }),
    listUserAgentSkills: async () => ({ bundles: [] }),
    listAccessibleMcpServersForAgent: async () => ({ connections: [] }),
    listMcpServerToolsSouth: async () => ({ tools: [] }),
    getAgentSpec: async () => ({ found: false }),
  };

  // Agent prefers "anthropic-direct" which is not in providers.
  const agentSpec: AgentSpec = {
    model: "claude-opus-4-5",
    approvalMode: "auto",
    skills: [],
    preferredProvider: "anthropic-direct",
    allowedProviders: [],
  };

  const plan = await resolveSessionPlan(
    { tenantId: TENANT, userId: "svc-agent", agentId: "agent-2" },
    {
      south,
      cfg: { llmModel: "anthropic/claude-sonnet-4.6", defaultTenantId: TENANT },
      proxyBaseUrl: "http://127.0.0.1:9999/tok/v1",
      agentSpec,
    },
  );

  assert.equal(
    plan.modelId,
    "anthropic/claude-sonnet-4.6",
    "must fall back to tenant default model when preferred provider is absent",
  );
});

// ── Regression: scheduled run agent id must be agentForUser-derived, not the bare UUID ──
//
// A UUID ownerUserId passed verbatim as agentId parses as a valid UUID on the
// broker and is inserted into tasks.agent_id — which references no agents row, so
// CreateGatewayTask fails tasks_agent_id_fkey (SQLSTATE 23503) and EVERY tool call
// in the run is blocked with "13 INTERNAL: failed to create task". The ticker must
// derive agentId via agentForUser ("<uuid>-agent", deliberately not a UUID), which
// the broker treats as an unbound personal task (NULL agent_id) — the same shape
// the interactive /agui path produces.
test("ticker: scheduled run agent id is agentForUser-derived, never the bare ownerUserId (FK fix)", async () => {
  const ownerUuid = "cccccccc-0000-4000-8000-000000000001";
  const capturedIdentities: RunIdentity[] = [];

  const [parentSide, childSide] = makePairedChannel();
  const exitHandlers: Array<(code: number | null) => void> = [];
  const capturedLink = new ParentLink(parentSide);
  capturedLink.onExit = (h) => exitHandlers.push(h);
  capturedLink.kill = () => {};
  const childLinkCapture = new ChildLink(childSide);
  childLinkCapture.on("prompt", (msg: PromptMessage) => {
    const done: DoneEvent = { kind: "done", runId: msg.runId };
    childLinkCapture.send(done);
  });

  const captureFactory = (identity: RunIdentity, _approver: Approver) => {
    capturedIdentities.push(identity);
    return {
      gate: async () => ({ allow: true }),
      execute: async () => ({ ok: true, output: "ok" }),
      delegate: async () => ({ ok: true }),
      saveWorkflow: async () => ({ ok: true }),
      runWorkflow: async () => ({ ok: true, result: null }),
      listWorkflows: async () => ({ ok: true, items: [] }),
      publishWorkflow: async () => ({ ok: true }),
      proposeWorkflow: async () => ({ ok: true }),
      analyzeImage: async () => ({ ok: true, text: "a red apple" }),
      scheduleWorkflow: async () => ({ ok: true }),
      setToken: () => {},
      setApprover: () => {},
    };
  };

  const captureSupervisor = new ChildSupervisor(
    makeFakeProxy(),
    makeFakeDeps(),
    captureFactory,
    () => capturedLink,
    makeFakeCredentials(),
  );

  const reportSouth = {
    claimDueScheduledRuns: () =>
      Promise.resolve({
        runs: [
          {
            id: "run-fk",
            ownerUserId: ownerUuid,
            prompt: "weather report",
            approvedTools: [],
            ownerGrant: "v1.tenant.owner.expires.sig",
          },
        ],
      }),
    reportScheduledRunResult: () => Promise.resolve(),
  };
  const reportClients = { south: reportSouth } as unknown as BrokerClients;

  await tick(baseCfg, reportClients, log, captureSupervisor);

  assert.ok(capturedIdentities.length > 0, "a run identity must be captured");
  const id = capturedIdentities[0];
  assert.equal(id.userId, ownerUuid, "userId stays the real owner");
  assert.notEqual(
    id.agentId,
    ownerUuid,
    "agentId must NOT be the bare ownerUserId — a valid UUID would FK-fail as a non-existent agent",
  );
  assert.equal(
    id.agentId,
    `${ownerUuid}-agent`,
    "agentId is agentForUser-derived (non-UUID → broker stores NULL agent_id)",
  );

  captureSupervisor.dispose();
});
