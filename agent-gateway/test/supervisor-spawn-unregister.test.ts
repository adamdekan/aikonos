// CP1 item 2: ChildSupervisor.spawn
// must unregister the proxy token on ANY throw between proxy.register() and a
// fully-built handle — not only the allowlist-mismatch branch (already covered
// by supervisor-credentials-coherence.test.ts). A leaked registration means a
// real upstream API key sits in EgressProxy.children with no child ever
// spawned to use it.
import { test } from "node:test";
import assert from "node:assert/strict";
import { ChildSupervisor } from "../src/ipc/supervisor.js";
import type { SpawnChildFn, SupervisorDeps, ProviderCredentialResolver } from "../src/ipc/supervisor.js";
import type { EgressProxy, RegisterResult, RegisterOptions } from "../src/llm/egress-proxy.js";
import type { Identity } from "../src/broker/governance.js";
import { makePairedChannel, ParentLink } from "../src/ipc/protocol.js";

function makeIdentity(): Identity {
  return { tenantId: "tenant-1", userId: "user-1", agentId: "agent-default" };
}

function makeTrackedProxy() {
  const registered: string[] = [];
  const unregistered: string[] = [];
  const proxy = {
    register: (_opts: RegisterOptions): RegisterResult => {
      const token = `token-${registered.length}`;
      registered.push(token);
      return { childToken: token, childBaseUrl: `http://127.0.0.1:9999/${token}/v1` };
    },
    unregister: (token: string) => { unregistered.push(token); },
    resetRunBudget: () => {},
    listen: async () => 9999,
    close: async () => {},
  } as unknown as EgressProxy;
  return { proxy, registered, unregistered };
}

// spawnChild throwing (e.g. a fork failure) is the OTHER step named in the
// spec item alongside resolveSessionPlan — every south RPC inside
// resolveSessionPlan itself fails open (warn + fallback, never throws), so a
// forced spawnChild failure is the realistic way to exercise "any throw from
// those steps" without contriving an unrelated failure mode.
function makeThrowingSpawn(): { spawnFn: SpawnChildFn; spawnCount: number[] } {
  const spawnCount = [0];
  const spawnFn: SpawnChildFn = () => {
    spawnCount[0]++;
    throw new Error("fork failed: EAGAIN");
  };
  return { spawnFn, spawnCount };
}

const credsResolver: ProviderCredentialResolver = async () => ({
  upstreamBaseUrl: "https://openrouter.ai/api/v1",
  apiKey: "fallback-key",
  modelId: "anthropic/claude-sonnet-4.6",
  modelAllowlist: ["anthropic/claude-sonnet-4.6"],
  fallbacks: [],
});

test("spawn: spawnChild throwing (not the allowlist-mismatch branch) still unregisters the proxy token — no leaked registration", async () => {
  const deps: SupervisorDeps = {
    south: {
      getLlmProviders: async () => ({ providers: [] }),
      getTenantModel: async () => ({ model: "anthropic/claude-sonnet-4.6" }),
      listUserSkills: async () => ({ skills: [] }),
      listUserAgentSkills: async () => ({ bundles: [] }),
      listAccessibleMcpServersForAgent: async () => ({ connections: [] }),
      listMcpServerToolsSouth: async () => ({ tools: [] }),
      getAgentSpec: async () => ({ found: false }),
    },
    cfg: { llmModel: "anthropic/claude-sonnet-4.6", defaultTenantId: "tenant-1" },
  };

  const { proxy, registered, unregistered } = makeTrackedProxy();
  const { spawnFn, spawnCount } = makeThrowingSpawn();

  const supervisor = new ChildSupervisor(
    proxy,
    deps,
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
    credsResolver,
  );

  const identity = makeIdentity();
  const key = supervisor.keyFor(identity);

  await assert.rejects(
    () => supervisor.getOrSpawn(key, identity),
    /fork failed: EAGAIN/,
  );

  assert.equal(spawnCount[0], 1, "spawnChild was attempted exactly once");
  assert.equal(registered.length, 1, "proxy.register was called once, before the failing fork");
  assert.deepEqual(unregistered, registered, "the failing fork must unregister the proxy token it registered — no leaked registration");

  supervisor.dispose();
});

// A synchronous throw from handle construction (RemoteBridgeServer, onExit
// wiring, or the init send) is AFTER the fork succeeds — the guard must cover
// this too, not only resolveSessionPlan/spawnChild, or a live upstream key
// stays registered with a forked-but-unusable child.
test("spawn: a throw during handle construction (after a successful fork) still unregisters the proxy token", async () => {
  const deps: SupervisorDeps = {
    south: {
      getLlmProviders: async () => ({ providers: [] }),
      getTenantModel: async () => ({ model: "anthropic/claude-sonnet-4.6" }),
      listUserSkills: async () => ({ skills: [] }),
      listUserAgentSkills: async () => ({ bundles: [] }),
      listAccessibleMcpServersForAgent: async () => ({ connections: [] }),
      listMcpServerToolsSouth: async () => ({ tools: [] }),
      getAgentSpec: async () => ({ found: false }),
    },
    cfg: { llmModel: "anthropic/claude-sonnet-4.6", defaultTenantId: "tenant-1" },
  };

  const { proxy, registered, unregistered } = makeTrackedProxy();
  const spawnFn: SpawnChildFn = () => {
    const [parentSide] = makePairedChannel();
    const link = new ParentLink(parentSide);
    // Simulate a post-fork wiring failure (e.g. the child channel is already
    // gone) — onExit is the first thing called on the link after construction.
    link.onExit = () => { throw new Error("onExit wiring failed"); };
    return link;
  };

  const supervisor = new ChildSupervisor(
    proxy,
    deps,
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
    credsResolver,
  );

  const identity = makeIdentity();
  const key = supervisor.keyFor(identity);

  await assert.rejects(
    () => supervisor.getOrSpawn(key, identity),
    /onExit wiring failed/,
  );

  assert.equal(registered.length, 1, "proxy.register was called once, before the successful fork");
  assert.deepEqual(unregistered, registered, "a post-fork construction failure must still unregister the proxy token");

  supervisor.dispose();
});
