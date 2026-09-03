// CP2 — the parent refuses an IPC tool
// call naming a tool the session was never given.
//
// WHY these tests exist: the depth-1 subagent cap was documented as "enforced
// structurally" but was only ever enforced by OMITTING spawn_subagents from the
// tool schema handed to a branch child. Nothing parent-side refused the call if
// the child issued the IPC message anyway, and FGA cannot help — a branch runs
// under the caller's own real identity, so CheckFGA(user, can_invoke,
// skill:subagents) passes at depth 1 exactly as it did at depth 0. The result
// was unbounded recursive fan-out: a fork bomb against the child pool plus LLM
// cost amplification.
//
// The backstop stores the session plan's allowedToolNames with the child at
// fork time and checks every toolName-carrying IPC op against it. It is
// defence-in-depth, NOT the primary authorization gate (that stays FGA/OPA/
// Biscuit at the broker) — hence the deliberate fail-open on an empty set,
// pinned by test 3 below.
import { test } from "node:test";
import assert from "node:assert/strict";
import { makePairedChannel, makeSeq, ParentLink, ChildLink } from "../src/ipc/protocol.js";
import type {
  GateRequest,
  GateResult,
  IpcMessage,
  SpawnSubagentsRequest,
  SpawnSubagentsResult,
} from "../src/ipc/protocol.js";
import { TOOL_NAMES } from "../src/pi/tools.js";
import { RemoteBridgeServer } from "../src/ipc/bridge-server.js";
import type { BridgeLike } from "../src/ipc/bridge-server.js";
import {
  ChildSupervisor,
  ephemeralKey,
  type ChildHandle,
  type SpawnChildFn,
  type ProviderCredentials,
  type ProviderCredentialResolver,
  type ProviderTarget,
  type SupervisorDeps,
} from "../src/ipc/supervisor.js";
import type { Identity, Approver } from "../src/broker/governance.js";
import type { EgressProxy, RegisterResult } from "../src/llm/egress-proxy.js";
import type { ResolveIdentity } from "../src/pi/session-plan.js";

const TEST_RUN_ID = "test-run-cp2-membership";

function withTimeout<T>(p: Promise<T>, ms: number, label: string): Promise<T> {
  return Promise.race([
    p,
    new Promise<never>((_, reject) =>
      setTimeout(() => reject(new Error(`timeout: ${label}`)), ms),
    ),
  ]);
}

// ── Spy bridge ─────────────────────────────────────────────────────────────────

interface SpyBridge extends BridgeLike {
  gateCalls: Array<{ toolCallId: string; toolName: string }>;
  spawnCalls: number;
  /** Every bridge method the server reached, in order — a refused op must add nothing. */
  calls: string[];
}

function makeSpyBridge(): SpyBridge {
  const spy: SpyBridge = {
    gateCalls: [],
    spawnCalls: 0,
    calls: [],
    gate(toolCallId: string, toolName: string) {
      spy.calls.push("gate");
      spy.gateCalls.push({ toolCallId, toolName });
      return Promise.resolve({ allow: true });
    },
    execute: async () => { spy.calls.push("execute"); return { ok: true, output: "result" }; },
    delegate: async () => { spy.calls.push("delegate"); return { ok: true }; },
    saveWorkflow: async () => { spy.calls.push("saveWorkflow"); return { ok: true }; },
    runWorkflow: async () => { spy.calls.push("runWorkflow"); return { ok: true, result: null }; },
    listWorkflows: async () => { spy.calls.push("listWorkflows"); return { ok: true, items: [] }; },
    publishWorkflow: async () => { spy.calls.push("publishWorkflow"); return { ok: true }; },
    proposeWorkflow: async () => { spy.calls.push("proposeWorkflow"); return { ok: true }; },
    analyzeImage: async () => { spy.calls.push("analyzeImage"); return { ok: true, text: "a red apple" }; },
    scheduleWorkflow: async () => { spy.calls.push("scheduleWorkflow"); return { ok: true }; },
    spawnSubagents() {
      spy.calls.push("spawnSubagents");
      spy.spawnCalls += 1;
      return Promise.resolve({ ok: true, branches: [], synthesis: "done" });
    },
  };
  return spy;
}

/** RemoteBridgeServer wired to a child-side link, with a pre-seeded run context
 *  and the given stored tool set. */
function makeWiredPair(
  bridge: BridgeLike,
  allowedToolNames?: string[],
): { server: RemoteBridgeServer; child: ChildLink } {
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const server = new RemoteBridgeServer(parentLink, () => bridge, allowedToolNames);
  server.setRunContext(TEST_RUN_ID, { tenantId: "t1", userId: "u1", agentId: "a1" }, async () => true);
  return { server, child: new ChildLink(childSide) };
}

function gateReq(seq: number, toolName: string): GateRequest {
  return {
    kind: "gate",
    seq,
    runId: TEST_RUN_ID,
    toolCallId: `tc-${toolName}`,
    toolName,
    input: {},
  };
}

// ── 1. Denied: a tool outside the stored set ──────────────────────────────────

test("CP2 membership: a gate for spawn_subagents on a child whose stored set omits it is denied, no broker RPC issued", async () => {
  // WHY: this is the fork-bomb path. A compromised branch child issues the IPC
  // message its schema never advertised; the parent must refuse before the
  // bridge (and therefore SubmitPlan/CheckFGA) is ever reached.
  const spy = makeSpyBridge();
  const { child } = makeWiredPair(spy, ["web_fetch", "delegate"]);

  const seq = makeSeq()();
  const reply = await withTimeout(
    child.request<GateResult>(gateReq(seq, "spawn_subagents"), "gate-result"),
    500,
    "denied gate",
  );

  assert.equal(reply.allow, false, "a tool outside the session's own set must be denied");
  assert.match(String(reply.reason), /spawn_subagents/, "the denial must name the refused tool");
  assert.equal(spy.gateCalls.length, 0, "the bridge must never be called — no broker RPC issued");

  // The child must still be alive and serving: the refusal is a denial reply,
  // not a throw that tears the process down.
  const seq2 = makeSeq()();
  const ok = await withTimeout(
    child.request<GateResult>(gateReq(seq2, "web_fetch"), "gate-result"),
    500,
    "post-denial gate",
  );
  assert.equal(ok.allow, true, "the child must keep working after a refusal");
});

test("CP2 membership: a spawn-subagents op on a child whose stored set omits spawn_subagents is refused", async () => {
  // WHY: spawn-subagents carries no toolName on the wire but IS the
  // spawn_subagents tool. Checking only the `gate` op would leave the whole
  // point of the fix bypassable by skipping the gate round-trip.
  const spy = makeSpyBridge();
  const { child } = makeWiredPair(spy, ["web_fetch", "delegate"]);

  const req: SpawnSubagentsRequest = {
    kind: "spawn-subagents",
    seq: makeSeq()(),
    runId: TEST_RUN_ID,
    branches: [{ task: "recurse" }],
    aggregatorInstruction: "combine",
  };

  const reply = await withTimeout(
    child.request<SpawnSubagentsResult>(req, "spawn-subagents-result"),
    500,
    "denied spawn-subagents",
  );

  assert.equal(reply.ok, false);
  assert.match(String(reply.error), /spawn_subagents/);
  assert.equal(spy.spawnCalls, 0, "the bridge must never fan out");
});

// ── 1b. Every tool-op's hardcoded literal, pinned against TOOL_NAMES ──────────
//
// WHY behavioural rather than by inspection: the ops below carry no toolName on
// the wire, so each names its own with a literal at the registration. A literal
// that does not match what allowedToolNames actually holds fails SILENTLY in
// one of two directions — deny the tool to every legitimate caller, or leave it
// unchecked. Driving each op twice (stored set omitting the name, stored set
// holding exactly the TOOL_NAMES spelling) catches both directions.

interface ToolOp {
  tool: string;
  replyKind: IpcMessage["kind"];
  req: (seq: number) => IpcMessage & { seq: number };
}

const TOOL_OPS: ToolOp[] = [
  { tool: "delegate", replyKind: "delegate-result",
    req: (seq) => ({ kind: "delegate", seq, runId: TEST_RUN_ID, to: "bob@example.com", intent: "triage" }) },
  { tool: "workflow_save", replyKind: "save-workflow-result",
    req: (seq) => ({ kind: "save-workflow", seq, runId: TEST_RUN_ID, def: {} }) },
  { tool: "workflow_run", replyKind: "run-workflow-result",
    req: (seq) => ({ kind: "run-workflow", seq, runId: TEST_RUN_ID, lineageId: "l1", inputs: {} }) },
  { tool: "workflow_list", replyKind: "list-workflows-result",
    req: (seq) => ({ kind: "list-workflows", seq, runId: TEST_RUN_ID }) },
  { tool: "workflow_publish", replyKind: "publish-workflow-result",
    req: (seq) => ({ kind: "publish-workflow", seq, runId: TEST_RUN_ID, lineageId: "l1", groupIds: [], version: 0 }) },
  { tool: "workflow_propose", replyKind: "propose-workflow-result",
    req: (seq) => ({ kind: "propose-workflow", seq, runId: TEST_RUN_ID, lineageId: "l1", def: {} }) },
  { tool: "workflow_schedule", replyKind: "schedule-workflow-result",
    req: (seq) => ({ kind: "schedule-workflow", seq, runId: TEST_RUN_ID, lineageId: "l1", inputs: {}, recurrence: { kind: "once", runAt: "2999-01-01T00:00:00Z" } }) },
  { tool: "analyze_image", replyKind: "analyze-image-result",
    req: (seq) => ({ kind: "analyze-image", seq, runId: TEST_RUN_ID, path: "references/x.png" }) },
  { tool: "spawn_subagents", replyKind: "spawn-subagents-result",
    req: (seq) => ({ kind: "spawn-subagents", seq, runId: TEST_RUN_ID, branches: [{ task: "t" }], aggregatorInstruction: "combine" }) },
];

/** The refusal text of a reply, or undefined when the op was not refused. */
function refusal(reply: IpcMessage): string | undefined {
  if ("allow" in reply) return reply.allow === false ? String(reply.reason ?? "") : undefined;
  if ("ok" in reply && reply.ok === false) return "error" in reply ? String(reply.error ?? "") : "";
  return undefined;
}

test("CP2 membership: every tool-op's hardcoded tool name is a real TOOL_NAMES entry", () => {
  for (const op of TOOL_OPS) {
    assert.ok(TOOL_NAMES.includes(op.tool), `${op.tool} is not a Pi tool name`);
  }
});

test("CP2 membership: every tool-op is refused by its own name when the stored set omits it", async () => {
  for (const op of TOOL_OPS) {
    const spy = makeSpyBridge();
    const { child } = makeWiredPair(spy, ["web_fetch"]);

    const reply = await withTimeout(
      child.request<IpcMessage>(op.req(makeSeq()()), op.replyKind),
      500,
      `denied ${op.tool}`,
    );

    const why = refusal(reply);
    assert.ok(why !== undefined, `${op.tool}: the op must be refused`);
    assert.match(
      why,
      new RegExp(`"${op.tool}"`),
      `${op.tool}: the refusal must name the tool exactly as TOOL_NAMES spells it`,
    );
    assert.deepEqual(spy.calls, [], `${op.tool}: the bridge must never be reached`);
  }
});

test("CP2 membership: every tool-op passes through when the stored set holds its TOOL_NAMES spelling", async () => {
  for (const op of TOOL_OPS) {
    const spy = makeSpyBridge();
    const { child } = makeWiredPair(spy, [op.tool]);

    const reply = await withTimeout(
      child.request<IpcMessage>(op.req(makeSeq()()), op.replyKind),
      500,
      `allowed ${op.tool}`,
    );

    assert.equal(
      refusal(reply),
      undefined,
      `${op.tool}: a granted tool must not be refused — the registration literal has drifted from TOOL_NAMES`,
    );
    assert.equal(spy.calls.length, 1, `${op.tool}: the bridge must be reached exactly once`);
  }
});

// ── 2. Allowed: a tool inside the stored set ──────────────────────────────────

test("CP2 membership: a gate for a tool that IS in the stored set passes through to the bridge unchanged", async () => {
  const spy = makeSpyBridge();
  const { child } = makeWiredPair(spy, ["web_fetch", "spawn_subagents"]);

  const seq = makeSeq()();
  const reply = await withTimeout(
    child.request<GateResult>(gateReq(seq, "spawn_subagents"), "gate-result"),
    500,
    "allowed gate",
  );

  assert.equal(reply.allow, true);
  assert.equal(spy.gateCalls.length, 1, "the bridge must be reached");
  assert.equal(spy.gateCalls[0]?.toolName, "spawn_subagents", "the toolName must arrive unchanged");
});

// ── 3. Fail-open on an empty stored set ───────────────────────────────────────

test("CP2 membership: an empty stored set allows every tool name (deliberate fail-open backstop)", async () => {
  // WHY pinned: sessions that legitimately build no full plan — test harnesses,
  // and any spawn path that constructs a bridge server without one — must not
  // break. This check is defence-in-depth against a compromised child, not the
  // primary authorization gate; that stays FGA/OPA/Biscuit at the broker, which
  // still runs for every one of these calls.
  const spy = makeSpyBridge();
  const { child } = makeWiredPair(spy); // no stored set at all

  for (const name of ["spawn_subagents", "doc_write", "anything_at_all"]) {
    const reply = await withTimeout(
      child.request<GateResult>(gateReq(makeSeq()(), name), "gate-result"),
      500,
      `fail-open gate ${name}`,
    );
    assert.equal(reply.allow, true, `${name} must pass through when no set is stored`);
  }
  assert.equal(spy.gateCalls.length, 3, "every call must reach the bridge");
});

// ── 4. A branch child's stored set omits spawn_subagents ──────────────────────

function makeFakeProxy(): EgressProxy {
  let n = 0;
  return {
    register(): RegisterResult {
      const childToken = `fake-token-${++n}`;
      return { childToken, childBaseUrl: `http://127.0.0.1:9999/${childToken}` };
    },
    resetRunBudget() {},
    consumeLlmBudget() { return true; },
    unregister() {},
    start() { return Promise.resolve(); },
    stop() { return Promise.resolve(); },
    address() { return { address: "127.0.0.1", port: 9999 }; },
  } as unknown as EgressProxy;
}

const fakeCredentials: ProviderCredentialResolver = async (
  _identity: ResolveIdentity,
): Promise<ProviderCredentials> => ({
  upstreamBaseUrl: "https://openrouter.ai/api/v1",
  apiKey: "REAL_API_KEY_NEVER_IN_CHILD",
  modelId: "anthropic/claude-sonnet-4.6",
  modelAllowlist: ["anthropic/claude-sonnet-4.6"],
  fallbacks: [] as ProviderTarget[],
});

// The user genuinely holds skill:subagents — so a pooled child DOES get
// spawn_subagents, and the branch child's omission is the depth cap, not an
// artefact of an ungranted identity.
function makeDeps(): SupervisorDeps {
  return {
    south: {
      getLlmProviders: async () => ({ providers: [] }),
      getTenantModel: async () => ({ model: "anthropic/claude-sonnet-4.6" }),
      listUserSkills: async () => ({ skills: ["subagents", "web.fetch"] }),
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

interface Rig {
  supervisor: ChildSupervisor;
  childLinks: ChildLink[];
  bridge: SpyBridge;
}

function makeRig(): Rig {
  const childLinks: ChildLink[] = [];
  const spawnFn: SpawnChildFn = () => {
    const [parentSide, childSide] = makePairedChannel();
    const link = new ParentLink(parentSide);
    link.onExit = () => {};
    link.offExit = () => {};
    link.kill = () => {};
    childLinks.push(new ChildLink(childSide));
    return link;
  };
  const bridge = makeSpyBridge();
  const supervisor = new ChildSupervisor(
    makeFakeProxy(),
    makeDeps(),
    () => bridge,
    spawnFn,
    fakeCredentials,
  );
  return { supervisor, childLinks, bridge };
}

const IDENTITY: Identity = {
  tenantId: "00000000-0000-0000-0000-000000000001",
  userId: "user-a",
  agentId: "agent-a",
};

const APPROVER: Approver = async () => true;

test("CP2 membership: a subagent branch child's stored set omits spawn_subagents — the depth cap is a property of the stored set, not the model's schema", async () => {
  // WHY this framing: the pre-fix cap lived only in the tool schema handed to
  // the model. A child that ignored its schema was unconstrained. Asserting on
  // the STORED set (by driving a real IPC message through the branch child's
  // own bridge server) is what proves the cap now survives a compromised child.
  const rig = makeRig();

  // Baseline: an ordinary pooled child for the same identity DOES get the tool.
  const pooled = await rig.supervisor.getOrSpawn(rig.supervisor.keyFor(IDENTITY), IDENTITY);
  assert.ok(
    pooled.allowedToolNames.includes("spawn_subagents"),
    "precondition: the user holds skill:subagents, so a pooled child gets the tool",
  );

  const runId = "run-branch-1";
  let branchHandle: ChildHandle | undefined;
  await rig.supervisor.withEphemeralChild(ephemeralKey(runId, 0), IDENTITY, async (handle) => {
    branchHandle = handle;
    handle.setRunContext(TEST_RUN_ID, IDENTITY, APPROVER);

    const branchLink = rig.childLinks[rig.childLinks.length - 1];
    assert.ok(branchLink, "the branch child's link must exist");

    const reply = await withTimeout(
      branchLink.request<GateResult>(gateReq(makeSeq()(), "spawn_subagents"), "gate-result"),
      500,
      "branch gate",
    );
    assert.equal(reply.allow, false, "a branch child must be refused spawn_subagents at the IPC boundary");

    const spawnReply = await withTimeout(
      branchLink.request<SpawnSubagentsResult>(
        {
          kind: "spawn-subagents",
          seq: makeSeq()(),
          runId: TEST_RUN_ID,
          branches: [{ task: "recurse" }],
          aggregatorInstruction: "combine",
        },
        "spawn-subagents-result",
      ),
      500,
      "branch spawn-subagents",
    );
    assert.equal(spawnReply.ok, false, "the bridge-direct op must be refused too");
  });

  assert.ok(branchHandle, "the branch handle must have been observed");
  assert.ok(
    !branchHandle.allowedToolNames.includes("spawn_subagents"),
    "the branch child's stored set must omit spawn_subagents",
  );
  assert.equal(rig.bridge.spawnCalls, 0, "no fan-out may reach the bridge from a branch child");
});
