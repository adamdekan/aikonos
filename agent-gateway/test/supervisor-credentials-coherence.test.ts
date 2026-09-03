// CP1 Case C: credentials and
// session plan must agree on the model. resolveCredentials (the parent's
// proxy.register source) and resolveSessionPlan (the child's InitMessage
// source) run two INDEPENDENT south resolutions — a divergence between them
// (e.g. one call transiently fails while the other succeeds) previously
// produced a spawned child whose plan.modelId the proxy's modelAllowlist
// rejected on every LLM call, with nothing surfaced on /agui (the second half
// of the on-prem incident). ChildSupervisor.spawn now asserts the two agree
// BEFORE forking the child.
import { test } from "node:test";
import assert from "node:assert/strict";
import { ChildSupervisor, GatewayOverloadError } from "../src/ipc/supervisor.js";
import type { SpawnChildFn, SupervisorDeps, ProviderCredentialResolver } from "../src/ipc/supervisor.js";
import type { EgressProxy, RegisterResult, RegisterOptions } from "../src/llm/egress-proxy.js";
import type { Identity } from "../src/broker/governance.js";

function makeIdentity(): Identity {
  return { tenantId: "tenant-1", userId: "user-1", agentId: "agent-default" };
}

// Fake proxy that records register/unregister calls so the test can prove no
// leaked registration survives a coherence-guard rejection.
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

function makeFakeSpawn(): { spawnFn: SpawnChildFn; spawnCount: number[] } {
  const spawnCount = [0];
  const spawnFn: SpawnChildFn = () => {
    spawnCount[0]++;
    return {
      onExit: () => {},
      offExit: () => {},
      kill: () => {},
      send: () => {},
      on: () => {},
      off: () => {},
    } as unknown as ReturnType<SpawnChildFn>;
  };
  return { spawnFn, spawnCount };
}

test("CP1 Case C: session-plan model outside the resolved credentials' allowlist rejects spawn — no fork, no leaked proxy registration", async () => {
  // Credentials resolve to a fallback allowlist that does NOT include the
  // model resolveSessionPlan will independently pick (DB provider present in
  // its own south call, divergent from what the credential resolver saw).
  const credsResolver: ProviderCredentialResolver = async () => ({
    upstreamBaseUrl: "https://openrouter.ai/api/v1",
    apiKey: "fallback-key",
    modelId: "anthropic/claude-sonnet-4.6",
    modelAllowlist: ["anthropic/claude-sonnet-4.6"],
    fallbacks: [],
  });

  const deps: SupervisorDeps = {
    south: {
      getLlmProviders: async () => ({
        providers: [
          {
            id: "openai-prod",
            enabled: true,
            isDefault: true,
            models: [{ id: "gpt-5.4-nano" }],
          },
        ],
      }),
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
  const { spawnFn, spawnCount } = makeFakeSpawn();

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
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /llm credentials unavailable/);
      assert.match(err.message, /gpt-5\.4-nano/);
      return !(err instanceof GatewayOverloadError);
    },
  );

  assert.equal(spawnCount[0], 0, "the child process must NEVER be forked when the coherence guard rejects");
  assert.equal(registered.length, 1, "proxy.register was called once (before the guard tripped)");
  assert.deepEqual(unregistered, registered, "the coherence guard must unregister the proxy token it registered — no leaked registration");

  supervisor.dispose();
});
