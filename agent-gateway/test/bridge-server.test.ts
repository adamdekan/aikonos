// CP2 tests: parent-side remote bridge server (SECURITY CORE).
//
// WHY these tests exist: the RemoteBridgeServer is the single enforcement point
// that ensures a forked child cannot escalate privileges by injecting identity
// fields into IPC messages. The bound GovernanceBridge is constructed once from
// the child's *spawn record*; messages carry no identity. Any identity field that
// appears in a message must be silently ignored — the bridge's recorded owner is
// what acts, period.
//
// Secondary concern: a bridge method that throws must never produce an unhandled
// rejection that crashes the parent process — it must surface as a clean *-result
// with ok:false / allow:false + error string.
//
// All tests use the fake in-memory paired channel + a spy GovernanceBridge.
// No real process.fork, no real broker.
import { test } from "node:test";
import assert from "node:assert/strict";
import { makePairedChannel, makeSeq, ParentLink, ChildLink } from "../src/ipc/protocol.js";
import type {
  GateRequest,
  GateResult,
  ExecuteRequest,
  ExecuteResult,
  DelegateRequest,
  DelegateResult,
  AnalyzeImageRequest,
  AnalyzeImageResult,
  GetSkillBodyRequest,
  GetSkillBodyResult,
  GetSkillFileRequest,
  GetSkillFileResult,
  SpawnSubagentsRequest,
  SpawnSubagentsResult,
} from "../src/ipc/protocol.js";
import { RemoteBridgeServer } from "../src/ipc/bridge-server.js";
import type { BridgeLike } from "../src/ipc/bridge-server.js";

// Fixed runId used by all tests in this file. Tests here focus on IPC routing
// correctness (field mapping, crash-safety, concurrency) rather than per-run
// context isolation — the run-context tests live in agui-supervisor.test.ts.
const TEST_RUN_ID = "test-run-001";

// ── Helpers ────────────────────────────────────────────────────────────────────

function withTimeout<T>(p: Promise<T>, ms: number, label: string): Promise<T> {
  return Promise.race([
    p,
    new Promise<never>((_, reject) =>
      setTimeout(() => reject(new Error(`timeout: ${label}`)), ms),
    ),
  ]);
}

/** Tick the event loop enough times for setImmediate-delivered IPC messages to
 *  propagate AND for async bridge methods to resolve. */
async function flushAsync(): Promise<void> {
  for (let i = 0; i < 5; i++) {
    await new Promise((r) => setImmediate(r));
  }
}

// ── Spy bridge factory ─────────────────────────────────────────────────────────
//
// Returns a BridgeLike spy whose methods record their calls and return
// configurable outcomes. Tests control behaviour by overriding the on* hooks.

interface SpyBridge extends BridgeLike {
  // gateRawArgs records the full arguments list passed to each gate() call.
  // WHY: a 4-param spy that uses named params silently drops a 5th arg — JS
  // never reports it. Capturing ...args lets tests assert the exact arg count
  // and rule out identity injection regardless of parameter list length.
  gateRawArgs: unknown[][];
  gateCalls: Array<{ toolCallId: string; toolName: string; input: Record<string, unknown>; opts?: { readOnlyHint?: boolean } }>;
  executeCalls: Array<{ toolCallId: string }>;
  delegateCalls: Array<{ to: string; intent: string; scopes?: string[] }>;
  onGate: (toolCallId: string, toolName: string, input: Record<string, unknown>, opts?: { readOnlyHint?: boolean }) => Promise<{ allow: boolean; reason?: string }>;
  onExecute: (toolCallId: string) => Promise<{ ok: boolean; output: unknown; error?: string }>;
  onDelegate: (to: string, intent: string, scopes?: string[]) => Promise<{ ok: boolean; envelopeId?: string; error?: string }>;
}

function makeSpyBridge(): SpyBridge {
  const spy: SpyBridge = {
    gateRawArgs: [],
    gateCalls: [],
    executeCalls: [],
    delegateCalls: [],
    onGate: async () => ({ allow: true }),
    onExecute: async () => ({ ok: true, output: "result" }),
    onDelegate: async () => ({ ok: true, envelopeId: "env-1" }),

    gate(...args: unknown[]) {
      spy.gateRawArgs.push(args);
      const [toolCallId, toolName, input, opts] = args as [
        string,
        string,
        Record<string, unknown>,
        { readOnlyHint?: boolean } | undefined,
      ];
      spy.gateCalls.push({ toolCallId, toolName, input, opts });
      return spy.onGate(toolCallId, toolName, input, opts);
    },
    execute(toolCallId) {
      spy.executeCalls.push({ toolCallId });
      return spy.onExecute(toolCallId);
    },
    delegate(to, intent, scopes) {
      spy.delegateCalls.push({ to, intent, scopes });
      return spy.onDelegate(to, intent, scopes);
    },
    saveWorkflow: async () => ({ ok: true }),
    runWorkflow: async () => ({ ok: true, result: null }),
    listWorkflows: async () => ({ ok: true, items: [] }),
    publishWorkflow: async () => ({ ok: true }),
    proposeWorkflow: async () => ({ ok: true }),
    analyzeImage: async () => ({ ok: true, text: "a red apple" }),
    scheduleWorkflow: async () => ({ ok: true }),
  };
  return spy;
}

/** Build a wired pair: a RemoteBridgeServer on the parent side + a ChildLink on
 *  the child side so tests can send requests and read replies.
 *
 *  The bridge is wrapped in a factory that ignores per-run identity/approver
 *  (these tests focus on IPC field routing, not run-context isolation). A run
 *  context is pre-seeded for TEST_RUN_ID so requests arrive with a valid context. */
function makeWiredPair(bridge: BridgeLike): { server: RemoteBridgeServer; child: ChildLink } {
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  // Factory always returns the same spy bridge regardless of per-run identity.
  const server = new RemoteBridgeServer(parentLink, (_identity, _approver) => bridge);
  // Pre-seed a run context so test requests with TEST_RUN_ID are serviced.
  server.setRunContext(TEST_RUN_ID, {
    tenantId: "t1", userId: "u1", agentId: "a1",
  }, async () => true);
  const child = new ChildLink(childSide);
  return { server, child };
}

// ── Tests ─────────────────────────────────────────────────────────────────────

test("CP2 gate: a gate request is routed to the bound bridge with the correct fields", async () => {
  // WHY: the whole point of the bridge server is to proxy IPC requests to the
  // real GovernanceBridge. If field mapping is wrong, the bridge call is wrong.
  const spy = makeSpyBridge();
  const { child } = makeWiredPair(spy);

  const seq = makeSeq()();
  const req: GateRequest = {
    kind: "gate",
    seq,
    runId: TEST_RUN_ID,
    toolCallId: "tc-gate-1",
    toolName: "web_fetch",
    input: { url: "https://example.com" },
    opts: { readOnlyHint: true },
  };

  const replyP = withTimeout(
    child.request<GateResult>(req, "gate-result"),
    500,
    "gate request",
  );

  const reply = await replyP;

  assert.equal(reply.seq, seq, "reply must echo the request seq");
  assert.equal(reply.allow, true);

  assert.equal(spy.gateCalls.length, 1, "bridge.gate must be called exactly once");
  const call = spy.gateCalls[0];
  assert.ok(call, "gate call must be recorded");
  assert.equal(call.toolCallId, "tc-gate-1");
  assert.equal(call.toolName, "web_fetch");
  assert.deepEqual(call.input, { url: "https://example.com" });
  assert.deepEqual(call.opts, { readOnlyHint: true });
});

test("CP2 gate: SECURITY — a message with injected owner/userId fields does not affect the bound bridge identity", async () => {
  // WHY this test is load-bearing: a forged child process could inject an
  // 'owner', 'userId', or 'tenantId' field into a gate message hoping the server
  // reads it and calls the bridge with a different identity. The server must
  // ignore any such field entirely — it calls bridge.gate() only with the
  // toolCallId/toolName/input/opts from the message; the bridge's owner was
  // fixed at construction time from the spawn record.
  //
  // Failure mode if the invariant breaks: a child forges a high-privilege userId
  // in the message and the parent-side bridge uses it to act on their behalf.
  //
  // How we prove it non-vacuously: gateRawArgs captures the *entire* args list
  // passed to bridge.gate(). We assert exactly 4 args arrived. A server that
  // did `bridge.gate(toolCallId, toolName, input, opts, msg.userId)` would pass
  // 5 args — this assertion catches that. We also verify none of the 4 args
  // equals or contains a forged identity value.
  const spy = makeSpyBridge();
  const { child } = makeWiredPair(spy);

  const gen = makeSeq();
  const seq = gen();

  // Inject forged identity fields into the raw message object.
  const forgedMsg: Record<string, unknown> = {
    kind: "gate",
    seq,
    runId: TEST_RUN_ID,
    toolCallId: "tc-forged",
    toolName: "web_fetch",
    input: { url: "https://evil.example.com" },
    // These extra fields must be silently ignored by the server.
    owner: "forged-owner",
    userId: "evil-user-id",
    tenantId: "evil-tenant",
    agentId: "evil-agent",
  };

  // Send via the raw channel — bypass the typed ChildLink.send() so the test
  // can inject fields that the GateRequest type doesn't have.
  const replyP = new Promise<GateResult>((resolve) => {
    child.on("gate-result", resolve);
  });
  // Channel.send accepts unknown — no cast needed.
  child.send(forgedMsg);

  const reply = await withTimeout(replyP, 500, "forged-identity gate reply");

  assert.equal(reply.seq, seq);
  assert.equal(reply.allow, true, "bridge call must succeed using the recorded identity");

  // Verify the bridge was called exactly once.
  assert.equal(spy.gateRawArgs.length, 1, "bridge.gate must be called exactly once");
  const rawArgs = spy.gateRawArgs[0];
  assert.ok(rawArgs, "raw args must be recorded");

  // CRITICAL: exactly 4 positional args — no identity field was passed as a 5th arg.
  // A server that did bridge.gate(toolCallId, toolName, input, opts, msg.userId)
  // would fail this assertion.
  assert.equal(rawArgs.length, 4, `bridge.gate must receive exactly 4 args, got ${rawArgs.length}`);

  // Verify the 4 args are the correct tool-call fields (not forged identity).
  assert.equal(rawArgs[0], "tc-forged", "arg[0] must be toolCallId");
  assert.equal(rawArgs[1], "web_fetch", "arg[1] must be toolName");
  assert.deepEqual(rawArgs[2], { url: "https://evil.example.com" }, "arg[2] must be input");
  assert.equal(rawArgs[3], undefined, "arg[3] must be opts (undefined — not a forged value)");

  // None of the args contains a forged identity value.
  const serialised = JSON.stringify(rawArgs);
  assert.ok(!serialised.includes("forged-owner"), "no arg must contain forged 'owner'");
  assert.ok(!serialised.includes("evil-user-id"), "no arg must contain forged 'userId'");
  assert.ok(!serialised.includes("evil-tenant"), "no arg must contain forged 'tenantId'");
  assert.ok(!serialised.includes("evil-agent"), "no arg must contain forged 'agentId'");
});

test("CP2 gate: bridge returning DENIED → gate-result with allow:false and reason", async () => {
  // WHY: a denied tool call must be surfaced to the child so the Pi loop can
  // report the failure. Swallowing a denial would let the child proceed as if
  // the tool was allowed — a security and correctness bug.
  const spy = makeSpyBridge();
  spy.onGate = async () => ({ allow: false, reason: "denied by policy: no skill grant" });
  const { child } = makeWiredPair(spy);

  const seq = makeSeq()();
  const req: GateRequest = {
    kind: "gate",
    seq,
    runId: TEST_RUN_ID,
    toolCallId: "tc-denied",
    toolName: "web_fetch",
    input: {},
  };

  const reply = await withTimeout(
    child.request<GateResult>(req, "gate-result"),
    500,
    "denied gate",
  );

  assert.equal(reply.allow, false);
  assert.equal(reply.reason, "denied by policy: no skill grant");
  assert.equal(reply.seq, seq);
});

test("CP2 gate: bridge that suspends (NEEDS_HUMAN) is awaited before sending gate-result", async () => {
  // WHY: the IPC model requires the gate-result to be sent ONLY after the bridge
  // fully resolves — including any HITL browser round-trip. The child's gate
  // request remains pending until the approval resolves. If the server sent the
  // reply before the bridge awaited the approver, the child would proceed without
  // a capability token.
  const spy = makeSpyBridge();

  let resolveApproval!: (allow: boolean) => void;
  spy.onGate = () =>
    new Promise<{ allow: boolean; reason?: string }>((resolve) => {
      resolveApproval = (allow) => resolve({ allow });
    });

  const { child } = makeWiredPair(spy);

  const seq = makeSeq()();
  const req: GateRequest = {
    kind: "gate",
    seq,
    runId: TEST_RUN_ID,
    toolCallId: "tc-suspend",
    toolName: "doc_write",
    input: {},
  };

  let replied = false;
  const replyP = child.request<GateResult>(req, "gate-result").then((r) => {
    replied = true;
    return r;
  });

  // Flush — the reply must NOT have arrived yet (bridge is suspended).
  await flushAsync();
  assert.equal(replied, false, "gate-result must not be sent before the bridge resolves");

  // Now resolve the approval.
  resolveApproval(true);
  const reply = await withTimeout(replyP, 500, "suspended gate");

  assert.equal(reply.allow, true);
  assert.equal(replied, true);
});

test("CP2 execute: execute request is routed to bridge.execute and result echoes seq", async () => {
  const spy = makeSpyBridge();
  spy.onExecute = async () => ({ ok: true, output: { data: 42 } });
  const { child } = makeWiredPair(spy);

  const seq = makeSeq()();
  const req: ExecuteRequest = { kind: "execute", seq, runId: TEST_RUN_ID, toolCallId: "tc-exec-1" };

  const reply = await withTimeout(
    child.request<ExecuteResult>(req, "execute-result"),
    500,
    "execute request",
  );

  assert.equal(reply.seq, seq);
  assert.equal(reply.ok, true);
  assert.deepEqual(reply.output, { data: 42 });
  assert.equal(spy.executeCalls.length, 1);
  assert.equal(spy.executeCalls[0]?.toolCallId, "tc-exec-1");
});

test("CP2 execute: bridge.execute returning ok:false is surfaced in execute-result", async () => {
  const spy = makeSpyBridge();
  spy.onExecute = async () => ({ ok: false, output: null, error: "tool invocation failed: 503" });
  const { child } = makeWiredPair(spy);

  const seq = makeSeq()();
  const req: ExecuteRequest = { kind: "execute", seq, runId: TEST_RUN_ID, toolCallId: "tc-exec-fail" };

  const reply = await withTimeout(
    child.request<ExecuteResult>(req, "execute-result"),
    500,
    "execute fail",
  );

  assert.equal(reply.ok, false);
  assert.equal(reply.error, "tool invocation failed: 503");
  assert.equal(reply.seq, seq);
});

test("CP5 analyze-image: analyze-image request is routed to bridge.analyzeImage and result echoes seq", async () => {
  const spy = makeSpyBridge();
  const { child } = makeWiredPair(spy);

  const seq = makeSeq()();
  const req: AnalyzeImageRequest = {
    kind: "analyze-image",
    seq,
    runId: TEST_RUN_ID,
    path: "references/apple.png",
    prompt: "what fruit is this?",
  };

  const reply = await withTimeout(
    child.request<AnalyzeImageResult>(req, "analyze-image-result"),
    500,
    "analyze-image request",
  );

  assert.equal(reply.seq, seq);
  assert.equal(reply.ok, true);
  assert.equal(reply.text, "a red apple");
});

test("CP5 analyze-image: bridge.analyzeImage that THROWS → clean analyze-image-result {ok:false, error}, no unhandled rejection", async () => {
  const spy = makeSpyBridge();
  spy.analyzeImage = async () => {
    throw new Error("no vision provider assigned");
  };
  const { child } = makeWiredPair(spy);

  const seq = makeSeq()();
  const req: AnalyzeImageRequest = { kind: "analyze-image", seq, runId: TEST_RUN_ID, path: "x.png" };

  const reply = await withTimeout(
    child.request<AnalyzeImageResult>(req, "analyze-image-result"),
    500,
    "analyze-image throw",
  );

  assert.equal(reply.ok, false);
  assert.match(reply.error ?? "", /no vision provider assigned/);
  assert.equal(reply.seq, seq);
});

// ── personal skills ────────────────────────

test("CP4 get-skill-body: request is routed to bridge.getSkillBody and result echoes seq", async () => {
  const spy = makeSpyBridge();
  spy.getSkillBody = async (name: string) => ({ ok: true, body: `body of ${name}`, allowedTools: ["web.fetch"] });
  const { child } = makeWiredPair(spy);

  const seq = makeSeq()();
  const req: GetSkillBodyRequest = { kind: "get-skill-body", seq, runId: TEST_RUN_ID, name: "my-notes" };

  const reply = await withTimeout(
    child.request<GetSkillBodyResult>(req, "get-skill-body-result"),
    500,
    "get-skill-body request",
  );

  assert.equal(reply.seq, seq);
  assert.equal(reply.ok, true);
  assert.equal(reply.body, "body of my-notes");
  assert.deepEqual(reply.allowedTools, ["web.fetch"]);
});

test("CP4 get-skill-body: bridge.getSkillBody that THROWS → clean get-skill-body-result {ok:false, error}, no unhandled rejection", async () => {
  const spy = makeSpyBridge();
  spy.getSkillBody = async () => {
    throw new Error("NotFound");
  };
  const { child } = makeWiredPair(spy);

  const seq = makeSeq()();
  const req: GetSkillBodyRequest = { kind: "get-skill-body", seq, runId: TEST_RUN_ID, name: "gone" };

  const reply = await withTimeout(
    child.request<GetSkillBodyResult>(req, "get-skill-body-result"),
    500,
    "get-skill-body throw",
  );

  assert.equal(reply.ok, false);
  assert.match(reply.error ?? "", /NotFound/);
  assert.equal(reply.seq, seq);
});

test("CP4 get-skill-body: a bridge with no getSkillBody method (predates this feature) fails closed, not silently", async () => {
  const spy = makeSpyBridge(); // no getSkillBody override — the property is absent
  const { child } = makeWiredPair(spy);

  const seq = makeSeq()();
  const req: GetSkillBodyRequest = { kind: "get-skill-body", seq, runId: TEST_RUN_ID, name: "my-notes" };

  const reply = await withTimeout(
    child.request<GetSkillBodyResult>(req, "get-skill-body-result"),
    500,
    "get-skill-body unsupported",
  );

  assert.equal(reply.ok, false);
  assert.match(reply.error ?? "", /not supported/);
});

// ── skill-full-tree ─────────────────────────

test("CP3 get-skill-file: request is routed to bridge.getSkillFile and result echoes seq", async () => {
  const spy = makeSpyBridge();
  const content = new TextEncoder().encode("## Guide");
  const contentB64 = Buffer.from(content).toString("base64");
  spy.getSkillFile = async (ref: string, path: string) => {
    assert.equal(ref, "bundle-uuid-1");
    assert.equal(path, "references/guide.md");
    // Round-trip through JSON — mirrors the real child IPC channel
    // (supervisor.ts's defaultSpawnChild forks with serialization:"json",
    // the fake paired channel used by makeWiredPair does not). contentB64
    // (a plain string) must survive this unchanged — the fix for the JSON-
    // boundary corruption bug (bug 1, CP3 review) that a raw Uint8Array
    // field would not have survived.
    return JSON.parse(JSON.stringify({ ok: true, contentB64 }));
  };
  const { child } = makeWiredPair(spy);

  const seq = makeSeq()();
  const req: GetSkillFileRequest = {
    kind: "get-skill-file", seq, runId: TEST_RUN_ID, ref: "bundle-uuid-1", path: "references/guide.md",
  };

  const reply = await withTimeout(
    child.request<GetSkillFileResult>(req, "get-skill-file-result"),
    500,
    "get-skill-file request",
  );

  assert.equal(reply.seq, seq);
  assert.equal(reply.ok, true);
  assert.equal(reply.contentB64, contentB64);
});

test("CP3 get-skill-file: bridge.getSkillFile that THROWS → clean get-skill-file-result {ok:false, error}, no unhandled rejection", async () => {
  const spy = makeSpyBridge();
  spy.getSkillFile = async () => {
    throw new Error("NotFound");
  };
  const { child } = makeWiredPair(spy);

  const seq = makeSeq()();
  const req: GetSkillFileRequest = { kind: "get-skill-file", seq, runId: TEST_RUN_ID, ref: "gone", path: "x.txt" };

  const reply = await withTimeout(
    child.request<GetSkillFileResult>(req, "get-skill-file-result"),
    500,
    "get-skill-file throw",
  );

  assert.equal(reply.ok, false);
  assert.match(reply.error ?? "", /NotFound/);
  assert.equal(reply.seq, seq);
});

test("CP3 get-skill-file: a bridge with no getSkillFile method (predates this feature) fails closed, not silently", async () => {
  const spy = makeSpyBridge(); // no getSkillFile override — the property is absent
  const { child } = makeWiredPair(spy);

  const seq = makeSeq()();
  const req: GetSkillFileRequest = { kind: "get-skill-file", seq, runId: TEST_RUN_ID, ref: "bundle-uuid-1", path: "x.txt" };

  const reply = await withTimeout(
    child.request<GetSkillFileResult>(req, "get-skill-file-result"),
    500,
    "get-skill-file unsupported",
  );

  assert.equal(reply.ok, false);
  assert.match(reply.error ?? "", /not supported/);
});

test("CP2 execute: bridge.execute that THROWS → clean execute-result {ok:false, error}, no unhandled rejection", async () => {
  // WHY: an unhandled rejection in an async handler crashes the parent process.
  // The server must catch bridge throws and map them to a clean error result so
  // the child gets a deterministic response and the parent stays alive.
  //
  // We instrument process 'unhandledRejection' to prove the invariant — not just
  // assert it by name. If the server leaks a rejected promise, the handler fires
  // and the assertion at the end fails.
  const spy = makeSpyBridge();
  spy.onExecute = async () => {
    throw new Error("unexpected broker RPC failure");
  };
  const { child } = makeWiredPair(spy);

  let unhandledFired = false;
  const unhandledHandler = () => { unhandledFired = true; };
  process.on("unhandledRejection", unhandledHandler);

  const seq = makeSeq()();
  const req: ExecuteRequest = { kind: "execute", seq, runId: TEST_RUN_ID, toolCallId: "tc-exec-throw" };

  const reply = await withTimeout(
    child.request<ExecuteResult>(req, "execute-result"),
    500,
    "execute throw",
  );

  process.off("unhandledRejection", unhandledHandler);

  assert.equal(reply.ok, false);
  assert.ok(
    reply.error?.includes("unexpected broker RPC failure"),
    `error must contain the thrown message, got: ${reply.error}`,
  );
  assert.equal(reply.seq, seq);
  assert.equal(unhandledFired, false, "bridge throw must not produce an unhandled rejection");
});

test("CP2 delegate: delegate request is routed to bridge.delegate and result echoes seq", async () => {
  const spy = makeSpyBridge();
  spy.onDelegate = async () => ({ ok: true, envelopeId: "env-abc" });
  const { child } = makeWiredPair(spy);

  const seq = makeSeq()();
  const req: DelegateRequest = {
    kind: "delegate",
    seq,
    runId: TEST_RUN_ID,
    to: "bob",
    intent: "summarise logs",
    scopes: ["siem:read"],
  };

  const reply = await withTimeout(
    child.request<DelegateResult>(req, "delegate-result"),
    500,
    "delegate request",
  );

  assert.equal(reply.seq, seq);
  assert.equal(reply.ok, true);
  assert.equal(reply.envelopeId, "env-abc");

  assert.equal(spy.delegateCalls.length, 1);
  const call = spy.delegateCalls[0];
  assert.ok(call, "delegate call must be recorded");
  assert.equal(call.to, "bob");
  assert.equal(call.intent, "summarise logs");
  assert.deepEqual(call.scopes, ["siem:read"]);
});

test("CP2 delegate: bridge.delegate that THROWS → clean delegate-result {ok:false, error}, no unhandled rejection", async () => {
  // Same crash-safety guarantee as execute — any bridge method may throw.
  // Instrumented: if the server leaks an unhandled rejection the handler fires
  // and the assertion at the end fails.
  const spy = makeSpyBridge();
  spy.onDelegate = async () => {
    throw new Error("SendEnvelope RPC timeout");
  };
  const { child } = makeWiredPair(spy);

  let unhandledFired = false;
  const unhandledHandler = () => { unhandledFired = true; };
  process.on("unhandledRejection", unhandledHandler);

  const seq = makeSeq()();
  const req: DelegateRequest = {
    kind: "delegate",
    seq,
    runId: TEST_RUN_ID,
    to: "charlie",
    intent: "analyse",
  };

  const reply = await withTimeout(
    child.request<DelegateResult>(req, "delegate-result"),
    500,
    "delegate throw",
  );

  process.off("unhandledRejection", unhandledHandler);

  assert.equal(reply.ok, false);
  assert.ok(
    reply.error?.includes("SendEnvelope RPC timeout"),
    `error must contain thrown message, got: ${reply.error}`,
  );
  assert.equal(reply.seq, seq);
  assert.equal(unhandledFired, false, "bridge throw must not produce an unhandled rejection");
});

test("CP2 gate: bridge.gate that THROWS → clean gate-result {allow:false, reason}, no unhandled rejection", async () => {
  // Instrumented: if the server leaks an unhandled rejection the handler fires
  // and the assertion at the end fails.
  const spy = makeSpyBridge();
  spy.onGate = async () => {
    throw new Error("SubmitPlan RPC failed");
  };
  const { child } = makeWiredPair(spy);

  let unhandledFired = false;
  const unhandledHandler = () => { unhandledFired = true; };
  process.on("unhandledRejection", unhandledHandler);

  const seq = makeSeq()();
  const req: GateRequest = {
    kind: "gate",
    seq,
    runId: TEST_RUN_ID,
    toolCallId: "tc-gate-throw",
    toolName: "web_fetch",
    input: {},
  };

  const reply = await withTimeout(
    child.request<GateResult>(req, "gate-result"),
    500,
    "gate throw",
  );

  process.off("unhandledRejection", unhandledHandler);

  assert.equal(reply.allow, false);
  assert.ok(
    reply.reason?.includes("SubmitPlan RPC failed"),
    `reason must contain thrown message, got: ${reply.reason}`,
  );
  assert.equal(reply.seq, seq);
  assert.equal(unhandledFired, false, "bridge throw must not produce an unhandled rejection");
});

test("CP2 concurrent: multiple in-flight requests are correctly demultiplexed by seq", async () => {
  // WHY: a child may have multiple tool calls in flight (e.g. concurrent async
  // tool calls in the Pi loop). Each must get its own reply correlated by seq —
  // not the first reply to arrive resolving all of them.
  const spy = makeSpyBridge();

  // Make execute stall on "tc-slow" until we release it.
  let releaseSlowExec!: () => void;
  const slowBarrier = new Promise<void>((r) => { releaseSlowExec = r; });

  spy.onExecute = async (toolCallId) => {
    if (toolCallId === "tc-slow") {
      await slowBarrier;
      return { ok: true, output: "slow-result" };
    }
    return { ok: true, output: "fast-result" };
  };

  const { child } = makeWiredPair(spy);
  const gen = makeSeq();

  const slowSeq = gen();
  const fastSeq = gen();

  const slowP = child.request<ExecuteResult>(
    { kind: "execute", seq: slowSeq, runId: TEST_RUN_ID, toolCallId: "tc-slow" },
    "execute-result",
  );
  const fastP = child.request<ExecuteResult>(
    { kind: "execute", seq: fastSeq, runId: TEST_RUN_ID, toolCallId: "tc-fast" },
    "execute-result",
  );

  // Fast should resolve first.
  const fastReply = await withTimeout(fastP, 500, "fast execute");
  assert.equal(fastReply.seq, fastSeq);
  assert.equal(fastReply.output, "fast-result");

  // Slow is still pending — release it.
  releaseSlowExec();
  const slowReply = await withTimeout(slowP, 500, "slow execute");
  assert.equal(slowReply.seq, slowSeq);
  assert.equal(slowReply.output, "slow-result");
});

// ── spawn_subagents ───────────────────────

test("CP5 spawn-subagents: request is routed to bridge.spawnSubagents and result echoes seq", async () => {
  const spy = makeSpyBridge();
  spy.spawnSubagents = async (branches, aggregatorInstruction) => ({
    ok: true,
    branches: branches.map((b, index) => ({ index, task: b.task, ok: true, output: "done" })),
    synthesis: `synthesis for: ${aggregatorInstruction}`,
  });
  const { child } = makeWiredPair(spy);

  const seq = makeSeq()();
  const req: SpawnSubagentsRequest = {
    kind: "spawn-subagents",
    seq,
    runId: TEST_RUN_ID,
    branches: [{ task: "research A" }],
    aggregatorInstruction: "combine the findings",
  };

  const reply = await withTimeout(
    child.request<SpawnSubagentsResult>(req, "spawn-subagents-result"),
    500,
    "spawn-subagents request",
  );

  assert.equal(reply.seq, seq);
  assert.equal(reply.ok, true);
  assert.equal(reply.synthesis, "synthesis for: combine the findings");
  assert.deepEqual(reply.branches, [{ index: 0, task: "research A", ok: true, output: "done" }]);
});

test("CP5 spawn-subagents: bridge.spawnSubagents that THROWS → clean spawn-subagents-result {ok:false, error}, no unhandled rejection", async () => {
  const spy = makeSpyBridge();
  spy.spawnSubagents = async () => {
    throw new Error("spend cap breached for this month");
  };
  const { child } = makeWiredPair(spy);

  const seq = makeSeq()();
  const req: SpawnSubagentsRequest = {
    kind: "spawn-subagents",
    seq,
    runId: TEST_RUN_ID,
    branches: [{ task: "a" }],
    aggregatorInstruction: "combine",
  };

  const reply = await withTimeout(
    child.request<SpawnSubagentsResult>(req, "spawn-subagents-result"),
    500,
    "spawn-subagents throw",
  );

  assert.equal(reply.ok, false);
  assert.match(reply.error ?? "", /spend cap breached/);
  assert.equal(reply.seq, seq);
});

test("CP5 spawn-subagents: a bridge with no spawnSubagents method (predates this feature) fails closed, not silently", async () => {
  const spy = makeSpyBridge(); // no spawnSubagents override — the property is absent
  const { child } = makeWiredPair(spy);

  const seq = makeSeq()();
  const req: SpawnSubagentsRequest = {
    kind: "spawn-subagents",
    seq,
    runId: TEST_RUN_ID,
    branches: [{ task: "a" }],
    aggregatorInstruction: "combine",
  };

  const reply = await withTimeout(
    child.request<SpawnSubagentsResult>(req, "spawn-subagents-result"),
    500,
    "spawn-subagents unsupported",
  );

  assert.equal(reply.ok, false);
  assert.match(reply.error ?? "", /not supported/);
});
