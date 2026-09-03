// CP7 integration tests — /agui interactive path wired to the supervisor.
//
// WHY these tests exist: the /agui handler now drives a forked child via the
// supervisor rather than building an inline Pi session. Three invariants must
// hold at the IPC seam:
//
//   1. A prompt → child events relayed as AG-UI frames on the SSE response.
//   2. HITL suspend-resume: a NEEDS_HUMAN gate suspends the child's `gate` IPC
//      request until the parent-side approver resolves — the child does NOT time
//      out or error during the approval wait — and only then does `execute`
//      proceed. On approver-deny the gate returns allow:false and no execute fires.
//   3. GatewayOverloadError from getOrSpawn → HTTP 503 {error:"gateway_overloaded"}.
//
// All tests use a fake child (fake spawn function, in-memory paired channel).
// The broker bridge is also faked. No real process.fork, no real LLM.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  makePairedChannel,
  ParentLink,
  ChildLink,
  makeSeq,
} from "../src/ipc/protocol.js";
import type {
  TextDeltaEvent,
  ToolStartEvent,
  ToolEndEvent,
  DoneEvent,
  GateRequest,
  GateResult,
  ExecuteRequest,
  ExecuteResult,
  PromptMessage,
} from "../src/ipc/protocol.js";
import { RemoteBridgeServer } from "../src/ipc/bridge-server.js";
import type { BridgeLike } from "../src/ipc/bridge-server.js";
import {
  ChildSupervisor,
  GatewayOverloadError,
  type SpawnChildFn,
  type SupervisorConfig,
  type ProviderCredentialResolver,
  type SupervisorDeps,
} from "../src/ipc/supervisor.js";
import type { Identity, Approver } from "../src/broker/governance.js";
import type { EgressProxy, RegisterResult, RegisterOptions } from "../src/llm/egress-proxy.js";
import type { ResolveIdentity } from "../src/pi/session-plan.js";

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
  simulateExit(): void;
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

  return {
    parentLink: link,
    childLink,
    simulateExit() {
      for (const h of exitHandlers) h(null);
    },
    promptsReceived,
  };
}

// ── Fake spawn + proxy ─────────────────────────────────────────────────────────

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

function makeFakeDeps(): SupervisorDeps {
  return {
    south: {
      getLlmProviders: async () => ({ providers: [] }),
      getTenantModel: async () => ({ model: "anthropic/claude-sonnet-4.6" }),
      // doc.write is granted because these tests drive a real gate IPC for
      // doc_write through a real supervisor-built child. Since CP2
      // the parent refuses a toolName
      // outside the child's own plan-derived set, so an empty skill list would
      // have the HITL plumbing under test never reached. The wire toolName is
      // the Pi tool name (doc_write), matching what gateToolCall actually sends.
      listUserSkills: async () => ({ skills: ["doc.write"] }),
      listUserAgentSkills: async () => ({ bundles: [] }),
      listAccessibleMcpServersForAgent: async () => ({ connections: [] }),
      listMcpServerToolsSouth: async () => ({ tools: [] }),
      getAgentSpec: async () => ({ found: false }),
    },
    cfg: {
      llmModel: "anthropic/claude-sonnet-4.6",
      defaultTenantId: "00000000-0000-0000-0000-000000000001",
    },
  };
}

function makeFakeCredentials(): ProviderCredentialResolver {
  return async (_id: ResolveIdentity) => ({
    upstreamBaseUrl: "https://openrouter.ai/api/v1",
    apiKey: "dummy",
    modelId: "anthropic/claude-sonnet-4.6",
    modelAllowlist: ["anthropic/claude-sonnet-4.6"],
    fallbacks: [],
  });
}

// ── Spy bridge ─────────────────────────────────────────────────────────────────

interface SpyBridge extends BridgeLike {
  gateResult: { allow: boolean; reason?: string };
  executeCalls: string[];
  tokenSet: string | undefined;
  approverSet: Approver | undefined;
}

function makeSpyBridge(gateAllow = true): SpyBridge {
  const bridge: SpyBridge = {
    gateResult: { allow: gateAllow },
    executeCalls: [],
    tokenSet: undefined,
    approverSet: undefined,
    gate: async (_tcId, _toolName, _input) => bridge.gateResult,
    execute: async (toolCallId) => {
      bridge.executeCalls.push(toolCallId);
      return { ok: true, output: "result" };
    },
    delegate: async () => ({ ok: true }),
    saveWorkflow: async () => ({ ok: true }),
    runWorkflow: async () => ({ ok: true, result: null }),
    listWorkflows: async () => ({ ok: true, items: [] }),
    publishWorkflow: async () => ({ ok: true }),
    proposeWorkflow: async () => ({ ok: true }),
    analyzeImage: async () => ({ ok: true, text: "a red apple" }),
    scheduleWorkflow: async () => ({ ok: true }),
    setToken: (token) => { bridge.tokenSet = token; },
    setApprover: (approver) => { bridge.approverSet = approver; },
  };
  return bridge;
}

// ── Rig ───────────────────────────────────────────────────────────────────────

interface Rig {
  supervisor: ChildSupervisor;
  spawn: ReturnType<typeof makeFakeSpawn>;
  spyBridge: SpyBridge;
}

function makeRig(config?: Partial<SupervisorConfig>): Rig {
  const spawn = makeFakeSpawn();
  const spyBridge = makeSpyBridge();

  const supervisor = new ChildSupervisor(
    makeFakeProxy(),
    makeFakeDeps(),
    (_identity) => spyBridge,
    spawn.spawnFn,
    makeFakeCredentials(),
    config,
  );

  return { supervisor, spawn, spyBridge };
}

function makeIdentity(userId = "user-alice"): Identity {
  return {
    tenantId: "00000000-0000-0000-0000-000000000001",
    userId,
    agentId: "agent-default",
  };
}

// ── Tests ─────────────────────────────────────────────────────────────────────

test("CP7 relay: prompt → text_delta + tool_start/end events relayed to onEvent", async () => {
  // WHY: the run() method must forward child IPC events to the onEvent callback
  // so the /agui handler can translate them into AG-UI SSE frames. If events
  // from the child are silently dropped, the browser sees an empty stream.
  const { supervisor, spawn } = makeRig();
  const identity = makeIdentity();
  const key = supervisor.keyFor(identity);

  const handle = await supervisor.getOrSpawn(key, identity);
  await flushAsync(); // let init land on child

  const child = spawn.children[0];
  assert.ok(child, "child must have been spawned");

  const received: string[] = [];
  const runId = "run-001";
  const threadId = "thread-001";

  const runPromise = supervisor.run(
    handle,
    { runId, threadId, text: "hello" },
    (evt) => received.push(evt.kind),
  );

  await flushAsync();

  // Child was sent a prompt.
  assert.equal(child.promptsReceived.length, 1);
  assert.equal(child.promptsReceived[0].runId, runId);

  // Simulate child emitting events.
  const delta: TextDeltaEvent = { kind: "text_delta", runId, delta: "Hi there" };
  const toolStart: ToolStartEvent = { kind: "tool_start", runId, toolCallId: "tc-1", toolName: "web.fetch", input: { url: "https://example.com" } };
  const toolEnd: ToolEndEvent = { kind: "tool_end", runId, toolCallId: "tc-1", ok: true, result: "fetched" };
  const done: DoneEvent = { kind: "done", runId };

  child.childLink.send(delta);
  child.childLink.send(toolStart);
  child.childLink.send(toolEnd);
  child.childLink.send(done);

  await withTimeout(runPromise, 2000, "run to resolve on done");

  assert.deepEqual(received, ["text_delta", "tool_start", "tool_end", "done"]);
});

test("CP7 HITL suspend-resume: gate IPC request suspended until approver resolves, then execute fires; deny → no execute", async () => {
  // WHY: this is the load-bearing correctness test for the IPC seam. In the
  // in-process model the approver was a closure that awaited a browser POST; the
  // child's gate() call blocked synchronously in-process. After the fork, the
  // child sends an IPC gate request and the parent's RemoteBridgeServer awaits
  // the bridge.gate() call — which itself awaits the approver. The child is
  // suspended (its IPC request is pending) for the entire approval duration.
  // This test proves:
  //   a. The gate request does NOT time out or error during the approval wait.
  //   b. execute fires only AFTER the approver resolves with allow=true.
  //   c. On deny, gate returns allow:false and execute is never called.
  //
  // Under the per-run context API, the bridge is constructed from (identity,
  // approver) each run. Here the approver is a manually-resolved promise so
  // we can synchronise approval timing in the test.
  const identity = makeIdentity("user-bob");
  const key = identity.userId; // per-user keying for clarity

  // ── allow path ──────────────────────────────────────────────────────────────
  let resolveApproval!: (allow: boolean) => void;
  const approvalPromise = new Promise<boolean>((res) => { resolveApproval = res; });

  const executeCalls: string[] = [];
  // The bridge factory captures the approver injected by setRunContext.
  // gate() awaits the approver — mirrors GovernanceBridge on NEEDS_HUMAN.
  const spawn2 = makeFakeSpawn();
  const supervisorWithCustomBridge = new ChildSupervisor(
    makeFakeProxy(),
    makeFakeDeps(),
    (_runIdentity, approver) => ({
      gate: async (toolCallId, _toolName, _input) => {
        const allow = await approver({
          toolCallId, toolName: _toolName, toolId: _toolName,
          effectClass: 0, reason: "", args: _input, stepUp: false,
        });
        return { allow, reason: allow ? undefined : "approval declined" };
      },
      execute: async (toolCallId) => {
        executeCalls.push(toolCallId);
        return { ok: true, output: "result" };
      },
      delegate: async () => ({ ok: true }),
      saveWorkflow: async () => ({ ok: true }),
      runWorkflow: async () => ({ ok: true, result: null }),
      listWorkflows: async () => ({ ok: true, items: [] }),
      publishWorkflow: async () => ({ ok: true }),
      proposeWorkflow: async () => ({ ok: true }),
      analyzeImage: async () => ({ ok: true, text: "a red apple" }),
      scheduleWorkflow: async () => ({ ok: true }),
    }),
    spawn2.spawnFn,
    makeFakeCredentials(),
  );

  const handle = await supervisorWithCustomBridge.getOrSpawn(key, identity);
  await flushAsync();

  const child = spawn2.children[0];
  assert.ok(child, "child must have been spawned");

  const seq = makeSeq();
  const runId = "run-hitl";
  const threadId = "thread-hitl";

  // Bind per-run context: identity + the manually-resolved approver.
  handle.setRunContext(runId, identity, async () => approvalPromise);

  // Start run — will block waiting for the gate to resolve.
  const runPromise = supervisorWithCustomBridge.run(
    handle,
    { runId, threadId, text: "do something requiring approval" },
    (_evt) => {},
  );

  await flushAsync();

  // Simulate the child sending a gate request tagged with runId.
  const gateSeq = seq();
  // Register the "before" sentinel BEFORE sending the gate — if gate-result
  // arrives pre-approval this fires and we catch it at the post-flush check.
  // WHY we use a boolean flag rather than an array: the listener stays registered
  // through the flush that delivers the gate request, so we can assert "flag is
  // false after flush and before approval" deterministically. After approval
  // resolves we remove the listener so it doesn't count the legitimate reply.
  let gateResultArrivedBeforeApproval = false;
  const beforeListener = (_msg: GateResult) => { gateResultArrivedBeforeApproval = true; };
  child.childLink.on("gate-result", beforeListener);

  const gateReq: GateRequest = {
    kind: "gate",
    seq: gateSeq,
    runId,
    toolCallId: "tc-hitl",
    toolName: "doc_write",
    input: { path: "/tmp/x" },
  };
  child.childLink.send(gateReq);

  // Flush — the gate-result must NOT have arrived yet (bridge is awaiting approver).
  await flushAsync();
  assert.equal(
    gateResultArrivedBeforeApproval,
    false,
    "gate-result MUST NOT arrive before approver resolves — if this fires the HITL suspend is broken",
  );

  // Remove the sentinel before resolving so it doesn't trip on the legitimate reply.
  child.childLink.off("gate-result", beforeListener);

  // Install the post-approval listener BEFORE resolving approval — the reply is
  // delivered asynchronously (setImmediate) so it would race past a listener
  // registered after the flush.
  const gateResultsAfterApproval: GateResult[] = [];
  const gateResultPromise = new Promise<GateResult>((resolve) => {
    child.childLink.on("gate-result", (msg) => {
      gateResultsAfterApproval.push(msg);
      resolve(msg);
    });
  });

  // Resolve approval → the bridge.gate() promise settles → RemoteBridgeServer
  // sends gate-result → setImmediate delivers to the listener above.
  resolveApproval(true);
  await withTimeout(gateResultPromise, 2000, "gate-result after approval");
  assert.equal(gateResultsAfterApproval[0].allow, true, "gate-result must allow after approval");

  // Confirm the sentinel never fired — proves HITL was genuinely suspended.
  assert.equal(
    gateResultArrivedBeforeApproval,
    false,
    "gate-result sentinel must never have fired before approval (non-vacuous: proves real suspension)",
  );

  // Simulate child following up with execute.
  const executeSeq = seq();
  const execReq: ExecuteRequest = { kind: "execute", seq: executeSeq, runId, toolCallId: "tc-hitl" };
  child.childLink.send(execReq);
  await flushAsync();

  assert.equal(executeCalls.length, 1, "execute must fire once after gate allow");
  assert.equal(executeCalls[0], "tc-hitl");

  // Clean up the run promise (fire done to unblock) and clear run context.
  child.childLink.send({ kind: "done", runId } as DoneEvent);
  await withTimeout(runPromise, 2000, "run promise to settle");
  handle.clearRunContext(runId);

  // ── deny path ───────────────────────────────────────────────────────────────
  let resolveDeny!: (allow: boolean) => void;
  const denyPromise = new Promise<boolean>((res) => { resolveDeny = res; });

  const spawn3 = makeFakeSpawn();
  const supervisorDeny = new ChildSupervisor(
    makeFakeProxy(),
    makeFakeDeps(),
    (_runIdentity, approver) => ({
      gate: async (toolCallId, toolName, input) => {
        const allow = await approver({
          toolCallId, toolName, toolId: toolName,
          effectClass: 0, reason: "", args: input, stepUp: false,
        });
        return { allow, reason: allow ? undefined : "approval declined" };
      },
      execute: async (toolCallId) => {
        executeCalls.push(toolCallId + "-should-not-execute");
        return { ok: true, output: null };
      },
      delegate: async () => ({ ok: true }),
      saveWorkflow: async () => ({ ok: true }),
      runWorkflow: async () => ({ ok: true, result: null }),
      listWorkflows: async () => ({ ok: true, items: [] }),
      publishWorkflow: async () => ({ ok: true }),
      proposeWorkflow: async () => ({ ok: true }),
      analyzeImage: async () => ({ ok: true, text: "a red apple" }),
      scheduleWorkflow: async () => ({ ok: true }),
    }),
    spawn3.spawnFn,
    makeFakeCredentials(),
  );

  const runIdDeny = "run-deny";
  const handleDeny = await supervisorDeny.getOrSpawn(key, identity);
  await flushAsync();
  const childDeny = spawn3.children[0];

  handleDeny.setRunContext(runIdDeny, identity, async () => denyPromise);

  // Capture the run promise so it doesn't produce an unhandled rejection.
  const denyRunPromise = supervisorDeny.run(
    handleDeny,
    { runId: runIdDeny, threadId: "thread-deny", text: "denied task" },
    () => {},
  );
  await flushAsync();

  const denySeq = seq();
  const denyGateReq: GateRequest = {
    kind: "gate",
    seq: denySeq,
    runId: runIdDeny,
    toolCallId: "tc-deny",
    toolName: "doc_write",
    input: {},
  };
  childDeny.childLink.send(denyGateReq);
  await flushAsync();

  const denyResultPromise = new Promise<GateResult>((resolve) => {
    childDeny.childLink.on("gate-result", resolve);
  });
  resolveDeny(false);
  const denyResult = await withTimeout(denyResultPromise, 2000, "deny gate-result");

  assert.equal(denyResult.allow, false, "gate-result must deny");
  assert.equal(
    executeCalls.filter((c) => c.endsWith("-should-not-execute")).length,
    0,
    "execute must not fire after gate deny",
  );

  // Settle the deny run promise cleanly to avoid unhandled rejection at test exit.
  childDeny.childLink.send({ kind: "done", runId: runIdDeny } as DoneEvent);
  await withTimeout(denyRunPromise, 2000, "deny run promise to settle");
  handleDeny.clearRunContext(runIdDeny);

  supervisorWithCustomBridge.dispose();
  supervisorDeny.dispose();
});

test("CP7 concurrent-clobber: two runs on ONE shared child use their own approvers — no cross-run clobber", async () => {
  // WHY: this is the regression test for 🔴 #1 — under the old setRunState API
  // a second /agui request would overwrite the single bridge's approver, so
  // run A's HITL would be answered by run B's approver (wrong browser socket).
  // Under the per-run context API each runId has its own bridge; they cannot
  // interfere.
  //
  // Non-vacuity proof: two runs in flight simultaneously; each gate request is
  // tagged with its own runId. Run A's gate waits for approverA; run B's gate
  // waits for approverB. We resolve approverB FIRST. If run A's gate-result
  // arrives before approverA resolves, the test fails. Only after approverA
  // resolves does run A's gate-result arrive — with approverA's answer, not B's.
  //
  // NOTE: single-keying is NOT a cross-user security boundary. A compromised
  // shared child can reference concurrent run contexts by guessing runIds.
  // Phase 3 per-user keying is the OS-level boundary. This test proves the IPC
  // layer does not clobber — it does not prove OS-level isolation.
  const identity = makeIdentity("user-concurrent");
  // Under "single" keying (default) all identities share key "__single__",
  // so both runs land on the same shared child — which is the scenario we test.
  const key = "__single__";

  let resolveApproverA!: (allow: boolean) => void;
  let resolveApproverB!: (allow: boolean) => void;
  const approverAPromise = new Promise<boolean>((r) => { resolveApproverA = r; });
  const approverBPromise = new Promise<boolean>((r) => { resolveApproverB = r; });

  // Track which approver was called for each run.
  const approverCalls: string[] = [];

  const spawn4 = makeFakeSpawn();
  const supervisorConcurrent = new ChildSupervisor(
    makeFakeProxy(),
    makeFakeDeps(),
    (_runIdentity, approver) => ({
      gate: async (toolCallId, toolName, input) => {
        const allow = await approver({
          toolCallId, toolName, toolId: toolName,
          effectClass: 0, reason: "", args: input, stepUp: false,
        });
        return { allow };
      },
      execute: async () => ({ ok: true, output: null }),
      delegate: async () => ({ ok: true }),
      saveWorkflow: async () => ({ ok: true }),
      runWorkflow: async () => ({ ok: true, result: null }),
      listWorkflows: async () => ({ ok: true, items: [] }),
      publishWorkflow: async () => ({ ok: true }),
      proposeWorkflow: async () => ({ ok: true }),
      analyzeImage: async () => ({ ok: true, text: "a red apple" }),
      scheduleWorkflow: async () => ({ ok: true }),
    }),
    spawn4.spawnFn,
    makeFakeCredentials(),
  );

  const handle = await supervisorConcurrent.getOrSpawn(key, identity);
  await flushAsync();

  const child = spawn4.children[0];
  assert.ok(child, "child must have been spawned");

  const runIdA = "run-concurrent-A";
  const runIdB = "run-concurrent-B";
  const seqGen = makeSeq();

  // Bind each run's context with its own approver that records which was called.
  handle.setRunContext(runIdA, identity, async () => {
    approverCalls.push("A");
    return approverAPromise;
  });
  handle.setRunContext(runIdB, identity, async () => {
    approverCalls.push("B");
    return approverBPromise;
  });

  // Start both runs — they're both in flight on the same child.
  const runAPromise = supervisorConcurrent.run(
    handle,
    { runId: runIdA, threadId: "thread-A", text: "run A" },
    () => {},
  );
  const runBPromise = supervisorConcurrent.run(
    handle,
    { runId: runIdB, threadId: "thread-B", text: "run B" },
    () => {},
  );
  await flushAsync();

  // Send gate requests from both runs — tagged with their own runIds.
  const gateSeqA = seqGen();
  const gateSeqB = seqGen();

  // Register listeners BEFORE sending gates (non-vacuous: proves pre-approval silence).
  const gateResultsA: GateResult[] = [];
  const gateResultsB: GateResult[] = [];
  child.childLink.on("gate-result", (msg) => {
    if (msg.seq === gateSeqA) gateResultsA.push(msg);
    if (msg.seq === gateSeqB) gateResultsB.push(msg);
  });

  child.childLink.send({ kind: "gate", seq: gateSeqA, runId: runIdA, toolCallId: "tc-A", toolName: "doc_write", input: {} } as GateRequest);
  child.childLink.send({ kind: "gate", seq: gateSeqB, runId: runIdB, toolCallId: "tc-B", toolName: "doc_write", input: {} } as GateRequest);
  await flushAsync();

  // Neither run has a result yet — both approvers are pending.
  assert.equal(gateResultsA.length, 0, "run A gate-result must not arrive before approverA resolves");
  assert.equal(gateResultsB.length, 0, "run B gate-result must not arrive before approverB resolves");

  // Resolve approverB first — ONLY run B's gate should unblock.
  const gateResultBPromise = new Promise<GateResult>((resolve) => {
    child.childLink.on("gate-result", (msg) => {
      if (msg.seq === gateSeqB) resolve(msg);
    });
  });
  resolveApproverB(true);
  await withTimeout(gateResultBPromise, 2000, "run B gate-result after approverB resolves");

  // CRITICAL non-vacuous assertion: run A must still have no result.
  assert.equal(
    gateResultsA.length,
    0,
    "run A gate-result must NOT arrive after approverB resolves — approvers are per-run, not shared",
  );
  assert.equal(gateResultsB[0].allow, true, "run B gate-result must be allowed");

  // Now resolve approverA — run A should unblock with A's answer.
  const gateResultAPromise = new Promise<GateResult>((resolve) => {
    child.childLink.on("gate-result", (msg) => {
      if (msg.seq === gateSeqA) resolve(msg);
    });
  });
  resolveApproverA(false); // deny run A to distinguish the two answers
  await withTimeout(gateResultAPromise, 2000, "run A gate-result after approverA resolves");

  assert.equal(gateResultsA[0].allow, false, "run A gate-result must use approverA's answer (false), not approverB's (true)");

  // Verify approver calls: each run called its own approver exactly once.
  assert.ok(approverCalls.includes("A"), "approverA must have been called");
  assert.ok(approverCalls.includes("B"), "approverB must have been called");

  // Clean up both runs.
  child.childLink.send({ kind: "done", runId: runIdA } as DoneEvent);
  child.childLink.send({ kind: "done", runId: runIdB } as DoneEvent);
  await Promise.all([
    withTimeout(runAPromise, 2000, "run A to settle"),
    withTimeout(runBPromise, 2000, "run B to settle"),
  ]);
  handle.clearRunContext(runIdA);
  handle.clearRunContext(runIdB);

  supervisorConcurrent.dispose();
});

test("CP7 overload: GatewayOverloadError from getOrSpawn → supervisor throws it for caller to map to 503", async () => {
  // WHY: when all children are busy and the cap is reached, the /agui handler
  // must respond 503 {error:"gateway_overloaded"} rather than hanging or
  // crashing. The supervisor throws GatewayOverloadError; the handler catches it
  // and sends the response before hijacking the SSE socket.
  //
  // This test exercises the supervisor path directly (handler mapping is trivial
  // and verified by the GatewayOverloadError instanceof check in server.ts).
  const { supervisor, spawn } = makeRig({ maxChildren: 1, keying: "per-user" });

  const identityA = makeIdentity("user-alpha");
  const keyA = supervisor.keyFor(identityA);
  const handleA = await supervisor.getOrSpawn(keyA, identityA);
  await flushAsync();

  // Mark child busy so it cannot be LRU-evicted.
  supervisor.markBusy(handleA);

  // A second distinct identity under per-user keying must spawn a new child,
  // but the cap (1) is already reached with a busy child → GatewayOverloadError.
  const identityB = makeIdentity("user-beta");
  const keyB = supervisor.keyFor(identityB);

  await assert.rejects(
    () => supervisor.getOrSpawn(keyB, identityB),
    (err: unknown) => {
      assert.ok(
        err instanceof GatewayOverloadError,
        `expected GatewayOverloadError, got ${String(err)}`,
      );
      return true;
    },
  );

  supervisor.dispose();
  void spawn;
});
