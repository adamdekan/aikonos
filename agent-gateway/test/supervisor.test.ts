// CP6 tests — ChildSupervisor lifecycle FSM.
//
// WHY these tests exist: the supervisor is the parent-side gatekeeper that
// ensures each child process is forked with a scrubbed env (no provider keys,
// no pepper, no bearer, no grant), that the right identity is bound to each
// child's bridge from the spawn record, and that the pool's lifecycle
// invariants (reuse, eviction, crash-recovery, circuit-breaker, cap) hold.
//
// All tests use a fake spawn function — no real child_process.fork is called.
// The fake returns a controllable ParentLink. The EgressProxy and credential
// resolver are also faked so the test has no external I/O.
import { test } from "node:test";
import assert from "node:assert/strict";
import { fork } from "node:child_process";
import { writeFileSync, mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { makePairedChannel, ParentLink, ChildLink } from "../src/ipc/protocol.js";
import type { IpcMessage, InitMessage } from "../src/ipc/protocol.js";
import type { BridgeLike } from "../src/ipc/bridge-server.js";
import {
  ChildSupervisor,
  GatewayOverloadError,
  defaultSupervisorConfig,
  ephemeralKey,
  branchKeyPrefix,
  scrubEnv,
  ProcessChannel,
  type ChildHandle,
  type SpawnChildFn,
  type SpawnChildOptions,
  type SupervisorConfig,
  type ProviderCredentials,
  type ProviderCredentialResolver,
  type ProviderTarget,
  type SupervisorDeps,
} from "../src/ipc/supervisor.js";
import type { Identity, Approver } from "../src/broker/governance.js";
import type { AgentSpec } from "../src/pi/session.js";
import type { EgressProxy, RegisterResult } from "../src/llm/egress-proxy.js";
import type { ResolveIdentity } from "../src/pi/session-plan.js";

// ── Fake primitives ────────────────────────────────────────────────────────────

// FakeChildProcess wraps a paired channel. The test holds the child side
// (childLink) to inspect messages the supervisor sends (init, shutdown) and
// to simulate an exit event.
interface FakeChild {
  parentLink: ParentLink;
  childLink: ChildLink;
  // Simulate the child exiting — triggers the supervisor's onChildExit path.
  simulateExit(): void;
  // Messages received on the parent side from the supervisor.
  sentMessages: unknown[];
}

function makeFakeChild(): FakeChild {
  const [parentSide, childSide] = makePairedChannel();

  // Wire exit subscription through the first-class onExit method — no duck-type.
  const exitHandlers: Array<(code: number | null) => void> = [];
  const link = new ParentLink(parentSide);

  // Override the first-class onExit/offExit methods so the supervisor can
  // register and deregister handlers without any _channel probe.
  link.onExit = (handler: (code: number | null) => void) => {
    exitHandlers.push(handler);
  };
  link.offExit = (handler: (code: number | null) => void) => {
    const idx = exitHandlers.indexOf(handler);
    if (idx !== -1) exitHandlers.splice(idx, 1);
  };
  // kill is a no-op in tests (no real process to terminate).
  link.kill = (_delayMs: number) => {};

  const childLink = new ChildLink(childSide);

  const sentMessages: unknown[] = [];
  // Intercept every message the supervisor sends to the child.
  childLink.on("init", (msg) => sentMessages.push(msg));
  childLink.on("shutdown", (msg) => sentMessages.push(msg));
  // Also capture prompt messages (for completeness).
  childLink.on("prompt", (msg) => sentMessages.push(msg));

  return {
    parentLink: link,
    childLink,
    simulateExit() {
      for (const h of exitHandlers) h(null);
    },
    sentMessages,
  };
}

// FakeSpawn records spawn calls and returns a pre-built FakeChild.
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

// Fake EgressProxy — enough surface for the supervisor.
interface CapturedRegistration {
  upstreamBaseUrl: string;
  realApiKey: string;
  modelAllowlist: string[];
  fallbacks?: ProviderTarget[];
}

function makeFakeProxy(): {
  proxy: EgressProxy;
  registered: Map<string, CapturedRegistration>;
  unregistered: string[];
  budgetResets: string[];
  budgetConsumes: string[];
} {
  let tokenCounter = 0;
  const registered = new Map<string, CapturedRegistration>();
  const unregistered: string[] = [];
  const budgetResets: string[] = [];
  const budgetConsumes: string[] = [];

  const proxy = {
    register(opts: CapturedRegistration): RegisterResult {
      const childToken = `fake-token-${++tokenCounter}`;
      registered.set(childToken, opts);
      return { childToken, childBaseUrl: `http://127.0.0.1:9999/${childToken}` };
    },
    resetRunBudget(token: string) { budgetResets.push(token); },
    consumeLlmBudget(token: string): boolean {
      budgetConsumes.push(token);
      return true;
    },
    unregister(token: string) {
      unregistered.push(token);
      registered.delete(token);
    },
    start() { return Promise.resolve(); },
    stop() { return Promise.resolve(); },
    address() { return { address: "127.0.0.1", port: 9999 }; },
  } as unknown as EgressProxy;

  return { proxy, registered, unregistered, budgetResets, budgetConsumes };
}

// Fake credential resolver.
function makeFakeCredentials(key = "REAL_API_KEY_NEVER_IN_CHILD"): ProviderCredentialResolver {
  return async (_identity: ResolveIdentity): Promise<ProviderCredentials> => ({
    upstreamBaseUrl: "https://openrouter.ai/api/v1",
    apiKey: key,
    modelId: "anthropic/claude-sonnet-4.6",
    modelAllowlist: ["anthropic/claude-sonnet-4.6"],
    fallbacks: [],
  });
}

// Fake south + cfg for resolveSessionPlan. emitLlmUsage is optional (spend-caps
// CP3) — omitted by default so every existing call site is unaffected; tests
// exercising usage forwarding pass a spy.
function makeFakeDeps(opts?: { emitLlmUsage?: SupervisorDeps["emitLlmUsage"] }): SupervisorDeps {
  const south = {
    getLlmProviders: async () => ({ providers: [] }),
    getTenantModel: async () => ({ model: "anthropic/claude-sonnet-4.6" }),
    listUserSkills: async () => ({ skills: [] }),
    listUserAgentSkills: async () => ({ bundles: [] }),
    listAccessibleMcpServersForAgent: async () => ({ connections: [] }),
    listMcpServerToolsSouth: async () => ({ tools: [] }),
    getAgentSpec: async () => ({ found: false }),
  };
  const cfg = {
    llmModel: "anthropic/claude-sonnet-4.6",
    defaultTenantId: "00000000-0000-0000-0000-000000000001",
  };
  return opts?.emitLlmUsage ? { south, cfg, emitLlmUsage: opts.emitLlmUsage } : { south, cfg };
}

// Fake bridge factory — returns a spy BridgeLike.
// The factory now accepts (identity, approver) per the BridgeFactory signature.
function makeBridgeFactory(): {
  factory: (identity: Identity, _approver: Approver, consumeLlmBudget?: () => boolean) => BridgeLike;
  calls: Identity[];
  budgetConsumers: Array<(() => boolean) | undefined>;
} {
  const calls: Identity[] = [];
  const budgetConsumers: Array<(() => boolean) | undefined> = [];
  const factory = (identity: Identity, _approver: Approver, consumeLlmBudget?: () => boolean): BridgeLike => {
    calls.push(identity);
    budgetConsumers.push(consumeLlmBudget);
    return {
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
    };
  };
  return { factory, calls, budgetConsumers };
}

// Helper to flush async (setImmediate-delivered IPC messages).
async function flushAsync(): Promise<void> {
  for (let i = 0; i < 5; i++) {
    await new Promise((r) => setImmediate(r));
  }
}

// ── Supervisor factory helpers ─────────────────────────────────────────────────

interface TestRig {
  supervisor: ChildSupervisor;
  spawnRecords: FakeSpawnRecord[];
  proxy: ReturnType<typeof makeFakeProxy>;
  bridgeCalls: Identity[];
  budgetConsumers: Array<(() => boolean) | undefined>;
}

function makeTestRig(configOverride?: Partial<SupervisorConfig>): TestRig {
  const { spawnFn, records } = makeFakeSpawnFn();
  const proxy = makeFakeProxy();
  const deps = makeFakeDeps();
  const { factory, calls, budgetConsumers } = makeBridgeFactory();
  const supervisor = new ChildSupervisor(
    proxy.proxy,
    deps,
    factory,
    spawnFn,
    makeFakeCredentials(),
    configOverride,
  );
  return {
    supervisor,
    spawnRecords: records,
    proxy,
    bridgeCalls: calls,
    budgetConsumers,
  };
}

function makeIdentity(userId = "user-a", agentId = "agent-a"): Identity {
  return {
    tenantId: "00000000-0000-0000-0000-000000000001",
    userId,
    agentId,
  };
}

// ── Tests ─────────────────────────────────────────────────────────────────────

test("CP6 spawn: getOrSpawn forks a new child and sends init on first call", async () => {
  // WHY: the first getOrSpawn for a key must fork exactly once and send the
  // secret-free init plan before returning the handle. Subsequent calls for
  // the same key must reuse without forking again.
  const rig = makeTestRig();
  const identity = makeIdentity();
  const key = rig.supervisor.keyFor(identity);

  const handle = await rig.supervisor.getOrSpawn(key, identity);
  await flushAsync();

  assert.equal(rig.spawnRecords.length, 1, "exactly one child must be forked");
  const rec = rig.spawnRecords[0];
  assert.ok(rec, "spawn record must exist");

  // Init must have been sent to the child.
  const init = rec.child.sentMessages.find((m) => (m as { kind: string }).kind === "init") as InitMessage | undefined;
  assert.ok(init, "supervisor must send init to the child");
  assert.equal(init.kind, "init");
  assert.ok(init.modelId, "init must carry a modelId");
  assert.ok(init.proxyBaseUrl.startsWith("http://127.0.0.1"), "proxyBaseUrl must be the proxy loopback URL");

  // Init must NOT contain any api key.
  const serialised = JSON.stringify(init);
  assert.ok(!serialised.includes("REAL_API_KEY"), "init plan must not contain the real api key");

  assert.ok(handle, "handle must be returned");
  assert.equal(handle.key, key);
});

test("CP6 reuse: getOrSpawn with the same key returns the SAME child handle", async () => {
  // WHY: forking a new process per request would defeat the point. Reuse is
  // the steady-state path and must work correctly.
  const rig = makeTestRig();
  const identity = makeIdentity();
  const key = rig.supervisor.keyFor(identity);

  const h1 = await rig.supervisor.getOrSpawn(key, identity);
  const h2 = await rig.supervisor.getOrSpawn(key, identity);

  assert.equal(rig.spawnRecords.length, 1, "second getOrSpawn must not fork a new child");
  assert.equal(h1, h2, "both calls must return the same handle object");
});

test("CP6 env-scrub: SECURITY — scrubbed env EQUALS the allowlist (fail-closed, not denylist)", async () => {
  // WHY this test is structured as an EQUALITY check, not a negative check:
  //
  // A denylist implementation would pass the usual "secret keys are absent"
  // assertions even while forwarding novel-named secrets (AIKONOS_VAULT_ROLE_ID,
  // WEIRD_FUTURE_SECRET, DATABASE_URL). This test proves the implementation is
  // allowlist-based by injecting several such "novel" vars and asserting that the
  // complete set of surviving keys EQUALS the expected allowlist — not merely that
  // known secrets were stripped. If any unlisted key survives, the test fails.
  //
  // Non-vacuity: a denylist scrubEnv would forward AIKONOS_VAULT_ROLE_ID and
  // WEIRD_FUTURE_SECRET (neither matches any denylist pattern), so the surviving
  // key set would differ from the allowlist and this test would fail.

  const injectedEnv: NodeJS.ProcessEnv = {
    // Known-secret vars (a denylist would strip these).
    OPENROUTER_API_KEY: "sk-or-real-key",
    AIKONOS_API_KEY_PEPPER: "pepper-secret",
    OIDC_BEARER_TOKEN: "bearer-abc",
    MY_PASSWORD: "hunter2",
    // Novel-named secret vars (a denylist would PASS these — the critical cases).
    AIKONOS_VAULT_ROLE_ID: "vault-role-id-secret",
    WEIRD_FUTURE_SECRET: "i-am-a-secret-but-not-in-any-denylist",
    DATABASE_URL: "postgres://user:pass@host/db",
    // Allowlisted vars that must survive.
    PATH: "/usr/bin:/bin",
    HOME: "/home/node",
    TMPDIR: "/tmp",
    TMP: "/tmp",
    TEMP: "/tmp",
    NODE_ENV: "test",
    AIKONOS_GATEWAY_THREAD_TTL_MS: "1800000",
    // A non-allowlisted AIKONOS_* var — must NOT survive.
    AIKONOS_BROKER_NORTH_ADDR: "127.0.0.1:9090",
    AIKONOS_LLM_MODEL: "anthropic/claude-sonnet-4.6",
  };

  const scrubbed = scrubEnv(injectedEnv);

  // The surviving key set must EQUAL the allowlist plus AIKONOS_CHILD_ENTRY.
  // Any key not in this set surviving = denylist, not allowlist.
  const expectedKeys = new Set([
    "PATH",
    "HOME",
    "TMPDIR",
    "TMP",
    "TEMP",
    "NODE_ENV",
    "AIKONOS_GATEWAY_THREAD_TTL_MS",
    "AIKONOS_CHILD_ENTRY",
  ]);

  const survivingKeys = new Set(Object.keys(scrubbed));

  // Assert no unexpected key survived (the critical allowlist property).
  for (const k of survivingKeys) {
    assert.ok(
      expectedKeys.has(k),
      `unexpected key survived scrubEnv: ${k} — this would leak a secret if it held one`,
    );
  }

  // Assert every expected allowlist key is present (if it was in the input).
  // Keys not present in the input are not required in the output.
  for (const k of expectedKeys) {
    if (k === "AIKONOS_CHILD_ENTRY") {
      assert.equal(scrubbed["AIKONOS_CHILD_ENTRY"], "1", "AIKONOS_CHILD_ENTRY must be injected");
      continue;
    }
    if (Object.prototype.hasOwnProperty.call(injectedEnv, k)) {
      assert.equal(scrubbed[k], injectedEnv[k], `allowlisted key ${k} must survive with its value`);
    }
  }

  // Explicitly verify the novel-named secrets were excluded (the non-vacuous cases).
  assert.ok(!("AIKONOS_VAULT_ROLE_ID" in scrubbed), "AIKONOS_VAULT_ROLE_ID must be excluded by the allowlist");
  assert.ok(!("WEIRD_FUTURE_SECRET" in scrubbed), "WEIRD_FUTURE_SECRET must be excluded by the allowlist");
  assert.ok(!("DATABASE_URL" in scrubbed), "DATABASE_URL must be excluded by the allowlist");
  assert.ok(!("AIKONOS_BROKER_NORTH_ADDR" in scrubbed), "non-allowlisted AIKONOS_* must be excluded");
});

test("CP6 env-scrub: spawned child receives scrubbed env with no provider key", async () => {
  // WHY: scrubEnv correctness proven above; this test proves the supervisor
  // actually calls spawnChild with the scrubbed env, not process.env directly.
  const { spawnFn, records } = makeFakeSpawnFn();
  const proxy = makeFakeProxy();
  const deps = makeFakeDeps();

  // Inject a fake key resolver that returns a recognisable real key string.
  const realKey = "sk-or-REAL-KEY-MUST-NOT-REACH-CHILD";
  const resolver: ProviderCredentialResolver = async () => ({
    upstreamBaseUrl: "https://openrouter.ai/api/v1",
    apiKey: realKey,
    modelId: "anthropic/claude-sonnet-4.6",
    modelAllowlist: ["anthropic/claude-sonnet-4.6"],
    fallbacks: [],
  });

  const { factory } = makeBridgeFactory();
  const supervisor = new ChildSupervisor(proxy.proxy, deps, factory, spawnFn, resolver);

  const identity = makeIdentity();
  const key = supervisor.keyFor(identity);
  await supervisor.getOrSpawn(key, identity);

  assert.equal(records.length, 1, "one spawn must have occurred");
  const rec = records[0];
  assert.ok(rec, "spawn record must exist");

  // The real api key must not appear anywhere in the spawned env.
  const envStr = JSON.stringify(rec.opts.env);
  assert.ok(!envStr.includes("REAL-KEY-MUST-NOT-REACH-CHILD"), "real api key must not appear in child env");
  assert.equal(rec.opts.env["AIKONOS_CHILD_ENTRY"], "1", "AIKONOS_CHILD_ENTRY must be set in child env");
});

test("CP6 proxy-gets-real-key: proxy.register receives the real api key, not the child", async () => {
  // WHY: the api key must go to the egress proxy (parent-side), not the child.
  // proxy.register is the only place the key must travel. Verifying this proves
  // the invariant at the boundary that matters.
  const { spawnFn } = makeFakeSpawnFn();
  const proxy = makeFakeProxy();
  const deps = makeFakeDeps();

  const realKey = "sk-or-PROXY-ONLY-KEY";
  const resolver: ProviderCredentialResolver = async () => ({
    upstreamBaseUrl: "https://openrouter.ai/api/v1",
    apiKey: realKey,
    modelId: "anthropic/claude-sonnet-4.6",
    modelAllowlist: ["anthropic/claude-sonnet-4.6"],
    fallbacks: [],
  });

  const { factory } = makeBridgeFactory();
  const supervisor = new ChildSupervisor(proxy.proxy, deps, factory, spawnFn, resolver);

  const identity = makeIdentity();
  const key = supervisor.keyFor(identity);
  await supervisor.getOrSpawn(key, identity);

  // The proxy must have seen the real key.
  const entries = [...proxy.registered.values()];
  assert.equal(entries.length, 1, "proxy.register must be called once");
  assert.equal(entries[0]?.realApiKey, realKey, "proxy must receive the real api key");
});

test("failover: the resolved provider chain's fallbacks are threaded into proxy.register", async () => {
  // WHY: resolveProviderCredentials returns the whole ordered chain, but the
  // proxy is the only component that can act on it. If the supervisor drops
  // `fallbacks` here, runtime failover silently never happens — the tenant's
  // configured fallback provider is resolved, keyed, and then thrown away.
  const { spawnFn } = makeFakeSpawnFn();
  const proxy = makeFakeProxy();
  const deps = makeFakeDeps();

  const fallbacks: ProviderTarget[] = [
    {
      upstreamBaseUrl: "https://fallback.example/v1",
      apiKey: "sk-fallback-only-in-parent",
      modelId: "fallback-model",
      api: "azure-openai",
      apiVersion: "2024-08-01-preview",
    },
  ];
  const resolver: ProviderCredentialResolver = async () => ({
    upstreamBaseUrl: "https://openrouter.ai/api/v1",
    apiKey: "sk-primary",
    modelId: "anthropic/claude-sonnet-4.6",
    // The union across the chain (slice 2) — the spawn coherence guard checks the
    // session plan's model against this, so both models must be present.
    modelAllowlist: ["anthropic/claude-sonnet-4.6", "fallback-model"],
    fallbacks,
  });

  const { factory } = makeBridgeFactory();
  const supervisor = new ChildSupervisor(proxy.proxy, deps, factory, spawnFn, resolver);

  const identity = makeIdentity();
  await supervisor.getOrSpawn(supervisor.keyFor(identity), identity);

  const entries = [...proxy.registered.values()];
  assert.equal(entries.length, 1, "proxy.register must be called once");
  assert.deepEqual(
    entries[0]?.fallbacks,
    fallbacks,
    "the whole fallback chain (including each target's own key, model and dialect) must reach the proxy",
  );
});

test("CP6 crash: crashed child is removed and next getOrSpawn respawns", async () => {
  // WHY: a child crash must not leave the pool in an unusable state. The
  // supervisor removes the key so the next request spawns a fresh child.
  // Failing this would cause every subsequent request for that user to error
  // without recourse.
  const rig = makeTestRig();
  const identity = makeIdentity();
  const key = rig.supervisor.keyFor(identity);

  const h1 = await rig.supervisor.getOrSpawn(key, identity);
  assert.equal(rig.spawnRecords.length, 1);

  // Simulate the child crashing.
  const rec = rig.spawnRecords[0];
  assert.ok(rec, "spawn record must exist");
  rec.child.simulateExit();
  await flushAsync();

  // Next getOrSpawn must spawn a new child.
  const h2 = await rig.supervisor.getOrSpawn(key, identity);
  assert.equal(rig.spawnRecords.length, 2, "a new child must be forked after crash");
  assert.notEqual(h1, h2, "respawned handle must be a different object");
});

test("CP6 crash: proxy.unregister called when child crashes", async () => {
  // WHY: if proxy.unregister is not called on crash, the real api key remains
  // bound to a dead child token — a small key-lifetime leak. Cleanup must be
  // deterministic.
  const rig = makeTestRig();
  const identity = makeIdentity();
  const key = rig.supervisor.keyFor(identity);

  await rig.supervisor.getOrSpawn(key, identity);
  const proxyToken = rig.spawnRecords[0]?.child ? [...rig.proxy.registered.keys()][0] : undefined;
  assert.ok(proxyToken, "proxy token must be registered");

  rig.spawnRecords[0]?.child.simulateExit();
  await flushAsync();

  assert.ok(rig.proxy.unregistered.includes(proxyToken), "proxy.unregister must be called on crash");
});

test("CP6 circuit-breaker: trips after N rapid crashes and blocks respawn", async () => {
  // WHY: a poisoned prompt that immediately crashes the child could fork-bomb
  // the host. The circuit-breaker limits to N crashes in M seconds before
  // refusing to respawn for a cooling-off period.
  const rig = makeTestRig({ cbMaxCrashes: 3, cbWindowMs: 10_000, cbBlockMs: 60_000 });
  const identity = makeIdentity();
  const key = rig.supervisor.keyFor(identity);

  // First spawn.
  await rig.supervisor.getOrSpawn(key, identity);

  // Crash 1 — breaker has not tripped yet, respawn OK.
  rig.spawnRecords[0]?.child.simulateExit();
  await flushAsync();
  await rig.supervisor.getOrSpawn(key, identity);

  // Crash 2 — still OK.
  rig.spawnRecords[1]?.child.simulateExit();
  await flushAsync();
  await rig.supervisor.getOrSpawn(key, identity);

  // Crash 3 — trips the breaker (cbMaxCrashes = 3 within cbWindowMs).
  rig.spawnRecords[2]?.child.simulateExit();
  await flushAsync();

  // Next getOrSpawn must throw (breaker open).
  await assert.rejects(
    () => rig.supervisor.getOrSpawn(key, identity),
    (err: unknown) => {
      assert.ok(err instanceof Error, "must throw an Error");
      assert.ok(err.message.includes("circuit-breaker"), `message must mention circuit-breaker, got: ${err.message}`);
      return true;
    },
    "getOrSpawn must throw when circuit-breaker is open",
  );
});

test("CP6 idle-eviction: idle child past TTL is evicted; active child is retained", async () => {
  // WHY: idle children hold a bound bridge (which holds a bearer reference).
  // TTL-eviction bounds the live credential window and keeps memory bounded.
  // Active children must never be evicted mid-run.
  const rig = makeTestRig({ childTtlMs: 1000 });
  const identity = makeIdentity();
  const key = rig.supervisor.keyFor(identity);

  const handle = await rig.supervisor.getOrSpawn(key, identity);

  // markIdle first (it stamps lastUsedAt), then backdate so the TTL check fires.
  rig.supervisor.markIdle(handle);
  handle.lastUsedAt = Date.now() - 2000;

  // Trigger the reaper manually (private method exposed via reflection).
  (rig.supervisor as unknown as { reapIdle(): void }).reapIdle();
  await flushAsync();

  // The child must have been evicted (proxy.unregister called).
  assert.ok(rig.proxy.unregistered.length > 0, "proxy.unregister must be called when idle child is evicted");

  // Next getOrSpawn must spawn a new child (the old one is gone).
  const h2 = await rig.supervisor.getOrSpawn(key, identity);
  assert.equal(rig.spawnRecords.length, 2, "a new child must be forked after eviction");
  assert.notEqual(handle, h2, "evicted handle must be replaced");
});

test("CP6 idle-eviction: busy child is NOT evicted past TTL", async () => {
  // WHY: evicting a busy child mid-run would orphan an in-flight LLM turn —
  // the tool calls would have no bridge to reply to, leaving the user in a
  // broken state.
  const rig = makeTestRig({ childTtlMs: 1000 });
  const identity = makeIdentity();
  const key = rig.supervisor.keyFor(identity);

  const handle = await rig.supervisor.getOrSpawn(key, identity);

  // Backdate and mark BUSY.
  handle.lastUsedAt = Date.now() - 2000;
  rig.supervisor.markBusy(handle);

  (rig.supervisor as unknown as { reapIdle(): void }).reapIdle();
  await flushAsync();

  assert.equal(rig.proxy.unregistered.length, 0, "busy child must not be evicted");
});

test("CP6 keying-single: different identities share the same child under 'single' keying", async () => {
  // WHY: Phase 2 uses a single shared child. The security win in Phase 2 is the
  // process split (no key in child env); per-user isolation comes in Phase 3.
  // This test proves the Phase-2 default is wired correctly.
  const rig = makeTestRig({ keying: "single" });

  const iA = makeIdentity("alice", "agent-a");
  const iB = makeIdentity("bob", "agent-b");

  const keyA = rig.supervisor.keyFor(iA);
  const keyB = rig.supervisor.keyFor(iB);

  assert.equal(keyA, keyB, "single keying must produce the same key for different identities");

  const hA = await rig.supervisor.getOrSpawn(keyA, iA);
  const hB = await rig.supervisor.getOrSpawn(keyB, iB);

  assert.equal(rig.spawnRecords.length, 1, "single keying must not fork a second child");
  assert.equal(hA, hB, "single keying must return the same handle");
});

test("CP6 keying-per-user: different identities get distinct children under 'per-user' keying", async () => {
  // WHY: Phase 3 requires per-user isolation — each (userId, agentId) pair must
  // have its own child so a compromised child cannot read another user's data.
  // This test proves the Phase-3 seam works now even though Phase 2 doesn't
  // enable it by default — we don't want to discover the seam is broken when
  // we flip the flag in Phase 3.
  const rig = makeTestRig({ keying: "per-user" });

  const iA = makeIdentity("alice", "agent-a");
  const iB = makeIdentity("bob", "agent-b");

  const keyA = rig.supervisor.keyFor(iA);
  const keyB = rig.supervisor.keyFor(iB);

  assert.notEqual(keyA, keyB, "per-user keying must produce distinct keys for different identities");

  const hA = await rig.supervisor.getOrSpawn(keyA, iA);
  const hB = await rig.supervisor.getOrSpawn(keyB, iB);

  assert.equal(rig.spawnRecords.length, 2, "per-user keying must fork a distinct child for each identity");
  assert.notEqual(hA, hB, "per-user keying must return distinct handles");
});

test("CP6 keying-per-user: key is collision-safe for ambiguous userId/agentId splits", () => {
  // WHY: if the key is built with a "/" separator, (userId="a/b", agentId="c")
  // produces the same key as (userId="a", agentId="b/c"). UUIDs use only hex
  // digits and hyphens; a space separator cannot appear in any UUID field, so
  // the three-field join is collision-safe.
  //
  // Non-vacuous: with a "/" separator both identities below would produce "a/b/c"
  // and the keys would be equal — this test would fail, exposing the bug.
  const rig = makeTestRig({ keying: "per-user" });

  const tenantId = "00000000-0000-0000-0000-000000000001";

  // Ambiguous split: userId="a/b", agentId="c" vs userId="a", agentId="b/c"
  const iAmbig1: Identity = { tenantId, userId: "a/b", agentId: "c" };
  const iAmbig2: Identity = { tenantId, userId: "a", agentId: "b/c" };

  const key1 = rig.supervisor.keyFor(iAmbig1);
  const key2 = rig.supervisor.keyFor(iAmbig2);

  assert.notEqual(key1, key2, "keyFor must not collide for (userId='a/b',agentId='c') vs (userId='a',agentId='b/c')");

  // Also verify two genuinely different UUIDs produce different keys.
  const iReal1: Identity = {
    tenantId,
    userId: "00000000-0000-0000-0000-000000000002",
    agentId: "00000000-0000-0000-0000-000000000003",
  };
  const iReal2: Identity = {
    tenantId,
    userId: "00000000-0000-0000-0000-000000000002",
    agentId: "00000000-0000-0000-0000-000000000004",
  };
  assert.notEqual(rig.supervisor.keyFor(iReal1), rig.supervisor.keyFor(iReal2),
    "different agentIds must produce different keys");
});

test("CP6 cap-eviction: when cap is exceeded and idle child exists, LRU idle child is evicted", async () => {
  // WHY: without a cap, a flood of unique users (per-user keying) could fork
  // enough children to exhaust container memory. The cap + LRU-eviction keeps
  // the pool bounded.
  const rig = makeTestRig({ keying: "per-user", maxChildren: 2 });

  const iA = makeIdentity("alice", "agent-a");
  const iB = makeIdentity("bob", "agent-b");
  const iC = makeIdentity("carol", "agent-c");

  const keyA = rig.supervisor.keyFor(iA);
  const keyB = rig.supervisor.keyFor(iB);
  const keyC = rig.supervisor.keyFor(iC);

  const hA = await rig.supervisor.getOrSpawn(keyA, iA);
  const hB = await rig.supervisor.getOrSpawn(keyB, iB);

  // Make A the LRU idle child.
  hA.lastUsedAt = Date.now() - 5000;
  rig.supervisor.markIdle(hA);
  hB.lastUsedAt = Date.now() - 1000;
  rig.supervisor.markIdle(hB);

  // This must evict A (LRU idle) to make room for C.
  await rig.supervisor.getOrSpawn(keyC, iC);

  assert.equal(rig.spawnRecords.length, 3, "three total children must have been spawned");
  assert.ok(rig.proxy.unregistered.length > 0, "LRU child must be unregistered from the proxy");
});

test("P3 cross-user isolation: per-user keying gives each identity a distinct handle and bridge; single keying shares them", async () => {
  // WHY this test exists: Phase 3 makes per-user keying the production default.
  // This test proves the cross-user boundary at the supervisor level — two callers
  // with different userIds get distinct ChildHandles (distinct link + bridgeServer),
  // so run-context set by one is invisible to the other.
  //
  // Non-vacuity: under "single" keying both identities share one handle and the
  // isolation assertion fails — proving the test is not trivially satisfied.

  // ── Part 1: per-user keying gives distinct handles ──────────────────────────

  const rigPerUser = makeTestRig({ keying: "per-user" });

  const idA: Identity = { tenantId: "00000000-0000-0000-0000-000000000001", userId: "user-alice", agentId: "agent-x" };
  const idB: Identity = { tenantId: "00000000-0000-0000-0000-000000000001", userId: "user-bob",   agentId: "agent-x" };

  const keyA = rigPerUser.supervisor.keyFor(idA);
  const keyB = rigPerUser.supervisor.keyFor(idB);

  const hA = await rigPerUser.supervisor.getOrSpawn(keyA, idA);
  const hB = await rigPerUser.supervisor.getOrSpawn(keyB, idB);

  // Distinct keys.
  assert.notEqual(keyA, keyB, "per-user: different userIds must produce different keys");

  // Distinct handles — different object references.
  assert.notEqual(hA, hB, "per-user: different userIds must yield distinct ChildHandle objects");

  // Distinct underlying links — the cross-user boundary at IPC level.
  assert.notEqual(hA.link, hB.link, "per-user: handles must have distinct ParentLink instances");

  // Distinct bridge servers — run-context set on A is not visible on B.
  assert.notEqual(hA.bridgeServer, hB.bridgeServer, "per-user: handles must have distinct RemoteBridgeServer instances");

  // Set a run context on A's bridge; B's bridge must be a different instance
  // with no knowledge of A's run.
  hA.setRunContext("run-a", idA, async () => true);
  // B's bridgeServer is a different object — no shared state.
  assert.notEqual(hA.bridgeServer, hB.bridgeServer,
    "per-user: run context registered on A's bridgeServer must not bleed into B's bridgeServer");

  // Exactly two children spawned.
  assert.equal(rigPerUser.spawnRecords.length, 2, "per-user: exactly one child per distinct userId");

  rigPerUser.supervisor.dispose();

  // ── Part 2: non-vacuity — single keying shares one handle (isolation fails) ─

  const rigSingle = makeTestRig({ keying: "single" });

  const keyAsin = rigSingle.supervisor.keyFor(idA);
  const keyBsin = rigSingle.supervisor.keyFor(idB);

  const hAsin = await rigSingle.supervisor.getOrSpawn(keyAsin, idA);
  const hBsin = await rigSingle.supervisor.getOrSpawn(keyBsin, idB);

  // Under single keying both keys and handles must be the same — this is the
  // "isolation fails" case that proves the per-user assertion above is non-trivial.
  assert.equal(keyAsin, keyBsin, "single: different userIds must map to the same key");
  assert.equal(hAsin, hBsin, "single: different userIds must share the same handle");
  assert.equal(hAsin.link, hBsin.link, "single: same handle means same link — no cross-user boundary");
  assert.equal(hAsin.bridgeServer, hBsin.bridgeServer, "single: same handle means same bridgeServer");

  rigSingle.supervisor.dispose();
});

test("CP6 cap-overload: all children busy over cap → GatewayOverloadError", async () => {
  // WHY: when every child is busy and we are at the cap, we must not silently
  // queue (unbounded queue = OOM) or spawn anyway (uncapped pool). A typed error
  // lets the HTTP layer map to 503 deterministically.
  const rig = makeTestRig({ keying: "per-user", maxChildren: 2 });

  const iA = makeIdentity("alice", "agent-a");
  const iB = makeIdentity("bob", "agent-b");
  const iC = makeIdentity("carol", "agent-c");

  const keyA = rig.supervisor.keyFor(iA);
  const keyB = rig.supervisor.keyFor(iB);
  const keyC = rig.supervisor.keyFor(iC);

  const hA = await rig.supervisor.getOrSpawn(keyA, iA);
  const hB = await rig.supervisor.getOrSpawn(keyB, iB);

  // Mark both busy — no LRU candidate.
  rig.supervisor.markBusy(hA);
  rig.supervisor.markBusy(hB);

  await assert.rejects(
    () => rig.supervisor.getOrSpawn(keyC, iC),
    (err: unknown) => {
      assert.ok(err instanceof GatewayOverloadError, `must throw GatewayOverloadError, got: ${String(err)}`);
      return true;
    },
    "getOrSpawn must throw GatewayOverloadError when all children are busy and cap is reached",
  );
});

test("CP6 init-no-key: init plan sent to child contains no api key or credentials", async () => {
  // WHY: the init plan is the only structured data sent from the parent to the
  // child at spawn time (aside from env). If an api key or bearer slips into
  // the plan, the child's process.env doesn't need to have it — the child can
  // extract it from the plan. This test closes that gap.
  const rig = makeTestRig();
  const identity = makeIdentity();
  const key = rig.supervisor.keyFor(identity);

  await rig.supervisor.getOrSpawn(key, identity);
  await flushAsync();

  const rec = rig.spawnRecords[0];
  assert.ok(rec, "spawn record must exist");

  const init = rec.child.sentMessages.find(
    (m) => (m as { kind: string }).kind === "init",
  ) as InitMessage | undefined;

  assert.ok(init, "init must have been sent");

  const planStr = JSON.stringify(init);
  // The fake credential resolver returns "REAL_API_KEY_NEVER_IN_CHILD".
  assert.ok(!planStr.includes("REAL_API_KEY_NEVER_IN_CHILD"), "real api key must not appear in the init plan");
  assert.ok(!planStr.includes("NEVER_IN_CHILD"), "no secret fragment must appear in the init plan");

  // The proxyBaseUrl must be present (it's the child's provider endpoint).
  assert.ok(init.proxyBaseUrl.includes("127.0.0.1"), "proxyBaseUrl must be the loopback proxy");
  assert.ok(init.modelId, "init must carry a modelId");
});

test("CP6 exit-listener-cleanup: exit handler count returns to baseline after each run settles", async () => {
  // WHY: a long-lived shared child (single-keying, Phase 2 default) serves many
  // sequential runs. Before the offExit fix each run left one dead exit handler
  // on the link — after 10 concurrent runs Node emitted MaxListenersExceededWarning
  // and the dead handlers persisted for the child's lifetime. This test proves
  // the count returns to baseline after each run settles.
  //
  // Non-vacuous: removing the offExit(exitHandler) call from settle() would cause
  // exitHandlers to grow by 1 per run and this assertion would fail after the
  // first run completes.
  const rig = makeTestRig({ keying: "single" });
  const identity = makeIdentity();
  const key = rig.supervisor.keyFor(identity);
  const handle = await rig.supervisor.getOrSpawn(key, identity);

  // Access the FakeChild so we can inspect its exitHandlers array and drive
  // done events.
  const rec = rig.spawnRecords[0];
  assert.ok(rec, "spawn record must exist");

  // The supervisor wires its own onChildExit handler at spawn time — that is the
  // baseline (1 handler). Record it now so we can assert equality after each run.
  const baseline = (handle.link as unknown as { _exitHandlers?: unknown }).toString();
  // We cannot reach _exitHandlers directly (it's local to makeFakeChild), so we
  // measure indirectly: simulateExit fires all handlers. After each run the count
  // must equal the count right after spawn (baseline = 1: the supervisor's own
  // onChildExit handler). We count by replacing onExit with an interceptor.

  // Re-capture the exitHandlers reference via the closure in makeFakeChild.
  // We do this by registering a sentinel handler and immediately removing it to
  // get the current length from the side-channel the fake provides.
  //
  // Simpler approach: drive N runs to completion and assert that simulateExit
  // fires the child-exit path exactly once per call regardless of prior run count.
  // The true regression is that dead handlers accumulated would keep reject()ing
  // settled promises — but since settle() is idempotent (settled=true guard) we
  // cannot observe duplicate rejects. Instead we observe the handler count
  // directly: override onExit to count registrations vs offExit to count removals.

  let registered = 0;
  let deregistered = 0;
  const originalOnExit = handle.link.onExit.bind(handle.link);
  const originalOffExit = handle.link.offExit.bind(handle.link);
  handle.link.onExit = (h: (code: number | null) => void) => {
    registered++;
    originalOnExit(h);
  };
  handle.link.offExit = (h: (code: number | null) => void) => {
    deregistered++;
    originalOffExit(h);
  };

  // Run N sequential runs to completion.
  const N = 5;
  for (let i = 0; i < N; i++) {
    const runId = `run-cleanup-${i}`;
    const runPromise = rig.supervisor.run(
      handle,
      { runId, threadId: "th-cleanup", text: `msg ${i}` },
      () => {},
    );
    await flushAsync();
    // Drive the run to completion with a "done" event.
    rec.child.childLink.send({ kind: "done", runId });
    await flushAsync();
    await runPromise;
  }

  // Each run registered one exit handler and must have removed it on settle.
  assert.equal(registered, N, `onExit must be called once per run (got ${registered})`);
  assert.equal(deregistered, N, `offExit must be called once per run on settle (got ${deregistered}) — without offExit dead handlers accumulate`);
  assert.equal(registered - deregistered, 0, "net exit handler accumulation must be zero after all runs settle");
});

test("CP6 bridge-bound: bridge is constructed from the caller-provided identity, not from any child message", async () => {
  // WHY: this is the load-bearing security property — the identity that the
  // bridge acts as is fixed at setRunContext time from the caller's verified record.
  // Nothing the child sends in an IPC message can change which identity the bridge
  // sees; the factory receives exactly what the server passes to setRunContext.
  const { spawnFn } = makeFakeSpawnFn();
  const proxy = makeFakeProxy();
  const deps = makeFakeDeps();
  const identitiesSeenByBridge: Identity[] = [];
  const factory = (identity: Identity, _approver: Approver): BridgeLike => {
    identitiesSeenByBridge.push(identity);
    return {
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
    };
  };

  const supervisor = new ChildSupervisor(proxy.proxy, deps, factory, spawnFn, makeFakeCredentials());
  const identity = makeIdentity("alice", "agent-a");
  const key = supervisor.keyFor(identity);

  const handle = await supervisor.getOrSpawn(key, identity);

  // Factory is called per setRunContext, not per spawn — register run context now.
  handle.setRunContext("run-bridge-test", identity, async () => true);

  assert.equal(identitiesSeenByBridge.length, 1, "bridge factory must be called once per setRunContext");
  const bound = identitiesSeenByBridge[0];
  assert.ok(bound, "bound identity must be recorded");
  assert.equal(bound.userId, "alice", "bound identity must match the caller-provided userId");
  assert.equal(bound.agentId, "agent-a", "bound identity must match the caller-provided agentId");
});

// ── AIKONOS_GATEWAY_CHILD_KEYING enum guard ────────────────────────────────────

// defaultSupervisorConfig reads process.env directly, so we swap the var,
// call it, then restore — no module reload needed.
function withKeyingEnv(value: string | undefined, fn: () => void): void {
  const prev = process.env["AIKONOS_GATEWAY_CHILD_KEYING"];
  if (value === undefined) {
    delete process.env["AIKONOS_GATEWAY_CHILD_KEYING"];
  } else {
    process.env["AIKONOS_GATEWAY_CHILD_KEYING"] = value;
  }
  try {
    fn();
  } finally {
    if (prev === undefined) {
      delete process.env["AIKONOS_GATEWAY_CHILD_KEYING"];
    } else {
      process.env["AIKONOS_GATEWAY_CHILD_KEYING"] = prev;
    }
  }
}

// F28: default flipped single→per-user (old assertion pinned "single"; the
// spec's default-pinning authorization covers exactly this rewrite).
test('supervisor: AIKONOS_GATEWAY_CHILD_KEYING absent defaults to "per-user"', () => {
  withKeyingEnv(undefined, () => {
    const cfg = defaultSupervisorConfig();
    assert.equal(cfg.keying, "per-user");
  });
});

test('supervisor: AIKONOS_GATEWAY_CHILD_KEYING="single" is accepted', () => {
  withKeyingEnv("single", () => {
    const cfg = defaultSupervisorConfig();
    assert.equal(cfg.keying, "single");
  });
});

test('supervisor: AIKONOS_GATEWAY_CHILD_KEYING="per-user" is accepted', () => {
  withKeyingEnv("per-user", () => {
    const cfg = defaultSupervisorConfig();
    assert.equal(cfg.keying, "per-user");
  });
});

// ── F26: maxChildren/childTtlMs no longer read from process.env here ─────────
//
// These values now flow from the validated Config (config.ts:buildConfig) via
// the Partial<SupervisorConfig> override the ChildSupervisor constructor
// already merges over defaultSupervisorConfig()'s return. Before F26,
// defaultSupervisorConfig read AIKONOS_GATEWAY_MAX_CHILDREN/CHILD_TTL_MS
// directly via `Number(...)`, so setting the env var here would have changed
// its output — these tests fail on the pre-F26 implementation.
function withEnv(name: string, value: string | undefined, fn: () => void): void {
  const prev = process.env[name];
  if (value === undefined) delete process.env[name];
  else process.env[name] = value;
  try {
    fn();
  } finally {
    if (prev === undefined) delete process.env[name];
    else process.env[name] = prev;
  }
}

test("defaultSupervisorConfig: AIKONOS_GATEWAY_MAX_CHILDREN env no longer affects maxChildren (F26)", () => {
  withEnv("AIKONOS_GATEWAY_MAX_CHILDREN", "999", () => {
    const cfg = defaultSupervisorConfig();
    assert.equal(cfg.maxChildren, 32, "maxChildren must come from injected Config, not process.env");
  });
});

test("defaultSupervisorConfig: AIKONOS_GATEWAY_CHILD_TTL_MS env no longer affects childTtlMs (F26)", () => {
  withEnv("AIKONOS_GATEWAY_CHILD_TTL_MS", "5", () => {
    const cfg = defaultSupervisorConfig();
    assert.equal(cfg.childTtlMs, 1_800_000, "childTtlMs must come from injected Config, not process.env");
  });
});

test("ChildSupervisor: config override for maxChildren/childTtlMs (as server.ts wires from Config) takes effect", async () => {
  // Mirrors server.ts's `{ maxChildren: cfg.maxChildren, childTtlMs: cfg.childTtlMs }`
  // wiring — proves the injected-config seam the supervisor already merges works
  // end-to-end for the two F26 fields, not just that defaultSupervisorConfig ignores env.
  const rig = makeTestRig({ maxChildren: 1, childTtlMs: 1000 });
  const identity = makeIdentity();
  const key = rig.supervisor.keyFor(identity);
  const handle = await rig.supervisor.getOrSpawn(key, identity);
  rig.supervisor.markIdle(handle);
  handle.lastUsedAt = Date.now() - 2000;
  (rig.supervisor as unknown as { reapIdle(): void }).reapIdle();
  assert.ok(rig.proxy.unregistered.length > 0, "childTtlMs=1000 override must evict the idle child after 2000ms");
});

test("supervisor: unknown AIKONOS_GATEWAY_CHILD_KEYING is rejected with actionable message", () => {
  withKeyingEnv("shared", () => {
    assert.throws(
      () => defaultSupervisorConfig(),
      (err: unknown) => {
        assert.ok(err instanceof Error);
        assert.match(err.message, /AIKONOS_GATEWAY_CHILD_KEYING/);
        assert.match(err.message, /shared/);
        assert.match(err.message, /"single".*"per-user"|"per-user".*"single"/);
        return true;
      },
    );
  });
});

// ── Approach A: replan-on-reuse so post-spawn grant changes take effect ─────────
//
// A long-lived child caches the FGA-derived tool allowlist from spawn time. These
// tests prove getOrSpawn re-resolves the plan on reuse (throttled, idle-only) and
// respawns the child only when the tool set actually changed — the fix for
// "I granted a tool but the scheduled run still doesn't have it".

// Build a supervisor whose listUserSkills reads a mutable variable, so a test can
// change the user's grants between getOrSpawn calls.
function makeMutableSkillsRig(initialSkills: string[]): {
  supervisor: ChildSupervisor;
  records: FakeSpawnRecord[];
  setSkills: (s: string[]) => void;
} {
  const { spawnFn, records } = makeFakeSpawnFn();
  const proxy = makeFakeProxy();
  let skills = initialSkills;
  const south = {
    getLlmProviders: async () => ({ providers: [] }),
    getTenantModel: async () => ({ model: "anthropic/claude-sonnet-4.6" }),
    listUserSkills: async () => ({ skills }),
    listUserAgentSkills: async () => ({ bundles: [] }),
    listAccessibleMcpServersForAgent: async () => ({ connections: [] }),
    listMcpServerToolsSouth: async () => ({ tools: [] }),
    getAgentSpec: async () => ({ found: false }),
  };
  const cfg = {
    llmModel: "anthropic/claude-sonnet-4.6",
    defaultTenantId: "00000000-0000-0000-0000-000000000001",
  };
  const { factory } = makeBridgeFactory();
  const supervisor = new ChildSupervisor(
    proxy.proxy,
    { south, cfg },
    factory,
    spawnFn,
    makeFakeCredentials(),
    { keying: "per-user" },
  );
  return { supervisor, records, setSkills: (s) => { skills = s; } };
}

test("Approach A: reused child respawns when the user's tool grants changed since spawn", async () => {
  const { supervisor, records, setSkills } = makeMutableSkillsRig(["web.fetch"]);
  const identity = makeIdentity();
  const key = supervisor.keyFor(identity);

  const h1 = await supervisor.getOrSpawn(key, identity);
  assert.equal(records.length, 1, "first call forks once");
  assert.ok(h1.allowedToolNames.includes("web_fetch"), "baseline grant present at spawn");
  assert.ok(!h1.allowedToolNames.includes("doc_write"), "ungranted tool absent at spawn");

  // Grant doc.write after the child spawned, then bypass the re-check throttle.
  setSkills(["web.fetch", "doc.write"]);
  h1.lastPlanCheckAt = 0;

  const h2 = await supervisor.getOrSpawn(key, identity);
  assert.equal(records.length, 2, "tool-set change must respawn the child");
  assert.notEqual(h2, h1, "a fresh handle replaces the stale one");
  assert.ok(h2.allowedToolNames.includes("doc_write"), "new grant now in the child's plan");
  assert.ok(h2.allowedToolNames.includes("web_fetch"), "prior grant retained");
});

test("Approach A: reused child is NOT respawned when grants are unchanged", async () => {
  const { supervisor, records } = makeMutableSkillsRig(["web.fetch"]);
  const identity = makeIdentity();
  const key = supervisor.keyFor(identity);

  const h1 = await supervisor.getOrSpawn(key, identity);
  h1.lastPlanCheckAt = 0; // force the re-check; grants are still the same

  const h2 = await supervisor.getOrSpawn(key, identity);
  assert.equal(records.length, 1, "unchanged grants must not respawn");
  assert.equal(h2, h1, "same handle reused");
});

test("Approach A: a busy child is not respawned mid-run even if grants changed", async () => {
  const { supervisor, records, setSkills } = makeMutableSkillsRig(["web.fetch"]);
  const identity = makeIdentity();
  const key = supervisor.keyFor(identity);

  const h1 = await supervisor.getOrSpawn(key, identity);
  supervisor.markBusy(h1); // a concurrent run is in flight on this child
  setSkills(["web.fetch", "doc.write"]);
  h1.lastPlanCheckAt = 0;

  const h2 = await supervisor.getOrSpawn(key, identity);
  assert.equal(records.length, 1, "busy child must not be killed mid-run");
  assert.equal(h2, h1, "busy child is reused as-is; refreshes on next idle reuse");
});

test("Approach A: re-check is throttled — no replan within PLAN_RECHECK_MS", async () => {
  const { supervisor, records, setSkills } = makeMutableSkillsRig(["web.fetch"]);
  const identity = makeIdentity();
  const key = supervisor.keyFor(identity);

  const h1 = await supervisor.getOrSpawn(key, identity);
  // Grant changes, but the throttle window has not elapsed (lastPlanCheckAt ≈ now).
  setSkills(["web.fetch", "doc.write"]);

  const h2 = await supervisor.getOrSpawn(key, identity);
  assert.equal(records.length, 1, "within the throttle window, no re-resolve/respawn");
  assert.equal(h2, h1, "same handle reused until the throttle elapses");
});

// ── F28: evictIdleForAgent — push invalidation for soul edits ──────────────────

test("evictIdleForAgent: evicts an idle child bound to the target agent", async () => {
  const rig = makeTestRig({ keying: "per-user" });
  const identity = makeIdentity("user-a", "agent-a");
  const key = rig.supervisor.keyFor(identity);

  const h1 = await rig.supervisor.getOrSpawn(key, identity);
  await flushAsync();
  assert.equal(rig.spawnRecords.length, 1);

  rig.supervisor.evictIdleForAgent("agent-a", "soul updated");

  const h2 = await rig.supervisor.getOrSpawn(key, identity);
  await flushAsync();
  assert.equal(rig.spawnRecords.length, 2, "evicted child must be respawned on next use");
  assert.notEqual(h2, h1, "fresh handle replaces the evicted one");
});

test("evictIdleForAgent: leaves a BUSY child for the target agent running", async () => {
  const rig = makeTestRig({ keying: "per-user" });
  const identity = makeIdentity("user-a", "agent-a");
  const key = rig.supervisor.keyFor(identity);

  const h1 = await rig.supervisor.getOrSpawn(key, identity);
  await flushAsync();
  rig.supervisor.markBusy(h1);

  rig.supervisor.evictIdleForAgent("agent-a", "soul updated");

  const h2 = await rig.supervisor.getOrSpawn(key, identity);
  assert.equal(rig.spawnRecords.length, 1, "busy child must not be evicted mid-run");
  assert.equal(h2, h1, "same busy handle returned");
});

test("evictIdleForAgent: leaves other agents' idle children untouched", async () => {
  const rig = makeTestRig({ keying: "per-user" });
  const identityA = makeIdentity("user-a", "agent-a");
  const identityB = makeIdentity("user-b", "agent-b");
  const keyA = rig.supervisor.keyFor(identityA);
  const keyB = rig.supervisor.keyFor(identityB);

  const hA1 = await rig.supervisor.getOrSpawn(keyA, identityA);
  const hB1 = await rig.supervisor.getOrSpawn(keyB, identityB);
  await flushAsync();
  assert.equal(rig.spawnRecords.length, 2);

  rig.supervisor.evictIdleForAgent("agent-a", "soul updated");

  const hA2 = await rig.supervisor.getOrSpawn(keyA, identityA);
  const hB2 = await rig.supervisor.getOrSpawn(keyB, identityB);
  await flushAsync();

  assert.equal(rig.spawnRecords.length, 3, "only agent-a's child was evicted and respawned");
  assert.notEqual(hA2, hA1, "agent-a's child was replaced");
  assert.equal(hB2, hB1, "agent-b's child was untouched");
});

test("evictIdleForAgent: matches a prefixed named-agent child against the bare route id", async () => {
  // Real-path shapes: src/routes/agui.ts's sessionAgentId prefixes named
  // agents as "agent:<id>" when spawning the child, but PUT /agents/:id/soul
  // (src/routes/agents.ts) calls evictIdleForAgent with the bare id from the
  // URL param. Exact string equality between "agent:agent-1" and "agent-1"
  // is always false, so the primary named-agent use case never evicted.
  const rig = makeTestRig({ keying: "per-user" });
  const identity = makeIdentity("user-a", "agent:agent-1");
  const key = rig.supervisor.keyFor(identity);

  const h1 = await rig.supervisor.getOrSpawn(key, identity);
  await flushAsync();
  assert.equal(rig.spawnRecords.length, 1);

  rig.supervisor.evictIdleForAgent("agent-1", "soul updated");

  const h2 = await rig.supervisor.getOrSpawn(key, identity);
  await flushAsync();
  assert.equal(rig.spawnRecords.length, 2, "prefixed child must be evicted by the bare route id");
  assert.notEqual(h2, h1, "fresh handle replaces the evicted one");
});

// ── Spend-caps CP3: usage relay forwarding ──────────────────────────────────────

test("CP3 usage-forward: run() forwards a child usage event to south.emitLlmUsage with the spawn-bound identity", async () => {
  // WHY: the child has no south client — the parent (this run() loop) is the
  // only place a usage event can reach the broker. The forwarded call must
  // carry the CHILD'S spawn-bound identity (tenantId/userId/agentId), not
  // anything from the IPC message body — matching the "identity is bound at
  // spawn, never from a message" invariant this file already tests elsewhere.
  const { spawnFn, records } = makeFakeSpawnFn();
  const proxy = makeFakeProxy();
  const emitLlmUsageCalls: unknown[] = [];
  const deps = makeFakeDeps({ emitLlmUsage: async (req) => { emitLlmUsageCalls.push(req); } });
  const { factory } = makeBridgeFactory();
  const supervisor = new ChildSupervisor(proxy.proxy, deps, factory, spawnFn, makeFakeCredentials());

  const identity = makeIdentity("alice", "agent-a");
  const key = supervisor.keyFor(identity);
  const handle = await supervisor.getOrSpawn(key, identity);
  await flushAsync();
  const rec = records[0];
  assert.ok(rec, "spawn record must exist");

  const runId = "run-usage-1";
  const runPromise = supervisor.run(handle, { runId, threadId: "th-1", sessionId: "sess-42", text: "hi" }, () => {});
  await flushAsync();

  rec.child.childLink.send({
    kind: "usage",
    runId,
    inputTokens: 100,
    outputTokens: 50,
    cost: 0.002,
    cacheRead: 5,
    cacheWrite: 2,
    provider: "openai",
    model: "gpt-4o",
  });
  await flushAsync();

  rec.child.childLink.send({ kind: "done", runId });
  await flushAsync();
  await runPromise;

  assert.equal(emitLlmUsageCalls.length, 1);
  assert.deepEqual(emitLlmUsageCalls[0], {
    tenantId: identity.tenantId,
    userId: identity.userId,
    agentId: identity.agentId,
    provider: "openai",
    model: "gpt-4o",
    tokensIn: 100,
    tokensOut: 50,
    cacheRead: 5,
    cacheWrite: 2,
    cost: 0.002,
    // Attribution: runId off the IPC
    // event, sessionId from the run() caller, source fixed for the child loop.
    runId,
    sessionId: "sess-42",
    source: "chat",
    quantity: 0,
    unit: "",
  });
});

test("CP3 usage-forward: missing additive fields on the wire default to 0/\"\" instead of throwing", async () => {
  // WHY: version skew (old child build, new parent) must not crash the relay
  // — a usage event carrying only token counts (the pre-CP3 shape) must still
  // forward successfully with the new fields defaulted.
  const { spawnFn, records } = makeFakeSpawnFn();
  const proxy = makeFakeProxy();
  const emitLlmUsageCalls: unknown[] = [];
  const deps = makeFakeDeps({ emitLlmUsage: async (req) => { emitLlmUsageCalls.push(req); } });
  const { factory } = makeBridgeFactory();
  const supervisor = new ChildSupervisor(proxy.proxy, deps, factory, spawnFn, makeFakeCredentials());

  const identity = makeIdentity("bob", "agent-b");
  const key = supervisor.keyFor(identity);
  const handle = await supervisor.getOrSpawn(key, identity);
  await flushAsync();
  const rec = records[0];
  assert.ok(rec, "spawn record must exist");

  const runId = "run-usage-2";
  const runPromise = supervisor.run(handle, { runId, threadId: "th-2", text: "hi" }, () => {});
  await flushAsync();

  rec.child.childLink.send({ kind: "usage", runId, inputTokens: 10, outputTokens: 5 });
  await flushAsync();

  rec.child.childLink.send({ kind: "done", runId });
  await flushAsync();
  await runPromise;

  assert.equal(emitLlmUsageCalls.length, 1);
  assert.deepEqual(emitLlmUsageCalls[0], {
    tenantId: identity.tenantId,
    userId: identity.userId,
    agentId: identity.agentId,
    provider: "",
    model: "",
    tokensIn: 10,
    tokensOut: 5,
    cacheRead: 0,
    cacheWrite: 0,
    cost: 0,
    // A caller with no session (scheduler/external) sends "" — the event still
    // records, attributed to the run alone.
    runId,
    sessionId: "",
    source: "chat",
    quantity: 0,
    unit: "",
  });
});

test("CP3 usage-forward: a fully-zero usage event is skipped (nothing happened, no round-trip)", async () => {
  const { spawnFn, records } = makeFakeSpawnFn();
  const proxy = makeFakeProxy();
  const emitLlmUsageCalls: unknown[] = [];
  const deps = makeFakeDeps({ emitLlmUsage: async (req) => { emitLlmUsageCalls.push(req); } });
  const { factory } = makeBridgeFactory();
  const supervisor = new ChildSupervisor(proxy.proxy, deps, factory, spawnFn, makeFakeCredentials());

  const identity = makeIdentity();
  const key = supervisor.keyFor(identity);
  const handle = await supervisor.getOrSpawn(key, identity);
  await flushAsync();
  const rec = records[0];
  assert.ok(rec, "spawn record must exist");

  const runId = "run-usage-3";
  const runPromise = supervisor.run(handle, { runId, threadId: "th-3", text: "hi" }, () => {});
  await flushAsync();

  rec.child.childLink.send({ kind: "usage", runId, inputTokens: 0, outputTokens: 0, cacheRead: 0, cacheWrite: 0 });
  await flushAsync();

  rec.child.childLink.send({ kind: "done", runId });
  await flushAsync();
  await runPromise;

  assert.equal(emitLlmUsageCalls.length, 0, "an all-zero usage event must not be forwarded");
});

test("CP3 usage-forward: an emitLlmUsage rejection never fails the run", async () => {
  const { spawnFn, records } = makeFakeSpawnFn();
  const proxy = makeFakeProxy();
  const deps = makeFakeDeps({ emitLlmUsage: async () => { throw new Error("broker down"); } });
  const { factory } = makeBridgeFactory();
  const supervisor = new ChildSupervisor(proxy.proxy, deps, factory, spawnFn, makeFakeCredentials());

  const identity = makeIdentity();
  const key = supervisor.keyFor(identity);
  const handle = await supervisor.getOrSpawn(key, identity);
  await flushAsync();
  const rec = records[0];
  assert.ok(rec, "spawn record must exist");

  const runId = "run-usage-4";
  const runPromise = supervisor.run(handle, { runId, threadId: "th-4", text: "hi" }, () => {});
  await flushAsync();

  rec.child.childLink.send({ kind: "usage", runId, inputTokens: 1, outputTokens: 1 });
  await flushAsync();

  rec.child.childLink.send({ kind: "done", runId });
  await flushAsync();

  await assert.doesNotReject(runPromise, "an emitLlmUsage rejection must never fail the run");
});

// ── Per-run LLM-call budget (EgressProxy property 9) ──────────────────────────

test("run(): resets the child's LLM-call budget so a pooled child starts each run with a full one", async () => {
  // WHY: the budget is per RUN but children are pooled and reused across runs.
  // Without the reset the second run on a reused child would begin already spent —
  // the cap would silently become per-child-lifetime instead of per-run.
  const rig = makeTestRig();
  const identity = makeIdentity("alice", "agent-a");
  const key = rig.supervisor.keyFor(identity);
  const handle = await rig.supervisor.getOrSpawn(key, identity);
  await flushAsync();
  const rec = rig.spawnRecords[0];
  assert.ok(rec, "spawn record must exist");

  assert.deepEqual(rig.proxy.budgetResets, [], "spawning alone must not reset anything");

  for (const runId of ["run-b-1", "run-b-2"]) {
    const runPromise = rig.supervisor.run(handle, { runId, threadId: "th-b", text: "hi" }, () => {});
    await flushAsync();
    rec.child.childLink.send({ kind: "done", runId });
    await flushAsync();
    await runPromise;
  }

  assert.deepEqual(
    rig.proxy.budgetResets,
    [handle.proxyToken, handle.proxyToken],
    "each run must reset THIS child's budget, keyed by its own proxy token",
  );
});

test("bridge factory: each per-run bridge gets a budget consumer bound to its own child's proxy token", async () => {
  // WHY: GovernanceBridge.reason/analyzeImage bypass the egress proxy but bill the
  // same run. They must book against the same counter the child's proxied calls
  // spend, otherwise they are an unbounded side channel around the cap.
  const rig = makeTestRig();
  const identity = makeIdentity("alice", "agent-a");
  const key = rig.supervisor.keyFor(identity);
  const handle = await rig.supervisor.getOrSpawn(key, identity);

  handle.setRunContext("run-seam", { ...identity }, async () => true);

  assert.equal(rig.budgetConsumers.length, 1, "one bridge must have been built for the run");
  const consume = rig.budgetConsumers[0];
  assert.ok(consume, "the bridge must be handed a budget consumer, not undefined");

  assert.equal(consume(), true);
  assert.deepEqual(
    rig.proxy.budgetConsumes,
    [handle.proxyToken],
    "the consumer must charge THIS child's token, not a shared or stale one",
  );
});

test("an over-budget run ends with an error instead of looping forever", async () => {
  // WHY: the budget only helps if a denial is terminal for the run. A 429 is
  // retried a bounded number of times by the LLM SDK inside the child and then
  // surfaces as a child `error` event — which must reject run(), freeing the pool
  // slot, rather than being swallowed into another LLM→tool→LLM iteration.
  const rig = makeTestRig();
  const identity = makeIdentity("alice", "agent-a");
  const key = rig.supervisor.keyFor(identity);
  const handle = await rig.supervisor.getOrSpawn(key, identity);
  await flushAsync();
  const rec = rig.spawnRecords[0];
  assert.ok(rec, "spawn record must exist");

  const runId = "run-over-budget";
  const runPromise = rig.supervisor.run(handle, { runId, threadId: "th-ob", text: "hi" }, () => {});
  // Attach the outcome handler before flushing — the child's error event lands
  // inside the flush below, and an unobserved rejection there fails the run.
  const settled = runPromise.then(() => "resolved", (err: unknown) => String(err));
  await flushAsync();

  rec.child.childLink.send({
    kind: "error",
    runId,
    message: "429 llm call budget exceeded for this run",
  });
  await flushAsync();

  assert.match(await settled, /budget exceeded/, "the run must reject, not resolve or hang");
  assert.equal(handle.busy, false, "the child must be released back to the pool, not left busy");
});

// ── Ephemeral subagent children ───────────
//
// WHY this block exists: a subagent branch child must start on fresh context,
// run exactly once, and leave nothing behind — no pool entry a later interactive
// getOrSpawn could land on, and no egress-proxy registration holding a real
// provider key. The eviction has to survive every terminal path (success, throw,
// crash, abort), so each of those is asserted separately rather than trusting the
// happy path to generalise.

test("CP2 ephemeral: ephemeralKey pins the synthetic-key shape the runner and teardown share", () => {
  assert.equal(ephemeralKey("run-7", 0), "subagent:run-7:0");
  assert.equal(ephemeralKey("run-7", 2), "subagent:run-7:2");
});

test("CP2 ephemeral: withEphemeralChild spawns fresh, returns fn's value, and leaves the key out of the pool", async () => {
  // WHY: the whole point of the ephemeral path. If the key stayed pooled, the
  // next getOrSpawn for it would reuse a child spawned for a finished branch.
  const rig = makeTestRig();
  const identity = makeIdentity();
  const key = ephemeralKey("run-eph-1", 0);

  let seenKey = "";
  let proxyToken = "";
  const result = await rig.supervisor.withEphemeralChild(key, identity, async (handle) => {
    seenKey = handle.key;
    proxyToken = handle.proxyToken;
    return "branch-output";
  });
  await flushAsync();

  assert.equal(result, "branch-output", "withEphemeralChild must pass fn's resolved value through");
  assert.equal(seenKey, key, "fn must receive a handle bound to the synthetic key");
  assert.equal(rig.spawnRecords.length, 1, "exactly one child must have been forked");
  assert.ok(
    rig.proxy.unregistered.includes(proxyToken),
    "the ephemeral child's proxy token must be unregistered on eviction",
  );

  // Pool-absence, observed behaviourally: asking for the same key again must fork
  // a second child rather than hand back the finished one.
  await rig.supervisor.getOrSpawn(key, identity);
  assert.equal(rig.spawnRecords.length, 2, "the ephemeral key must not have been left in the pool");
});

test("CP2 ephemeral: a throwing fn still evicts the child and releases its proxy token", async () => {
  // WHY: a leaked ephemeral child is the exact failure this path exists to
  // prevent — eviction lives in a finally, not on the success branch.
  const rig = makeTestRig();
  const identity = makeIdentity();
  const key = ephemeralKey("run-eph-2", 0);

  let proxyToken = "";
  await assert.rejects(
    () =>
      rig.supervisor.withEphemeralChild(key, identity, async (handle) => {
        proxyToken = handle.proxyToken;
        throw new Error("branch blew up");
      }),
    /branch blew up/,
    "the fn's error must propagate to the caller",
  );
  await flushAsync();

  assert.ok(proxyToken, "fn must have run and observed a handle");
  assert.ok(
    rig.proxy.unregistered.includes(proxyToken),
    "proxy token must be unregistered even when the branch throws",
  );
  await rig.supervisor.getOrSpawn(key, identity);
  assert.equal(rig.spawnRecords.length, 2, "a thrown branch must not leave its key pooled");
});

test("CP5 depth-1 cap: a subagent branch child (withEphemeralChild) never sees spawn_subagents, even holding skill:subagents", async () => {
  // WHY here and not session-plan.test.ts alone: the omission must be threaded
  // all the way from the actual branch-spawn path (withEphemeralChild), not
  // just provable at the resolveSessionPlan unit level — 's Risks table calls out exactly this gap. Asserted on BOTH the
  // tool list and the system prompt: filtering allowedToolNames after the
  // system prompt is built would still teach a tool the child will be refused.
  const { spawnFn, records } = makeFakeSpawnFn();
  const proxy = makeFakeProxy();
  const deps: SupervisorDeps = {
    south: {
      getLlmProviders: async () => ({ providers: [] }),
      getTenantModel: async () => ({ model: "anthropic/claude-sonnet-4.6" }),
      listUserSkills: async () => ({ skills: ["subagents", "web.fetch"] }),
      listUserAgentSkills: async () => ({ bundles: [] }),
      listAccessibleMcpServersForAgent: async () => ({ connections: [] }),
      listMcpServerToolsSouth: async () => ({ tools: [] }),
      getAgentSpec: async () => ({ found: false }),
    },
    cfg: { llmModel: "anthropic/claude-sonnet-4.6", defaultTenantId: "00000000-0000-0000-0000-000000000001" },
  };
  const { factory } = makeBridgeFactory();
  const supervisor = new ChildSupervisor(proxy.proxy, deps, factory, spawnFn, makeFakeCredentials());
  const identity = makeIdentity();
  const key = ephemeralKey("run-depth1", 0);

  await supervisor.withEphemeralChild(key, identity, async () => "done");
  await flushAsync();

  assert.equal(records.length, 1, "exactly one branch child must have been forked");
  const init = records[0]?.child.sentMessages.find((m) => (m as { kind: string }).kind === "init") as InitMessage | undefined;
  assert.ok(init, "the branch child must have received its init plan");
  assert.ok(
    !init!.allowedToolNames.includes("spawn_subagents"),
    "a subagent branch child must never see spawn_subagents in its own tool list, even holding skill:subagents",
  );
  assert.ok(
    !init!.systemPrompt.includes("spawn_subagents"),
    "the branch child's system prompt must not advertise a tool it will be refused",
  );

  supervisor.dispose();
});

test("CP5 depth-1 cap: an ordinary pooled child (getOrSpawn) still gets spawn_subagents when the caller holds skill:subagents", async () => {
  // WHY: a test asserting only the branch-side absence would pass even if the
  // tool were omitted for everyone — this proves the omission is scoped to the
  // branch-spawn path only, per 's brief.
  const { spawnFn, records } = makeFakeSpawnFn();
  const proxy = makeFakeProxy();
  const deps: SupervisorDeps = {
    south: {
      getLlmProviders: async () => ({ providers: [] }),
      getTenantModel: async () => ({ model: "anthropic/claude-sonnet-4.6" }),
      listUserSkills: async () => ({ skills: ["subagents", "web.fetch"] }),
      listUserAgentSkills: async () => ({ bundles: [] }),
      listAccessibleMcpServersForAgent: async () => ({ connections: [] }),
      listMcpServerToolsSouth: async () => ({ tools: [] }),
      getAgentSpec: async () => ({ found: false }),
    },
    cfg: { llmModel: "anthropic/claude-sonnet-4.6", defaultTenantId: "00000000-0000-0000-0000-000000000001" },
  };
  const { factory } = makeBridgeFactory();
  const supervisor = new ChildSupervisor(proxy.proxy, deps, factory, spawnFn, makeFakeCredentials());
  const identity = makeIdentity();
  const key = supervisor.keyFor(identity);

  await supervisor.getOrSpawn(key, identity);
  await flushAsync();

  const init = records[0]?.child.sentMessages.find((m) => (m as { kind: string }).kind === "init") as InitMessage | undefined;
  assert.ok(init, "the pooled child must have received its init plan");
  assert.ok(
    init!.allowedToolNames.includes("spawn_subagents"),
    "an ordinary chat child must still surface spawn_subagents when the caller holds the grant",
  );

  supervisor.dispose();
});

test("CP2 ephemeral: a child that crashes mid-run is evicted once, with exactly one proxy unregister", async () => {
  // WHY: onChildExit already removes the pool entry and unregisters the token,
  // so the finally-eviction must be idempotent — a second unregister for the
  // same token would be a sign the two teardown paths are fighting.
  const rig = makeTestRig();
  const identity = makeIdentity();
  const key = ephemeralKey("run-eph-3", 0);

  let resolveReady: ((h: ChildHandle) => void) | undefined;
  const handleReady = new Promise<ChildHandle>((r) => {
    resolveReady = r;
  });

  const branch = rig.supervisor.withEphemeralChild(key, identity, async (handle) => {
    resolveReady?.(handle);
    return rig.supervisor.run(handle, { runId: "run-eph-3", threadId: "th-eph", text: "go" }, () => {});
  });

  const handle = await handleReady;
  await flushAsync();
  const rec = rig.spawnRecords[0];
  assert.ok(rec, "spawn record must exist");

  // Attach the rejection expectation BEFORE triggering the crash — the branch
  // rejects synchronously inside simulateExit, and a handler attached afterwards
  // trips node's unhandled-rejection detector.
  const rejected = assert.rejects(branch, /exited mid-run/, "a crashed child must reject the branch");
  rec.child.simulateExit();
  await flushAsync();
  await rejected;

  const unregisterCount = rig.proxy.unregistered.filter((t) => t === handle.proxyToken).length;
  assert.equal(unregisterCount, 1, `proxy token must be unregistered exactly once (got ${unregisterCount})`);
  await rig.supervisor.getOrSpawn(key, identity);
  assert.equal(rig.spawnRecords.length, 2, "a crashed branch must not leave its key pooled");
});

test("CP2 ephemeral: an aborted in-flight branch is still evicted when its run rejects", async () => {
  // WHY: teardown (CP6) aborts branch children on SSE close. The abort surfaces
  // as a rejected run, which must take the same finally-eviction path.
  const rig = makeTestRig();
  const identity = makeIdentity();
  const key = ephemeralKey("run-eph-4", 0);
  const runId = "run-eph-4";

  let resolveReady: ((h: ChildHandle) => void) | undefined;
  const handleReady = new Promise<ChildHandle>((r) => {
    resolveReady = r;
  });

  const branch = rig.supervisor.withEphemeralChild(key, identity, async (handle) => {
    resolveReady?.(handle);
    return rig.supervisor.run(handle, { runId, threadId: "th-abort", text: "go" }, () => {});
  });

  const handle = await handleReady;
  await flushAsync();
  const rec = rig.spawnRecords[0];
  assert.ok(rec, "spawn record must exist");

  const rejected = assert.rejects(branch, /aborted/, "an aborted branch must reject");
  handle.abortRun(runId);
  rec.child.childLink.send({ kind: "error", runId, message: "aborted" });
  await flushAsync();
  await rejected;
  assert.ok(
    rig.proxy.unregistered.includes(handle.proxyToken),
    "an aborted branch's proxy token must be unregistered",
  );
});

test("CP2 ephemeral: an in-flight ephemeral child never satisfies a pooled getOrSpawn for the same identity", async () => {
  // WHY: depth/containment. A branch child's session was built for one subtask;
  // handing it to the user's interactive turn would swap the context underneath
  // them. The synthetic key is what keeps the two pools disjoint.
  const rig = makeTestRig();
  const identity = makeIdentity();
  const key = ephemeralKey("run-eph-5", 0);

  let resolveReady: ((h: ChildHandle) => void) | undefined;
  const handleReady = new Promise<ChildHandle>((r) => {
    resolveReady = r;
  });
  let releaseBranch: (() => void) | undefined;
  const branchGate = new Promise<void>((r) => {
    releaseBranch = r;
  });

  const branch = rig.supervisor.withEphemeralChild(key, identity, async (handle) => {
    resolveReady?.(handle);
    await branchGate;
    return "done";
  });

  const ephemeral = await handleReady;
  const pooled = await rig.supervisor.getOrSpawn(rig.supervisor.keyFor(identity), identity);

  assert.notEqual(pooled, ephemeral, "the pooled turn must get its own child, not the branch child");
  assert.equal(rig.spawnRecords.length, 2, "the pooled getOrSpawn must have forked its own child");

  releaseBranch?.();
  assert.equal(await branch, "done");
});

test("CP2 ephemeral: an in-flight branch child is busy, so an exhausted pool rejects rather than evicting it", async () => {
  // WHY two properties in one test: they are the same invariant seen from both
  // sides. The branch child must be busy for the whole of its single run (or the
  // LRU pass would take it mid-flight), and with no idle victim left the cap must
  // reject fast with GatewayOverloadError — never queue.
  const rig = makeTestRig({ keying: "per-user", maxChildren: 1 });
  const identity = makeIdentity();
  const key = ephemeralKey("run-eph-6", 0);

  let resolveReady: ((h: ChildHandle) => void) | undefined;
  const handleReady = new Promise<ChildHandle>((r) => {
    resolveReady = r;
  });
  let releaseBranch: (() => void) | undefined;
  const branchGate = new Promise<void>((r) => {
    releaseBranch = r;
  });

  const branch = rig.supervisor.withEphemeralChild(key, identity, async (handle) => {
    resolveReady?.(handle);
    await branchGate;
    return "done";
  });

  const ephemeral = await handleReady;
  assert.equal(ephemeral.busy, true, "an in-flight ephemeral child must be marked busy");

  await assert.rejects(
    () => rig.supervisor.getOrSpawn(rig.supervisor.keyFor(identity), identity),
    (err: unknown) => {
      assert.ok(err instanceof GatewayOverloadError, `must be GatewayOverloadError, got: ${String(err)}`);
      return true;
    },
    "at cap with only a busy branch child, the pool must reject instead of evicting it",
  );

  assert.equal(
    rig.proxy.unregistered.length,
    0,
    "the busy branch child must not have been LRU-evicted mid-flight",
  );

  releaseBranch?.();
  assert.equal(await branch, "done");
});

test("CP2 ephemeral: a key already live in the pool is refused instead of clobbering that child", async () => {
  // WHY: spawn() overwrites children.set(key, …), so reusing a live key would
  // orphan the existing child — never killed, proxy token never released. Fail
  // loud at the boundary instead.
  const rig = makeTestRig();
  const identity = makeIdentity();
  const pooledKey = rig.supervisor.keyFor(identity);
  const pooled = await rig.supervisor.getOrSpawn(pooledKey, identity);

  await assert.rejects(
    () => rig.supervisor.withEphemeralChild(pooledKey, identity, async () => "nope"),
    /collision/,
    "reusing a live pool key must be refused",
  );

  assert.equal(rig.spawnRecords.length, 1, "no second child may be forked for the colliding key");
  assert.equal(rig.proxy.unregistered.length, 0, "the existing pooled child must be untouched");
  assert.equal(await rig.supervisor.getOrSpawn(pooledKey, identity), pooled, "the pooled child must survive");
});

// ── CP3.5: cap reservation under concurrent fan-out (finding F-9) ──────────────

// waitUntil spins the microtask/immediate queue until pred holds. Used instead of
// a fixed flushAsync count because spawn()'s depth (credential resolve + session
// plan) is an implementation detail this test must not encode.
// Throws on exhaustion so a never-satisfied predicate fails HERE rather than
// surfacing later as a confusing count assertion.
async function waitUntil(pred: () => boolean, maxTicks = 200): Promise<void> {
  for (let i = 0; i < maxTicks; i++) {
    if (pred()) return;
    await new Promise((r) => setImmediate(r));
  }
  if (!pred()) throw new Error(`waitUntil: predicate still false after ${maxTicks} ticks`);
}

test("CP3.5 concurrent cap: N simultaneous ephemeral spawns against a cap of N-1 never over-admit", async () => {
  // WHY: enforceCapBefore reads children.size, but spawn() only does
  // children.set two awaits later. Callers that fan out with Promise.allSettled
  // (src/subagent/run.ts) therefore all clear the cap check in the same microtask
  // batch, before any of them has registered — the pool cap silently does not
  // hold. The spec's "fan-out cannot starve the interactive pool" mitigation
  // rests on that cap being real.
  const rig = makeTestRig({ keying: "per-user", maxChildren: 3 });

  const started: string[] = [];
  const failures: unknown[] = [];
  let release!: () => void;
  const held = new Promise<void>((r) => { release = r; });

  // Four branches launched with NO await between them — the real fan-out shape.
  const keys = [0, 1, 2, 3].map((i) => ephemeralKey("run-cap", i));
  const branches = keys.map((key, i) =>
    rig.supervisor
      .withEphemeralChild(key, makeIdentity(`user-${i}`, `agent-${i}`), async () => {
        started.push(key);
        await held;
        return key;
      })
      .catch((err: unknown) => {
        failures.push(err);
        return undefined;
      }),
  );

  // Let every admitted branch reach its parked fn, and every rejected one settle.
  await waitUntil(() => started.length + failures.length >= keys.length);

  assert.equal(started.length, 3, "at most maxChildren branches may be live at once");
  assert.equal(rig.spawnRecords.length, 3, "no more than maxChildren children may be forked");
  assert.equal(failures.length, 1, "the excess branch must be rejected, not admitted");
  assert.ok(
    failures[0] instanceof GatewayOverloadError,
    `excess branch must reject with GatewayOverloadError, got: ${String(failures[0])}`,
  );
  // Weak by construction: all four reservations are taken before any spawn
  // registers, so children is empty when the excess caller reaches the LRU pass
  // and there is nothing evictable regardless of implementation. The real
  // "a reservation is never the LRU victim" claim is carried by the
  // at-cap-with-idle-children test below.
  assert.equal(rig.proxy.unregistered.length, 0, "nothing is evicted when the pool holds no registered child yet");

  release();
  await Promise.all(branches);
  rig.supervisor.dispose();
});

test("CP3.5 concurrent cap boundary: exactly maxChildren simultaneous spawns from an empty pool all succeed", async () => {
  // WHY: counting reservations against the cap must not over-correct — the
  // boundary case (width == cap) has to admit every branch, or a legitimate
  // fan-out starts failing.
  const rig = makeTestRig({ keying: "per-user", maxChildren: 3 });

  const started: string[] = [];
  const failures: unknown[] = [];
  let release!: () => void;
  const held = new Promise<void>((r) => { release = r; });

  const keys = [0, 1, 2].map((i) => ephemeralKey("run-boundary", i));
  const branches = keys.map((key, i) =>
    rig.supervisor
      .withEphemeralChild(key, makeIdentity(`user-${i}`, `agent-${i}`), async () => {
        started.push(key);
        await held;
        return key;
      })
      .catch((err: unknown) => {
        failures.push(err);
        return undefined;
      }),
  );

  await waitUntil(() => started.length + failures.length >= keys.length);

  assert.deepEqual(failures, [], "no branch may be rejected when width equals the cap");
  assert.equal(started.length, 3, "every branch must be admitted");
  assert.equal(rig.spawnRecords.length, 3, "exactly maxChildren children forked");

  release();
  assert.deepEqual(await Promise.all(branches), keys, "every branch must return its own result");
  rig.supervisor.dispose();
});

test("CP3.5 reservation release: a spawn that throws frees its slot — the next spawn at the same cap succeeds", async () => {
  // WHY: a reservation taken before spawn() and released only on the success
  // path would leak a permanent phantom slot on any spawn failure (fork EAGAIN,
  // model-allowlist divergence), shrinking the usable pool by one per failure
  // until the gateway rejects everything.
  const proxy = makeFakeProxy();
  const { factory } = makeBridgeFactory();
  const spawnRecords: FakeSpawnRecord[] = [];
  let failNext = true;
  const spawnFn: SpawnChildFn = (opts) => {
    if (failNext) {
      failNext = false;
      throw new Error("fork failed: EAGAIN");
    }
    const child = makeFakeChild();
    spawnRecords.push({ opts, child });
    return child.parentLink;
  };

  const supervisor = new ChildSupervisor(
    proxy.proxy,
    makeFakeDeps(),
    factory,
    spawnFn,
    makeFakeCredentials(),
    { keying: "per-user", maxChildren: 1 },
  );

  const iA = makeIdentity("user-a", "agent-a");
  await assert.rejects(
    () => supervisor.getOrSpawn(supervisor.keyFor(iA), iA),
    /fork failed: EAGAIN/,
    "the first spawn must surface the fork failure",
  );

  // Same cap of 1, nothing live: this must be admitted.
  const iB = makeIdentity("user-b", "agent-b");
  const handle = await supervisor.getOrSpawn(supervisor.keyFor(iB), iB);
  assert.ok(handle, "a failed spawn must not leave a phantom reservation behind");
  assert.equal(spawnRecords.length, 1, "exactly one child forked successfully");

  supervisor.dispose();
});

test("CP3.5 no double-count: a caller arriving mid-spawn must never evict the freshly registered child", async () => {
  // WHY: a key held in BOTH children and reserved is not merely "temporarily
  // stricter". At the inflated count enforceCapBefore falls THROUGH to the LRU
  // pass, so an arriving caller can pick the just-registered idle child as its
  // victim — proxy.unregister + kill + shutdown — while the spawning caller is
  // still about to markBusy and run against it. Its own finally-evict is then a
  // no-op. The reservation must be released the instant children.set lands.
  //
  // Method: sweep the arriving caller across every microtask offset of an
  // in-flight spawn and assert the LRU pass never fires. sawRegistered proves
  // the sweep actually covered the post-children.set window (non-vacuity).
  const MAX_OFFSET = 60;
  let sawRegistered = false;

  for (let offset = 0; offset <= MAX_OFFSET; offset++) {
    const rig = makeTestRig({ keying: "per-user", maxChildren: 3 });

    // One busy child so the LRU pass, if it fires, can only take the in-flight one.
    const iBusy = makeIdentity("user-busy", "agent-busy");
    const hBusy = await rig.supervisor.getOrSpawn(rig.supervisor.keyFor(iBusy), iBusy);
    rig.supervisor.markBusy(hBusy);

    const iFlight = makeIdentity("user-flight", "agent-flight");
    const inFlight = rig.supervisor
      .getOrSpawn(rig.supervisor.keyFor(iFlight), iFlight)
      .catch(() => undefined);

    for (let tick = 0; tick < offset; tick++) await Promise.resolve();

    // enforceCapBefore decides synchronously on call, so the LRU pass (if any)
    // has already happened by the time this returns a promise.
    const iArriving = makeIdentity("user-arriving", "agent-arriving");
    const arriving = rig.supervisor
      .getOrSpawn(rig.supervisor.keyFor(iArriving), iArriving)
      .catch(() => undefined);

    // 2 forks = the in-flight child's children.set has landed (fork is the
    // statement immediately before it, both synchronous).
    if (rig.spawnRecords.length === 2) sawRegistered = true;

    assert.equal(
      rig.proxy.unregistered.length,
      0,
      `offset ${offset}: no child may be evicted while a spawn for a different key is in flight`,
    );

    await Promise.all([inFlight, arriving]);
    rig.supervisor.dispose();
  }

  assert.ok(
    sawRegistered,
    "the sweep never reached the window after children.set — widen MAX_OFFSET or this proves nothing",
  );
});

test("CP3.5 LRU admission at cap: concurrent spawns evict only real idle children, never a reservation", async () => {
  // WHY: the branch that admits BY EVICTING must take a reservation too, or the
  // slot it just freed is handed out twice. Deleting `this.reserved.add(newKey)`
  // from that branch makes the third branch below succeed instead of rejecting.
  // The unregistered-token assertion is the other half: a reservation has no
  // handle, so only a genuinely registered idle child may ever be a victim.
  const rig = makeTestRig({ keying: "per-user", maxChildren: 2 });

  const iA = makeIdentity("pool-a", "agent-pool-a");
  const iB = makeIdentity("pool-b", "agent-pool-b");
  const hA = await rig.supervisor.getOrSpawn(rig.supervisor.keyFor(iA), iA);
  const hB = await rig.supervisor.getOrSpawn(rig.supervisor.keyFor(iB), iB);
  // Deterministic LRU order (Date.now() can be identical for both spawns).
  hA.lastUsedAt = Date.now() - 5000;
  hB.lastUsedAt = Date.now() - 1000;

  const started: string[] = [];
  const failures: unknown[] = [];
  let release!: () => void;
  const held = new Promise<void>((r) => { release = r; });

  const keys = [0, 1, 2].map((i) => ephemeralKey("run-lru", i));
  const branches = keys.map((key, i) =>
    rig.supervisor
      .withEphemeralChild(key, makeIdentity(`branch-${i}`, `agent-branch-${i}`), async () => {
        started.push(key);
        await held;
        return key;
      })
      .catch((err: unknown) => {
        failures.push(err);
        return undefined;
      }),
  );

  await waitUntil(() => started.length + failures.length >= keys.length);

  assert.equal(started.length, 2, "at most maxChildren branches may be admitted, even when admission is by eviction");
  assert.equal(failures.length, 1, "the third branch has no evictable child left — a reservation is not a victim");
  assert.ok(
    failures[0] instanceof GatewayOverloadError,
    `the excess branch must reject with GatewayOverloadError, got: ${String(failures[0])}`,
  );
  assert.deepEqual(
    [...rig.proxy.unregistered].sort(),
    [hA.proxyToken, hB.proxyToken].sort(),
    "exactly the two real idle children may be evicted — no reserved slot, and nothing twice",
  );

  release();
  await Promise.all(branches);
  rig.supervisor.dispose();
});

test("CP3.5 drift respawn: the slot a persona-drift respawn frees is reserved for it, not handed to a new-key caller", async () => {
  // WHY: getOrSpawn's drift path does evict(key) then spawn(key) with no
  // reservation, and the gap spans two awaited south RPCs plus the whole spawn —
  // wide and macrotask-reachable, unlike the fan-out window. A concurrent caller
  // takes the freed slot and the pool ends over cap once the respawn registers.
  const proxy = makeFakeProxy();
  const { factory } = makeBridgeFactory();
  const { spawnFn, records } = makeFakeSpawnFn();

  // Park the THIRD credential resolve (A, B, then A's respawn) so the test can
  // fire the racing caller exactly inside the respawn's window.
  let credCalls = 0;
  let respawnParked = false;
  let releaseRespawn!: () => void;
  const respawnGate = new Promise<void>((r) => { releaseRespawn = r; });
  const resolveCreds: ProviderCredentialResolver = async (_identity: ResolveIdentity): Promise<ProviderCredentials> => {
    credCalls++;
    if (credCalls === 3) {
      respawnParked = true;
      await respawnGate;
    }
    return {
      upstreamBaseUrl: "https://openrouter.ai/api/v1",
      apiKey: "REAL_API_KEY_NEVER_IN_CHILD",
      modelId: "anthropic/claude-sonnet-4.6",
      modelAllowlist: ["anthropic/claude-sonnet-4.6"],
      fallbacks: [],
    };
  };

  // Forcing drift through the protected seam keeps the test independent of how
  // the session plan happens to derive a tool set from the fake south.
  class DriftingSupervisor extends ChildSupervisor {
    drifted = false;
    protected override async resolveAllowedToolNames(
      identity: Identity,
      agentSpec?: AgentSpec,
    ): Promise<{ allowedToolNames: string[]; systemPrompt: string }> {
      if (this.drifted) return { allowedToolNames: ["doc_write"], systemPrompt: "drifted persona" };
      return super.resolveAllowedToolNames(identity, agentSpec);
    }
  }

  const supervisor = new DriftingSupervisor(
    proxy.proxy,
    makeFakeDeps(),
    factory,
    spawnFn,
    resolveCreds,
    { keying: "per-user", maxChildren: 2 },
  );

  const iA = makeIdentity("drift-a", "agent-drift-a");
  const iB = makeIdentity("drift-b", "agent-drift-b");
  const hA = await supervisor.getOrSpawn(supervisor.keyFor(iA), iA);
  const hB = await supervisor.getOrSpawn(supervisor.keyFor(iB), iB);
  // B busy → once A's slot is reserved there is no victim left, so the racing
  // caller must be rejected rather than quietly admitted on top of the respawn.
  supervisor.markBusy(hB);

  hA.lastPlanCheckAt = 0;
  supervisor.drifted = true;
  const respawn = supervisor.getOrSpawn(supervisor.keyFor(iA), iA);

  await waitUntil(() => respawnParked);
  // A is evicted; its replacement has not registered yet.

  const iC = makeIdentity("drift-c", "agent-drift-c");
  await assert.rejects(
    () => supervisor.getOrSpawn(supervisor.keyFor(iC), iC),
    (err: unknown) => {
      assert.ok(err instanceof GatewayOverloadError, `must be GatewayOverloadError, got: ${String(err)}`);
      return true;
    },
    "a new-key caller must not take the slot the drift respawn just freed for itself",
  );

  releaseRespawn();
  await respawn;

  assert.equal(records.length, 3, "A, B, and A's respawn — a fourth fork means the cap was exceeded");
  assert.equal(proxy.unregistered.length, 1, "only A's original proxy token may have been released");

  supervisor.dispose();
});

test("CP3.5 drift respawn: a throwing evict must not leave a permanent phantom reservation", async () => {
  // WHY: the drift block's catch swallows anything evict() throws (dispose,
  // proxy.unregister, link.send are all reachable throws) and falls through to
  // `return existing` — spawn() never runs, so its finally never releases. If the
  // reservation is taken BEFORE the evict, the key stays in reserved for the rest
  // of the process lifetime and the pool is silently one slot smaller forever.
  // Reserving only after a successful evict removes the leak.
  const proxy = makeFakeProxy();
  const { factory } = makeBridgeFactory();
  const { spawnFn } = makeFakeSpawnFn();

  class DriftingSupervisor extends ChildSupervisor {
    drifted = false;
    protected override async resolveAllowedToolNames(
      identity: Identity,
      agentSpec?: AgentSpec,
    ): Promise<{ allowedToolNames: string[]; systemPrompt: string }> {
      if (this.drifted) return { allowedToolNames: ["doc_write"], systemPrompt: "drifted persona" };
      return super.resolveAllowedToolNames(identity, agentSpec);
    }
  }

  const supervisor = new DriftingSupervisor(
    proxy.proxy,
    makeFakeDeps(),
    factory,
    spawnFn,
    makeFakeCredentials(),
    { keying: "per-user", maxChildren: 2 },
  );

  const iA = makeIdentity("phantom-a", "agent-phantom-a");
  const keyA = supervisor.keyFor(iA);
  const hA = await supervisor.getOrSpawn(keyA, iA);

  // Blow up inside evict, after children.delete has already landed.
  hA.bridgeServer.dispose = () => {
    throw new Error("dispose blew up");
  };

  hA.lastPlanCheckAt = 0;
  supervisor.drifted = true;
  const afterDrift = await supervisor.getOrSpawn(keyA, iA);
  assert.equal(afterDrift, hA, "a failed evict leaves the drift path returning the existing handle");

  // The pool must now admit maxChildren fresh keys. B is marked busy so the
  // second admission cannot be papered over by an LRU eviction — a leaked
  // reservation for A makes C reject with GatewayOverloadError.
  supervisor.drifted = false;
  const iB = makeIdentity("phantom-b", "agent-phantom-b");
  const hB = await supervisor.getOrSpawn(supervisor.keyFor(iB), iB);
  supervisor.markBusy(hB);

  const iC = makeIdentity("phantom-c", "agent-phantom-c");
  const hC = await supervisor.getOrSpawn(supervisor.keyFor(iC), iC);
  assert.ok(hC, "a failed drift evict must not permanently consume a pool slot");

  supervisor.dispose();
});

// ── CP6: evictBranchesForRun — run-teardown sweep ──
//
// WHY this block exists: an `/agui` SSE close or user stop must abort every
// in-flight branch child of that run and release its egress-proxy token, so no
// branch child or proxy registration outlives the run that spawned it — without
// ever touching a sibling run's branches or an ordinary pooled child.

test("branchKeyPrefix: pins the shape ephemeralKey builds on, so run-teardown's match can never drift from the key format", () => {
  assert.equal(branchKeyPrefix("run-7"), "subagent:run-7:");
  assert.equal(ephemeralKey("run-7", 0), `${branchKeyPrefix("run-7")}0`);
  assert.equal(ephemeralKey("run-7", 2), `${branchKeyPrefix("run-7")}2`);
});

test("CP6 evictBranchesForRun: evicts every branch child of runId (abortRun before evict), leaves an unrelated pooled child untouched", async () => {
  const rig = makeTestRig();
  const identity = makeIdentity();
  const runId = "run-teardown-1";
  let release0!: () => void;
  let release1!: () => void;
  const held0 = new Promise<void>((r) => { release0 = r; });
  const held1 = new Promise<void>((r) => { release1 = r; });

  const branch0 = rig.supervisor.withEphemeralChild(ephemeralKey(runId, 0), identity, async () => {
    await held0;
    return "b0";
  });
  const branch1 = rig.supervisor.withEphemeralChild(ephemeralKey(runId, 1), identity, async () => {
    await held1;
    return "b1";
  });
  await flushAsync();

  // An ordinary pooled (non-branch) child must be a control: teardown of one
  // run's fan-out must never touch the interactive chat child sharing the pool.
  const pooledKey = rig.supervisor.keyFor(identity);
  const pooled = await rig.supervisor.getOrSpawn(pooledKey, identity);

  const rec0 = rig.spawnRecords[0];
  const rec1 = rig.spawnRecords[1];
  assert.ok(rec0 && rec1, "both branch children must have spawned");

  const abortCalls: string[] = [];
  // Cannot reach the branch handles directly (withEphemeralChild doesn't hand
  // them out), so assert the abort DIRECTIVE reached each child's own link
  // instead — sentMessages already records every "abort"/"shutdown" the
  // supervisor sent, in arrival order.
  rec0.child.childLink.on("abort", (msg) => abortCalls.push(`0:${msg.runId}`));
  rec1.child.childLink.on("abort", (msg) => abortCalls.push(`1:${msg.runId}`));

  rig.supervisor.evictBranchesForRun(runId, "run teardown");
  await flushAsync();

  assert.deepEqual(abortCalls.sort(), [`0:${runId}`, `1:${runId}`], "both branch children must receive an abort for this run");
  assert.ok(rec0.child.sentMessages.some((m) => (m as { kind: string }).kind === "shutdown"), "branch 0 must receive shutdown");
  assert.ok(rec1.child.sentMessages.some((m) => (m as { kind: string }).kind === "shutdown"), "branch 1 must receive shutdown");

  // The leak half is synchronous and immediate — no need to wait for either
  // child to actually exit: evict() unregisters the proxy token and drops the
  // pool entry as part of the sweep call itself.
  assert.equal(rig.proxy.unregistered.length, 2, "both branch proxy tokens must be released by the sweep");
  assert.ok(!rig.proxy.unregistered.includes(pooled.proxyToken), "the pooled child's own token must survive teardown of a different run");

  // The pooled child must be wholly unaffected: same handle, no abort sent to it.
  assert.equal(await rig.supervisor.getOrSpawn(pooledKey, identity), pooled, "the pooled child must be untouched by branch teardown");

  release0();
  release1();
  await Promise.all([branch0, branch1]);
  rig.supervisor.dispose();
});

test("CP6 evictBranchesForRun: a sibling run's branch children are untouched — one user's disconnect must not kill another's fan-out", async () => {
  // WHY: the drainForRun bug's shape (F-.../CP2 era) — a teardown keyed on the
  // wrong scope silently wipes out an unrelated run.
  const rig = makeTestRig();
  const identity = makeIdentity();
  let releaseOurs!: () => void;
  let releaseTheirs!: () => void;
  const heldOurs = new Promise<void>((r) => { releaseOurs = r; });
  const heldTheirs = new Promise<void>((r) => { releaseTheirs = r; });

  const ours = rig.supervisor.withEphemeralChild(ephemeralKey("run-ours", 0), identity, async () => {
    await heldOurs;
    return "ours";
  });
  const theirs = rig.supervisor.withEphemeralChild(ephemeralKey("run-theirs", 0), identity, async () => {
    await heldTheirs;
    return "theirs";
  });
  await flushAsync();

  rig.supervisor.evictBranchesForRun("run-ours", "run teardown");

  assert.equal(rig.proxy.unregistered.length, 1, "exactly one branch (the matching run's) must be evicted");

  // The other run's branch must still be live and reachable — asking for its
  // key again must refuse as a live-key collision, not fork a fresh child.
  await assert.rejects(
    () => rig.supervisor.withEphemeralChild(ephemeralKey("run-theirs", 0), identity, async () => "nope"),
    /collision/,
    "run-theirs' branch must still be pooled — untouched by run-ours' teardown",
  );

  releaseOurs();
  releaseTheirs();
  await Promise.all([ours, theirs]);
  rig.supervisor.dispose();
});

test("CP6 evictBranchesForRun: a run with no fan-out is a clean no-op — ordinary chat teardown must not regress", async () => {
  const rig = makeTestRig();
  const identity = makeIdentity();
  const pooledKey = rig.supervisor.keyFor(identity);
  const pooled = await rig.supervisor.getOrSpawn(pooledKey, identity);

  // No branch children exist for this (or any) run — this is what fires on
  // EVERY normal interactive chat close, not just a fan-out turn.
  rig.supervisor.evictBranchesForRun("run-with-no-fanout", "run teardown");

  assert.equal(rig.proxy.unregistered.length, 0, "nothing may be evicted when the run spawned no branches");
  assert.equal(await rig.supervisor.getOrSpawn(pooledKey, identity), pooled, "the pooled child must be untouched");
  rig.supervisor.dispose();
});

test("CP6 the real risk — a hang, not a leak: evicting a branch mid-flight does NOT settle its pending run() on its own; only the child's own exit does, and then it rejects instead of hanging", async () => {
  // WHY this test exists: teardown
  // evicts a branch child WHILE withEphemeralChild's callback is still
  // awaiting supervisor.run(...). Proving the fix means proving BOTH halves:
  //   1. evict() alone (children.delete/dispose/unregister/send-shutdown/kill)
  //      does NOT itself resolve or reject the pending run() promise — if it
  //      did settle synchronously here, that would be masking a race no real
  //      child obeys (a real child's shutdown handling is asynchronous IPC).
  //   2. once the child's own process actually exits — simulated here via
  //      simulateExit(), exactly mirroring child-entry.ts's shutdown handler
  //      calling process.exit(0) in production — the SAME exitHandler
  //      ChildSupervisor.run() already wires (onChildExit's twin) rejects the
  //      pending promise. So the fan-out's Promise.allSettled provably
  //      resolves; it cannot hang forever waiting on an evicted branch.
  const rig = makeTestRig();
  const identity = makeIdentity();
  const runId = "run-hang-proof";
  const key = ephemeralKey(runId, 0);

  let resolveReady: ((h: ChildHandle) => void) | undefined;
  const handleReady = new Promise<ChildHandle>((r) => { resolveReady = r; });

  const branch = rig.supervisor.withEphemeralChild(key, identity, async (handle) => {
    resolveReady?.(handle);
    return rig.supervisor.run(handle, { runId, threadId: "th-hang", text: "go" }, () => {});
  });

  const handle = await handleReady;
  await flushAsync();
  const rec = rig.spawnRecords[0];
  assert.ok(rec, "spawn record must exist");

  let settled = false;
  branch.then(() => { settled = true; }, () => { settled = true; });

  rig.supervisor.evictBranchesForRun(runId, "run teardown");
  await flushAsync();

  // Half 1: the leak is fixed immediately (synchronous), but the branch's own
  // pending run() promise must NOT have settled merely from evict() — proving
  // the settlement genuinely depends on the child's exit, not an implicit
  // reject buried inside evict() that a reader could mistake for the real fix.
  assert.equal(rig.proxy.unregistered.length, 1, "the proxy token must already be released by the sweep");
  assert.equal(settled, false, "evict() alone must not settle the branch's pending run() — only the child's own exit does");

  // Half 2: once the child actually exits (production: its shutdown handler's
  // process.exit(0); here: simulateExit()), the pending run() promise must
  // reject rather than hang forever.
  const rejected = assert.rejects(branch, /exited mid-run/, "the branch must reject once its evicted child actually exits, never hang");
  rec.child.simulateExit();
  await flushAsync();
  await rejected;

  assert.equal(settled, true, "the fan-out's Promise.allSettled provably resolves once the evicted child exits");
  const unregisterCount = rig.proxy.unregistered.filter((t) => t === handle.proxyToken).length;
  assert.equal(unregisterCount, 1, `the proxy token must be released exactly once across both the sweep and the child's own exit (got ${unregisterCount})`);

  // The key must not be left stuck in the pool — a fresh branch for the same
  // run id (a retry) must fork a brand-new child, not collide.
  await rig.supervisor.getOrSpawn(key, identity);
  assert.equal(rig.spawnRecords.length, 2, "the evicted branch key must not remain pooled");
});

test("ProcessChannel.send after the child process has exited is a silent no-op — proc.send() on a torn-down channel would otherwise crash the whole gateway", async () => {
  // WHY a REAL fork, not the fake channel: the fake channel never throws or
  // emits 'error' regardless of this guard, so it cannot prove the crash risk
  // is real or that the fix closes it. Verified empirically (see supervisor.ts's
  // ProcessChannel.send comment): calling proc.send() on a ChildProcess whose
  // channel is already closed schedules an unlistened 'error' event
  // (ERR_IPC_CHANNEL_CLOSED) on a later tick — reachable in production the
  // moment CP6's run-teardown evicts a genuinely in-flight branch, because the
  // branch's own `finally` (src/subagent/run.ts) still calls
  // handle.abortRun() — i.e. link.send() — after the evicted child has
  // already exited.
  const dir = mkdtempSync(join(tmpdir(), "gw-child-noop-"));
  const scriptPath = join(dir, "noop.cjs");
  writeFileSync(scriptPath, "process.exit(0);\n");
  const proc = fork(scriptPath, [], { silent: true });
  await new Promise<void>((resolve) => proc.on("exit", () => resolve()));
  assert.equal(proc.connected, false, "precondition: a real child must be disconnected once it has exited");

  // Replace proc.send with a spy that records the attempt WITHOUT calling the
  // real (already-torn-down) send — calling the real one here would reproduce
  // the very crash this test exists to prevent, defeating the point of a safe
  // regression test.
  let sendAttempted = false;
  proc.send = ((..._args: unknown[]) => {
    sendAttempted = true;
    return true;
  }) as typeof proc.send;

  const channel = new ProcessChannel(proc);
  channel.send({ kind: "shutdown" });

  assert.equal(sendAttempted, false, "send on an already-disconnected child must never reach the real proc.send()");
});

// ── CP8 usage attribution ──────────────────────
//
// The "chat" direction is already pinned by the CP3 usage-forward tests above
// (e.g. "run() forwards a child usage event to south.emitLlmUsage with the
// spawn-bound identity") — those go through getOrSpawn, never
// withEphemeralChild, so isSubagentBranch is false on every handle they touch
// and source stays "chat". This section pins the other direction only.

test("CP8 source tagging: a subagent branch child's usage forwards with source \"subagent\", under its REAL spawn-bound identity — never derived from the synthetic pool key", async () => {
  // WHY the pool key is asserted to carry none of the identity fields: the key
  // (ephemeralKey) is "subagent:<runId>:<index>" — no tenantId/userId/agentId
  // component at all. If a regression made forwardUsage derive source (or
  // identity) by parsing the key instead of reading a fact recorded on the
  // handle at fork time, this test would have nothing to parse and would fail
  // loudly rather than pass by accident.
  const { spawnFn, records } = makeFakeSpawnFn();
  const proxy = makeFakeProxy();
  const emitLlmUsageCalls: unknown[] = [];
  const deps = makeFakeDeps({ emitLlmUsage: async (req) => { emitLlmUsageCalls.push(req); } });
  const { factory } = makeBridgeFactory();
  const supervisor = new ChildSupervisor(proxy.proxy, deps, factory, spawnFn, makeFakeCredentials());

  const identity = makeIdentity("branch-caller", "branch-agent");
  const runId = "run-cp8-1";
  const key = ephemeralKey(runId, 0);
  assert.ok(
    !key.includes(identity.userId) && !key.includes(identity.agentId) && !key.includes(identity.tenantId),
    "precondition: the ephemeral pool key must carry no identity component",
  );

  await supervisor.withEphemeralChild(key, identity, async (handle) => {
    const runPromise = supervisor.run(handle, { runId, threadId: "th-cp8", sessionId: "sess-cp8", text: "go" }, () => {});
    await flushAsync();
    const rec = records[0];
    assert.ok(rec, "spawn record must exist");
    rec.child.childLink.send({ kind: "usage", runId, inputTokens: 7, outputTokens: 3 });
    await flushAsync();
    rec.child.childLink.send({ kind: "done", runId });
    await runPromise;
    return "branch done";
  });

  assert.equal(emitLlmUsageCalls.length, 1);
  assert.deepEqual(emitLlmUsageCalls[0], {
    tenantId: identity.tenantId,
    userId: identity.userId,
    agentId: identity.agentId,
    provider: "",
    model: "",
    tokensIn: 7,
    tokensOut: 3,
    cacheRead: 0,
    cacheWrite: 0,
    cost: 0,
    runId,
    sessionId: "sess-cp8",
    source: "subagent",
    quantity: 0,
    unit: "",
  });
});
