// Soul-refresh tests — verify that a ChildSupervisor respawns an idle child
// when the agent's system prompt (persona / soul) has changed since spawn,
// using the same throttled idle-only recheck semantics as the tool-allowlist
// refresh (Approach A).
//
// WHY these tests exist: the system prompt is built once at spawn and frozen
// into the child. If a user edits the agent persona after the child exists,
// the edit silently no-ops until the child is evicted. These tests encode the
// contract that the re-resolve check catches BOTH tool-allowlist drift AND
// system-prompt drift.
import { test } from "node:test";
import assert from "node:assert/strict";
import { makePairedChannel, ParentLink, ChildLink } from "../src/ipc/protocol.js";
import type { BridgeLike } from "../src/ipc/bridge-server.js";
import {
  ChildSupervisor,
  type SpawnChildFn,
  type SpawnChildOptions,
  type SupervisorConfig,
  type ProviderCredentials,
  type ProviderCredentialResolver,
  type SupervisorDeps,
} from "../src/ipc/supervisor.js";
import type { Identity, Approver } from "../src/broker/governance.js";
import type { EgressProxy, RegisterResult } from "../src/llm/egress-proxy.js";
import type { ResolveIdentity } from "../src/pi/session-plan.js";
import type { AgentSpec } from "../src/pi/session.js";

// ── Fake primitives (mirrors supervisor.test.ts) ───────────────────────────────

interface FakeChild {
  parentLink: ParentLink;
  childLink: ChildLink;
  simulateExit(): void;
  sentMessages: unknown[];
}

function makeFakeChild(): FakeChild {
  const [parentSide, childSide] = makePairedChannel();
  const exitHandlers: Array<(code: number | null) => void> = [];
  const link = new ParentLink(parentSide);
  link.onExit = (handler: (code: number | null) => void) => { exitHandlers.push(handler); };
  link.offExit = (handler: (code: number | null) => void) => {
    const idx = exitHandlers.indexOf(handler);
    if (idx !== -1) exitHandlers.splice(idx, 1);
  };
  link.kill = (_delayMs: number) => {};

  const childLink = new ChildLink(childSide);
  const sentMessages: unknown[] = [];
  childLink.on("init", (msg) => sentMessages.push(msg));
  childLink.on("shutdown", (msg) => sentMessages.push(msg));
  childLink.on("prompt", (msg) => sentMessages.push(msg));

  return {
    parentLink: link,
    childLink,
    simulateExit() { for (const h of exitHandlers) h(null); },
    sentMessages,
  };
}

interface FakeSpawnRecord {
  opts: SpawnChildOptions;
  child: FakeChild;
}

function makeFakeSpawnFn(): { spawnFn: SpawnChildFn; records: FakeSpawnRecord[] } {
  const records: FakeSpawnRecord[] = [];
  const spawnFn: SpawnChildFn = (opts) => {
    const child = makeFakeChild();
    records.push({ opts, child });
    return child.parentLink;
  };
  return { spawnFn, records };
}

function makeFakeProxy(): EgressProxy {
  let tokenCounter = 0;
  return {
    register(_opts: { upstreamBaseUrl: string; realApiKey: string; modelAllowlist: string[] }): RegisterResult {
      return { childToken: `token-${++tokenCounter}`, childBaseUrl: `http://127.0.0.1:9999/token-${tokenCounter}` };
    },
    unregister(_token: string) {},
    resetRunBudget(_token: string) {},
    start() { return Promise.resolve(); },
    stop() { return Promise.resolve(); },
    address() { return { address: "127.0.0.1", port: 9999 }; },
  } as unknown as EgressProxy;
}

function makeFakeCredentials(): ProviderCredentialResolver {
  return async (_identity: ResolveIdentity): Promise<ProviderCredentials> => ({
    upstreamBaseUrl: "https://openrouter.ai/api/v1",
    apiKey: "FAKE_KEY",
    modelId: "anthropic/claude-sonnet-4.6",
    modelAllowlist: ["anthropic/claude-sonnet-4.6"],
    fallbacks: [],
  });
}

function makeBridgeFactory(): (identity: Identity, _approver: Approver) => BridgeLike {
  return (_identity: Identity, _approver: Approver): BridgeLike => ({
    gate: async () => ({ allow: true }),
    execute: async () => ({ ok: true, output: null }),
    delegate: async () => ({ ok: true }),
    saveWorkflow: async () => ({ ok: true }),
    runWorkflow: async () => ({ ok: true, result: null }),
    listWorkflows: async () => ({ ok: true, items: [] }),
    publishWorkflow: async () => ({ ok: true }),
    proposeWorkflow: async () => ({ ok: true }),
    analyzeImage: async () => ({ ok: true, text: "a red apple" }),
    scheduleWorkflow: async () => ({ ok: true }),
  });
}

function makeIdentity(): Identity {
  return {
    tenantId: "00000000-0000-0000-0000-000000000001",
    userId: "user-a",
    // Must start with "agent:" so resolveSessionPlan fetches the soul via getAgentSpec.
    agentId: "agent:agent-a",
  };
}

// Build a supervisor whose resolveSessionPlan returns a controllable systemPrompt
// alongside the skill-derived allowedToolNames.
function makeMutableSoulRig(initialPrompt: string, initialSkills: string[]): {
  supervisor: ChildSupervisor;
  records: FakeSpawnRecord[];
  setPrompt: (p: string) => void;
  setSkills: (s: string[]) => void;
} {
  const { spawnFn, records } = makeFakeSpawnFn();
  let currentPrompt = initialPrompt;
  let skills = initialSkills;

  // resolveSessionPlan uses getAgentSpec to build the system prompt when an
  // agentSpec is present, but in these tests we drive it through the south
  // getAgentSpec stub directly — the simplest seam without touching session-plan.ts.
  // We make getAgentSpec return a soul string that resolveSessionPlan embeds.
  // Because the fake deps produce a simple plan, we instead inject systemPrompt
  // by overriding what resolveSessionPlan would see via the agentSpec.soul field.
  //
  // Simpler approach that avoids coupling to session-plan internals: we replace
  // deps.south with a stub whose getAgentSpec returns the mutable soul — the
  // real resolveSessionPlan path will embed it. The existing Approach A tests
  // do the same thing for skills via listUserSkills.
  const south = {
    getLlmProviders: async () => ({ providers: [] }),
    getTenantModel: async () => ({ model: "anthropic/claude-sonnet-4.6" }),
    listUserSkills: async () => ({ skills }),
    listUserAgentSkills: async () => ({ bundles: [] }),
    listAccessibleMcpServersForAgent: async () => ({ connections: [] }),
    listMcpServerToolsSouth: async () => ({ tools: [] }),
    // resolveSessionPlan reads r.found and r.soul (top-level), not r.spec.soul.
    getAgentSpec: async () => ({
      found: true,
      soul: currentPrompt,
    }),
  };
  const cfg = {
    llmModel: "anthropic/claude-sonnet-4.6",
    defaultTenantId: "00000000-0000-0000-0000-000000000001",
  };
  const deps: SupervisorDeps = { south, cfg };

  const supervisor = new ChildSupervisor(
    makeFakeProxy(),
    deps,
    makeBridgeFactory(),
    spawnFn,
    makeFakeCredentials(),
    { keying: "per-user" } satisfies Partial<SupervisorConfig>,
  );

  return {
    supervisor,
    records,
    setPrompt: (p: string) => { currentPrompt = p; },
    setSkills: (s: string[]) => { skills = s; },
  };
}

// ── Tests ──────────────────────────────────────────────────────────────────────

test("soul-refresh: idle child IS respawned when systemPrompt changed (allowlist unchanged)", async () => {
  // WHY: a persona edit must take effect on the next idle reuse after the
  // throttle window — not require a full process restart.
  const { supervisor, records, setPrompt } = makeMutableSoulRig(
    "You are a helpful assistant.",
    ["web.fetch"],
  );
  const identity = makeIdentity();
  const key = supervisor.keyFor(identity);

  const h1 = await supervisor.getOrSpawn(key, identity);
  assert.equal(records.length, 1, "first call spawns once");

  // Edit the persona after the child exists.
  setPrompt("You are a terse expert who always responds in bullet points.");
  // Bypass the throttle window.
  h1.lastPlanCheckAt = 0;

  const h2 = await supervisor.getOrSpawn(key, identity);
  assert.equal(records.length, 2, "systemPrompt change must respawn the child");
  assert.notEqual(h2, h1, "fresh handle replaces the stale one");
});

test("soul-refresh: idle child is NOT respawned when systemPrompt and allowlist are unchanged", async () => {
  // WHY: spurious respawns reset in-flight conversation context. Only drift
  // should trigger respawn.
  const { supervisor, records } = makeMutableSoulRig(
    "You are a helpful assistant.",
    ["web.fetch"],
  );
  const identity = makeIdentity();
  const key = supervisor.keyFor(identity);

  const h1 = await supervisor.getOrSpawn(key, identity);
  // Force the throttle to expire; nothing changed.
  h1.lastPlanCheckAt = 0;

  const h2 = await supervisor.getOrSpawn(key, identity);
  assert.equal(records.length, 1, "unchanged plan must not respawn");
  assert.equal(h2, h1, "same handle reused");
});

test("soul-refresh: changed tool allowlist still triggers respawn (regression)", async () => {
  // WHY: existing Approach A behaviour must not regress. Both conditions (prompt
  // change OR allowlist change) independently trigger respawn.
  const { supervisor, records, setSkills } = makeMutableSoulRig(
    "You are a helpful assistant.",
    ["web.fetch"],
  );
  const identity = makeIdentity();
  const key = supervisor.keyFor(identity);

  const h1 = await supervisor.getOrSpawn(key, identity);
  setSkills(["web.fetch", "doc.write"]);
  h1.lastPlanCheckAt = 0;

  const h2 = await supervisor.getOrSpawn(key, identity);
  assert.equal(records.length, 2, "tool allowlist change must still respawn");
  assert.notEqual(h2, h1);
});

// ThrowingReresolveSupervisor overrides the protected resolveAllowedToolNames
// to simulate a failure that propagates to getOrSpawn's try/catch.
//
// WHY a subclass rather than a cast: resolveSessionPlan catches every south RPC
// error internally and never rejects, so there is no south-stub input that makes
// the re-resolve path throw. The subclass is the minimal typed seam that exercises
// the supervisor's own guard (fail-to-last-known-good) without any `as` cast,
// `any`, or `@ts-ignore`. `resolveAllowedToolNames` is `protected` specifically
// to allow this pattern in tests.
class ThrowingReresolveSupervisor extends ChildSupervisor {
  #calls = 0;

  protected override async resolveAllowedToolNames(
    _identity: Identity,
    _agentSpec?: AgentSpec,
  ): Promise<{ allowedToolNames: string[]; systemPrompt: string }> {
    this.#calls++;
    throw new Error(`simulated re-resolve failure (call ${this.#calls})`);
  }
}

test("soul-refresh: re-resolve that throws keeps the existing child (fail-to-last-known-good)", async () => {
  // WHY: a transient re-resolve failure must not evict a working child — that
  // would break an active user session worse than serving a stale persona.
  //
  // resolveSessionPlan catches every south RPC error internally and never rejects,
  // so there is no pure south-stub trigger for this path. ThrowingReresolveSupervisor
  // overrides the protected re-resolve method to inject the failure without any cast.
  const { spawnFn, records } = makeFakeSpawnFn();
  const south = {
    getLlmProviders: async () => ({ providers: [] }),
    getTenantModel: async () => ({ model: "anthropic/claude-sonnet-4.6" }),
    listUserSkills: async () => ({ skills: ["web.fetch"] }),
    listUserAgentSkills: async () => ({ bundles: [] }),
    listAccessibleMcpServersForAgent: async () => ({ connections: [] }),
    listMcpServerToolsSouth: async () => ({ tools: [] }),
    getAgentSpec: async () => ({ found: false }),
  };
  const cfg = {
    llmModel: "anthropic/claude-sonnet-4.6",
    defaultTenantId: "00000000-0000-0000-0000-000000000001",
  };
  const deps: SupervisorDeps = { south, cfg };

  const supervisor = new ThrowingReresolveSupervisor(
    makeFakeProxy(),
    deps,
    makeBridgeFactory(),
    spawnFn,
    makeFakeCredentials(),
    { keying: "per-user" } satisfies Partial<SupervisorConfig>,
  );

  const identity = makeIdentity();
  const key = supervisor.keyFor(identity);

  // First getOrSpawn spawns the child directly (no re-resolve, just spawn).
  const h1 = await supervisor.getOrSpawn(key, identity);
  assert.equal(records.length, 1);

  // Force the throttle to expire — next reuse will attempt re-resolve, which throws.
  h1.lastPlanCheckAt = 0;

  // Must NOT throw; must return the existing (stale-but-working) child.
  const h2 = await supervisor.getOrSpawn(key, identity);
  assert.equal(records.length, 1, "failed re-resolve must not respawn");
  assert.equal(h2, h1, "existing child kept on error");
});
