// F8 regression test: a scheduled run that times out must abort the child run,
// not just clear the run context.
//
// WHY: runViaChild's finally block previously called only clearRunContext(runId)
// on the timeout path. clearRunContext removes the per-run identity/approver but
// does NOT tell the child to stop — the supervisor's markBusy/markIdle bookkeeping
// (set by supervisor.run()) stays busy=true forever because nothing ever resolves
// or rejects the in-flight run() promise from the child's side. reapIdle() skips
// busy children, so the pool can never evict a timed-out child — it burns tokens
// indefinitely. abortRun(runId) sends the abort IPC directive that makes the child
// stop and the run settle. Mirrors the pattern already correct in
// src/external/core.ts's finally (abortRun before clearRunContext).
import { test } from "node:test";
import assert from "node:assert/strict";
import { tick } from "../src/scheduler/ticker.js";
import type { Config } from "../src/config.js";
import type { Logger } from "../src/log.js";
import type { BrokerClients } from "../src/broker/clients.js";
import type { Identity, Approver } from "../src/broker/governance.js";
import type { RunIdentity } from "../src/ipc/bridge-server.js";
import { makePairedChannel, ParentLink, ChildLink } from "../src/ipc/protocol.js";
import type { DoneEvent, PromptMessage } from "../src/ipc/protocol.js";
import {
  ChildSupervisor,
  type SupervisorDeps,
  type ProviderCredentialResolver,
  type SpawnChildFn,
} from "../src/ipc/supervisor.js";
import type { EgressProxy, RegisterResult, RegisterOptions } from "../src/llm/egress-proxy.js";
import type { ResolveIdentity } from "../src/pi/session-plan.js";

const TENANT = "11111111-1111-1111-1111-111111111111";

const baseCfg = {
  defaultTenantId: TENANT,
  schedulerEnabled: true,
  schedulerTickMs: 30000,
  schedulerClaimLimit: 5,
  // Short timeout so the test doesn't have to wait around for a real run.
  schedulerRunTimeoutMs: 50,
  gatewaySpiffeId: "spiffe://aikonos.com/agent-gateway",
  agentForUserOverrides: {},
  openrouterApiKey: "",
  llmModel: "",
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

const log: Logger = {
  info: () => {},
  warn: () => {},
  error: () => {},
  debug: () => {},
} as unknown as Logger;

function makeFakeProxy(): EgressProxy {
  return {
    register: (_opts: RegisterOptions): RegisterResult => ({
      childToken: "tok",
      childBaseUrl: "http://127.0.0.1:9999/tok/v1",
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
      getTenantModel: async () => ({ model: "" }),
      listUserSkills: async () => ({ skills: [] }),
      listUserAgentSkills: async () => ({ bundles: [] }),
      listAccessibleMcpServersForAgent: async () => ({ connections: [] }),
      listMcpServerToolsSouth: async () => ({ tools: [] }),
      getAgentSpec: async () => ({ found: false }),
    },
    // llmModel must match makeFakeCreds()'s modelAllowlist below — the
    // supervisor's CP1 coherence guard
    // rejects a spawn whose session-plan model isn't in the resolved
    // credentials' allowlist, so the two fakes must agree the same way real
    // resolution would.
    cfg: { llmModel: "m1", defaultTenantId: TENANT },
  };
}

function makeFakeCreds(): ProviderCredentialResolver {
  return async (_: ResolveIdentity) => ({
    upstreamBaseUrl: "https://openrouter.ai/api/v1",
    apiKey: "dummy",
    modelId: "m1",
    modelAllowlist: ["m1"],
    fallbacks: [],
  });
}

test("F8: a scheduled run that times out calls handle.abortRun(runId) before clearRunContext", async () => {
  // A fake child that never replies — the "prompt" handler is a no-op, so
  // supervisor.run()'s promise never settles and the timeoutMs race in
  // runViaChild is what ends the run.
  const [parentSide] = makePairedChannel();
  const capturedLink = new ParentLink(parentSide);
  capturedLink.onExit = () => {};
  capturedLink.kill = () => {};

  const supervisor = new ChildSupervisor(
    makeFakeProxy(),
    makeFakeDeps(),
    (_identity: RunIdentity, _approver: Approver) => ({
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
    (() => capturedLink) as SpawnChildFn,
    makeFakeCreds(),
  );

  // Spy on the handle's abortRun/clearRunContext calls by wrapping getOrSpawn —
  // the only seam ticker.ts's runViaChild uses to obtain the handle.
  const calls: Array<{ method: "abortRun" | "clearRunContext"; runId: string }> = [];
  const originalGetOrSpawn = supervisor.getOrSpawn.bind(supervisor);
  supervisor.getOrSpawn = async (key, identity, agentSpec) => {
    const handle = await originalGetOrSpawn(key, identity, agentSpec);
    const originalAbort = handle.abortRun.bind(handle);
    const originalClear = handle.clearRunContext.bind(handle);
    handle.abortRun = (runId: string) => {
      calls.push({ method: "abortRun", runId });
      originalAbort(runId);
    };
    handle.clearRunContext = (runId: string) => {
      calls.push({ method: "clearRunContext", runId });
      originalClear(runId);
    };
    return handle;
  };

  const south = {
    claimDueScheduledRuns: () =>
      Promise.resolve({
        runs: [
          {
            id: "run-timeout",
            ownerUserId: "alice@example.com",
            prompt: "hang forever",
            approvedTools: [],
            ownerGrant: "v1.tenant.owner.expires.sig",
          },
        ],
      }),
    reportScheduledRunResult: () => Promise.resolve(),
  };
  const clients = { south } as unknown as BrokerClients;

  await tick(baseCfg, clients, log, supervisor);

  const abortCalls = calls.filter((c) => c.method === "abortRun");
  const clearCalls = calls.filter((c) => c.method === "clearRunContext");
  assert.equal(abortCalls.length, 1, "abortRun must be called exactly once on timeout");
  assert.equal(clearCalls.length, 1, "clearRunContext must still be called");
  // ticker.ts mints its own runId (randomUUID) per run — distinct from the
  // broker's schedule id ("run-timeout") — so assert abortRun/clearRunContext
  // agree with each other rather than hardcoding the minted value.
  assert.equal(
    abortCalls[0]?.runId,
    clearCalls[0]?.runId,
    "abortRun and clearRunContext must be called with the same runId",
  );

  const abortIndex = calls.findIndex((c) => c.method === "abortRun");
  const clearIndex = calls.findIndex((c) => c.method === "clearRunContext");
  assert.ok(abortIndex < clearIndex, "abortRun must fire before clearRunContext (mirrors external/core.ts)");

  supervisor.dispose();
});

// F30 regression test: tick() must execute claimed runs with bounded
// concurrency, not a strictly-serial for...of loop. A slow run must not
// head-of-line-block a fast run claimed in the same batch.
//
// WHY: the old tick() awaited runViaChild for each run in sequence, so a
// worst-case batch (N runs each near the timeout) fully serialized. Two
// different owners get two different children (per-user keying is the
// production default), so nothing prevents them from running concurrently —
// except the old for...of loop's own sequencing.
test("F30: tick() runs a batch with bounded concurrency — a fast run reports before a slow run claimed earlier finishes", async () => {
  // Deferred resolver for the slow run's child reply — the test controls
  // exactly when it "finishes" so it can assert the fast run already
  // completed while the slow one was still in flight.
  let releaseSlow: (() => void) | undefined;
  const slowReleased = new Promise<void>((resolve) => {
    releaseSlow = resolve;
  });

  // Default test config keys all identities to the same "single" child, so
  // both claimed runs are dispatched to the SAME fake child — the child
  // dispatches its reply based on the prompt text, not the runId, to
  // simulate two independent in-flight runs multiplexed on one link (which
  // is exactly what supervisor.run()'s runId-scoped event routing supports).
  const [parentSide, childSide] = makePairedChannel();
  const parent = new ParentLink(parentSide);
  parent.onExit = () => {};
  parent.kill = () => {};
  const child = new ChildLink(childSide);
  child.on("prompt", (msg: PromptMessage) => {
    if (msg.text === "slow task") {
      slowReleased.then(() => {
        child.send({ kind: "done", runId: msg.runId } as DoneEvent);
      });
    } else {
      setImmediate(() => {
        child.send({ kind: "done", runId: msg.runId } as DoneEvent);
      });
    }
  });

  const supervisor = new ChildSupervisor(
    makeFakeProxy(),
    makeFakeDeps(),
    (_identity: RunIdentity, _approver: Approver) => ({
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
    (() => parent) as SpawnChildFn,
    makeFakeCreds(),
  );

  const reportedOrder: string[] = [];
  const south = {
    // Slow run claimed FIRST (matches the old for...of head-of-line-blocking
    // scenario: if runs still executed serially, "fast" could never report
    // before "slow" resolves).
    claimDueScheduledRuns: () =>
      Promise.resolve({
        runs: [
          {
            id: "run-slow",
            ownerUserId: "slow@example.com",
            prompt: "slow task",
            approvedTools: [],
            ownerGrant: "v1.tenant.owner.expires.sig",
          },
          {
            id: "run-fast",
            ownerUserId: "fast@example.com",
            prompt: "fast task",
            approvedTools: [],
            ownerGrant: "v1.tenant.owner.expires.sig",
          },
        ],
      }),
    reportScheduledRunResult: (args: { id: string }) => {
      reportedOrder.push(args.id);
      return Promise.resolve();
    },
  };
  const clients = { south } as unknown as BrokerClients;

  const tickPromise = tick(baseCfg, clients, log, supervisor);

  // Give the fast run's microtask/setImmediate chain a chance to fully
  // complete while the slow run is deliberately still unresolved.
  for (let i = 0; i < 20; i++) {
    await new Promise((r) => setImmediate(r));
  }

  assert.ok(
    reportedOrder.includes("run-fast"),
    "the fast run must report its result while the slow run (claimed first) is still in flight — proves concurrency, not strict serial ordering",
  );
  assert.ok(
    !reportedOrder.includes("run-slow"),
    "the slow run must not have reported yet — it is still deliberately unresolved",
  );

  // tick() as a whole must not resolve until every claimed run (including
  // the still-pending slow one) has finished — this is what lets
  // startScheduler's self-rescheduling timer keep ticks non-overlapping:
  // the next tick is only scheduled after this promise settles.
  let tickSettled = false;
  void tickPromise.then(() => {
    tickSettled = true;
  });
  await new Promise((r) => setImmediate(r));
  assert.equal(tickSettled, false, "tick() must not resolve while the slow run is still pending");

  releaseSlow?.();
  await tickPromise;

  assert.deepEqual(
    reportedOrder,
    ["run-fast", "run-slow"],
    "both runs must eventually report, fast before the manually-released slow run",
  );

  supervisor.dispose();
});
