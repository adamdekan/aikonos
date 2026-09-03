// CP1 tests: IPC protocol module — discriminated-union message types + typed links.
//
// WHY these tests exist: the IPC seam is the security boundary between the trusted
// parent (credential-holder) and the untrusted child (Pi loop). The codec must:
//   - round-trip every message kind faithfully (no data loss, no kind confusion)
//   - correlate request/reply strictly by seq (a mismatched seq must not resolve)
//   - reject / log unknown kinds without throwing into the event loop
//   - time out pending requests so a crashed child doesn't leak promises forever
//
// All tests use the fake in-memory paired channel — no real process.fork here.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  makeSeq,
  makePairedChannel,
  ParentLink,
  ChildLink,
} from "../src/ipc/protocol.js";
import type {
  IpcMessage,
  GateRequest,
  GateResult,
  ExecuteRequest,
  ExecuteResult,
  DelegateRequest,
  DelegateResult,
  PromptMessage,
  ShutdownMessage,
  InitMessage,
  TextDeltaEvent,
  ToolStartEvent,
  ToolEndEvent,
  UsageEvent,
  DoneEvent,
  ErrorEvent,
} from "../src/ipc/protocol.js";

// ── Helpers ────────────────────────────────────────────────────────────────────

/** Resolve a promise within ms or throw a timeout error. */
function withTimeout<T>(p: Promise<T>, ms: number, label: string): Promise<T> {
  return Promise.race([
    p,
    new Promise<never>((_, reject) =>
      setTimeout(() => reject(new Error(`timeout: ${label}`)), ms),
    ),
  ]);
}

// ── seq generator ──────────────────────────────────────────────────────────────

test("CP1 seq: generator produces strictly monotonically increasing integers", () => {
  // WHY: seq is the sole correlation key for pending requests. Non-monotonic or
  // repeated seqs would allow a reply to resolve the wrong pending promise.
  const gen = makeSeq();
  const vals = [gen(), gen(), gen(), gen(), gen()];
  for (let i = 1; i < vals.length; i++) {
    assert.ok(vals[i] > vals[i - 1], `seq[${i}] must be > seq[${i - 1}]`);
  }
  // All integers
  for (const v of vals) {
    assert.equal(v, Math.floor(v), "seq must be an integer");
  }
});

test("CP1 seq: independent generators do not share state", () => {
  const g1 = makeSeq();
  const g2 = makeSeq();
  // Both start fresh; neither influences the other.
  const a = g1();
  const b = g2();
  assert.equal(a, b, "independent generators start at the same value");
  assert.equal(g1(), g2(), "they advance independently");
});

// ── fake channel ───────────────────────────────────────────────────────────────

test("CP1 channel: a message sent on side A is received on side B", async () => {
  // WHY: the fake channel is the test seam for all CP1-CP6 unit tests. If it
  // does not faithfully relay messages both sides would silently talk to themselves.
  const [a, b] = makePairedChannel();

  const received: IpcMessage[] = [];
  b.on("message", (msg) => received.push(msg));

  const msg: GateRequest = {
    kind: "gate",
    seq: 1,
    runId: "run-1",
    toolCallId: "tc-1",
    toolName: "web_fetch",
    input: { url: "https://example.com" },
  };
  a.send(msg);

  await new Promise((r) => setImmediate(r));
  assert.equal(received.length, 1);
  assert.deepEqual(received[0], msg);
});

test("CP1 channel: side B can send back to side A (bidirectional)", async () => {
  const [a, b] = makePairedChannel();

  const fromB: IpcMessage[] = [];
  a.on("message", (msg) => fromB.push(msg));

  const reply: GateResult = { kind: "gate-result", seq: 1, allow: true };
  b.send(reply);

  await new Promise((r) => setImmediate(r));
  assert.equal(fromB.length, 1);
  assert.deepEqual(fromB[0], reply);
});

// ── round-trip: every request kind ────────────────────────────────────────────

test("CP1 round-trip gate: child sends gate, parent sees it verbatim", async () => {
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const childLink = new ChildLink(childSide);

  const seen: GateRequest[] = [];
  parentLink.on("gate", (m) => seen.push(m));

  const msg: GateRequest = {
    kind: "gate",
    seq: 1,
    runId: "run-1",
    toolCallId: "tc-gate",
    toolName: "doc_write",
    input: { path: "/tmp/x", content: "hello" },
    opts: { readOnlyHint: false },
  };
  childLink.send(msg);

  await new Promise((r) => setImmediate(r));
  assert.equal(seen.length, 1);
  assert.deepEqual(seen[0], msg);
});

test("CP1 round-trip execute: child sends execute, parent sees it verbatim", async () => {
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const childLink = new ChildLink(childSide);

  const seen: ExecuteRequest[] = [];
  parentLink.on("execute", (m) => seen.push(m));

  const msg: ExecuteRequest = { kind: "execute", seq: 2, runId: "run-1", toolCallId: "tc-exec" };
  childLink.send(msg);

  await new Promise((r) => setImmediate(r));
  assert.deepEqual(seen[0], msg);
});

test("CP1 round-trip delegate: child sends delegate, parent sees it verbatim", async () => {
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const childLink = new ChildLink(childSide);

  const seen: DelegateRequest[] = [];
  parentLink.on("delegate", (m) => seen.push(m));

  const msg: DelegateRequest = {
    kind: "delegate",
    seq: 3,
    runId: "run-1",
    to: "bob",
    intent: "summarise logs",
    scopes: ["siem:read"],
  };
  childLink.send(msg);

  await new Promise((r) => setImmediate(r));
  assert.deepEqual(seen[0], msg);
});

test("CP1 round-trip gate-result: parent sends gate-result, child sees it verbatim", async () => {
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const childLink = new ChildLink(childSide);

  const seen: GateResult[] = [];
  childLink.on("gate-result", (m) => seen.push(m));

  const msg: GateResult = { kind: "gate-result", seq: 1, allow: false, reason: "denied" };
  parentLink.send(msg);

  await new Promise((r) => setImmediate(r));
  assert.deepEqual(seen[0], msg);
});

test("CP1 round-trip execute-result: parent sends execute-result, child sees it verbatim", async () => {
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const childLink = new ChildLink(childSide);

  const seen: ExecuteResult[] = [];
  childLink.on("execute-result", (m) => seen.push(m));

  const msg: ExecuteResult = { kind: "execute-result", seq: 2, ok: true, output: { data: "result" } };
  parentLink.send(msg);

  await new Promise((r) => setImmediate(r));
  assert.deepEqual(seen[0], msg);
});

test("CP1 round-trip delegate-result: parent sends delegate-result, child sees it verbatim", async () => {
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const childLink = new ChildLink(childSide);

  const seen: DelegateResult[] = [];
  childLink.on("delegate-result", (m) => seen.push(m));

  const msg: DelegateResult = { kind: "delegate-result", seq: 3, ok: true, envelopeId: "env-99" };
  parentLink.send(msg);

  await new Promise((r) => setImmediate(r));
  assert.deepEqual(seen[0], msg);
});

test("CP1 round-trip prompt: parent sends prompt, child sees it verbatim", async () => {
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const childLink = new ChildLink(childSide);

  const seen: PromptMessage[] = [];
  childLink.on("prompt", (m) => seen.push(m));

  const msg: PromptMessage = { kind: "prompt", runId: "run-1", threadId: "th-1", text: "hello" };
  parentLink.send(msg);

  await new Promise((r) => setImmediate(r));
  assert.deepEqual(seen[0], msg);
});

test("CP1 round-trip shutdown: parent sends shutdown, child sees it verbatim", async () => {
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const childLink = new ChildLink(childSide);

  const seen: ShutdownMessage[] = [];
  childLink.on("shutdown", (m) => seen.push(m));

  const msg: ShutdownMessage = { kind: "shutdown" };
  parentLink.send(msg);

  await new Promise((r) => setImmediate(r));
  assert.deepEqual(seen[0], msg);
});

test("CP1 round-trip init: parent sends init, child sees it verbatim", async () => {
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const childLink = new ChildLink(childSide);

  const seen: InitMessage[] = [];
  childLink.on("init", (m) => seen.push(m));

  const msg: InitMessage = {
    kind: "init",
    modelId: "anthropic/claude-sonnet-4-5",
    systemPrompt: "You are helpful.",
    allowedToolNames: ["web_fetch"],
    mcpTools: [],
    proxyBaseUrl: "http://127.0.0.1:9999",
    proxyModelAllowlist: ["anthropic/claude-sonnet-4-5"],
  };
  parentLink.send(msg);

  await new Promise((r) => setImmediate(r));
  assert.deepEqual(seen[0], msg);
});

// ── round-trip: child→parent events ───────────────────────────────────────────

test("CP1 round-trip text_delta: child sends event, parent receives it with correct runId", async () => {
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const childLink = new ChildLink(childSide);

  const seen: TextDeltaEvent[] = [];
  parentLink.on("text_delta", (m) => seen.push(m));

  const msg: TextDeltaEvent = { kind: "text_delta", runId: "run-42", delta: "Hello " };
  childLink.send(msg);

  await new Promise((r) => setImmediate(r));
  assert.deepEqual(seen[0], msg);
});

test("CP1 round-trip tool_start: event carries runId and toolCallId", async () => {
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const childLink = new ChildLink(childSide);

  const seen: ToolStartEvent[] = [];
  parentLink.on("tool_start", (m) => seen.push(m));

  const msg: ToolStartEvent = {
    kind: "tool_start",
    runId: "run-1",
    toolCallId: "tc-1",
    toolName: "web_fetch",
    input: { url: "https://example.com" },
  };
  childLink.send(msg);

  await new Promise((r) => setImmediate(r));
  assert.deepEqual(seen[0], msg);
});

test("CP1 round-trip tool_end: event carries ok status", async () => {
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const childLink = new ChildLink(childSide);

  const seen: ToolEndEvent[] = [];
  parentLink.on("tool_end", (m) => seen.push(m));

  const msg: ToolEndEvent = { kind: "tool_end", runId: "run-1", toolCallId: "tc-1", ok: true, result: "done" };
  childLink.send(msg);

  await new Promise((r) => setImmediate(r));
  assert.deepEqual(seen[0], msg);
});

test("CP1 round-trip usage: event carries token counts", async () => {
  // WHY: usage events must travel from child to parent so the parent can
  // forward them to the broker (ChildSupervisor.forwardUsage in
  // supervisor.ts). The child has no south client, so it cannot call the
  // broker directly — the parent must relay. This test proves the codec
  // doesn't swallow usage events.
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const childLink = new ChildLink(childSide);

  const seen: UsageEvent[] = [];
  parentLink.on("usage", (m) => seen.push(m));

  const msg: UsageEvent = {
    kind: "usage",
    runId: "run-1",
    inputTokens: 100,
    outputTokens: 50,
  };
  childLink.send(msg);

  await new Promise((r) => setImmediate(r));
  assert.deepEqual(seen[0], msg);
});

test("CP1 round-trip usage: event carries the additive metering fields (cost, cache, provider, model)", async () => {
  // WHY: spend-caps CP3 extends the usage relay so the parent can forward a
  // full EmitLlmUsage call (cost, cache reads/writes, real provider id, model)
  // instead of just token counts. These fields are optional on the wire for
  // version-skew tolerance — this test proves the codec carries them when present.
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const childLink = new ChildLink(childSide);

  const seen: UsageEvent[] = [];
  parentLink.on("usage", (m) => seen.push(m));

  const msg: UsageEvent = {
    kind: "usage",
    runId: "run-1",
    inputTokens: 100,
    outputTokens: 50,
    cost: 0.0012,
    cacheRead: 10,
    cacheWrite: 5,
    provider: "openai",
    model: "gpt-4o",
  };
  childLink.send(msg);

  await new Promise((r) => setImmediate(r));
  assert.deepEqual(seen[0], msg);
});

test("CP1 round-trip done: event terminates run stream", async () => {
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const childLink = new ChildLink(childSide);

  const seen: DoneEvent[] = [];
  parentLink.on("done", (m) => seen.push(m));

  const msg: DoneEvent = { kind: "done", runId: "run-1" };
  childLink.send(msg);

  await new Promise((r) => setImmediate(r));
  assert.deepEqual(seen[0], msg);
});

test("CP1 round-trip error: event carries message and terminates run stream", async () => {
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const childLink = new ChildLink(childSide);

  const seen: ErrorEvent[] = [];
  parentLink.on("error", (m) => seen.push(m));

  const msg: ErrorEvent = { kind: "error", runId: "run-1", message: "something exploded" };
  childLink.send(msg);

  await new Promise((r) => setImmediate(r));
  assert.deepEqual(seen[0], msg);
});

// ── request() correlation ──────────────────────────────────────────────────────

test("CP1 request(): resolves when the matching seq reply arrives", async () => {
  // WHY: request() is how the child drives the parent for gate/execute/delegate.
  // Correct seq correlation means only the reply for THIS call resolves the
  // promise — not a reply for a concurrent call.
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const childLink = new ChildLink(childSide);

  const gen = makeSeq();
  const seq = gen();
  const req: GateRequest = {
    kind: "gate",
    seq,
    runId: "run-1",
    toolCallId: "tc-req",
    toolName: "web_fetch",
    input: {},
  };

  // Parent echoes back the gate-result with the same seq.
  parentLink.on("gate", (m) => {
    const reply: GateResult = { kind: "gate-result", seq: m.seq, allow: true };
    parentLink.send(reply);
  });

  const result = await withTimeout(
    childLink.request<GateResult>(req, "gate-result"),
    500,
    "gate request",
  );

  assert.equal(result.allow, true);
  assert.equal(result.seq, seq);
});

test("CP1 request(): ignores a reply with a mismatched seq — only the matching seq resolves", async () => {
  // WHY: if mismatched seqs resolved the wrong promise, a concurrent gate from
  // another tool call would steal the reply. That would grant access for the
  // wrong toolCallId — a security flaw, not just a bug.
  //
  // Negative assertion: the promise must NOT be settled by the wrong-seq reply.
  // We prove this by recording a "wrongSeqDispatched" flag after the wrong reply
  // is sent and before the correct reply is sent. If the promise had settled on
  // the wrong-seq reply, `wrongSeqDispatched` would be false when we check it
  // post-await (the correct-seq send sets it to true first in our ordering).
  // Concretely: deleting the seq-key lookup in `dispatch` would cause the
  // promise to resolve on the wrong reply (allow=false), failing the
  // `result.allow === true` assertion AND revealing the timing violation.
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const childLink = new ChildLink(childSide);

  const gen = makeSeq();
  const correctSeq = gen();
  const wrongSeq = gen(); // different number

  const req: GateRequest = {
    kind: "gate",
    seq: correctSeq,
    runId: "run-1",
    toolCallId: "tc-mismatch",
    toolName: "web_fetch",
    input: {},
  };

  // settledAfterCorrect is set to true only when the correct-seq reply is
  // dispatched. If the promise resolved on the wrong-seq reply, this would be
  // false at the await boundary.
  let wrongSeqDispatched = false;
  let correctSeqDispatched = false;

  // First send a reply with the wrong seq — must not resolve.
  parentLink.on("gate", () => {
    const wrong: GateResult = { kind: "gate-result", seq: wrongSeq, allow: false };
    parentLink.send(wrong);
    // After the wrong reply is in flight, mark it dispatched, then send the correct one.
    setImmediate(() => {
      wrongSeqDispatched = true;
      const correct: GateResult = { kind: "gate-result", seq: correctSeq, allow: true };
      correctSeqDispatched = true;
      parentLink.send(correct);
    });
  });

  const result = await withTimeout(
    childLink.request<GateResult>(req, "gate-result"),
    500,
    "mismatch test",
  );

  // The resolved value must come from the correct-seq reply.
  assert.equal(result.allow, true, "must resolve with the matching-seq reply (allow=true), not the wrong-seq reply (allow=false)");
  assert.equal(result.seq, correctSeq, "resolved seq must match the request seq");

  // Negative: both wrong-seq and correct-seq were dispatched before resolution
  // settled — proves the promise waited for the right one.
  assert.equal(wrongSeqDispatched, true, "wrong-seq reply was dispatched (test precondition)");
  assert.equal(correctSeqDispatched, true, "correct-seq reply was dispatched before resolution");
});

test("CP1 request(): rejects after timeout when no reply arrives", async () => {
  // WHY: a crashed or stalled child must not leak the parent's promise map
  // indefinitely. Bounded timeout is the backstop against promise leaks.
  const [parentSide, childSide] = makePairedChannel();
  // parentLink exists but never replies — simulates a dead child.
  new ParentLink(parentSide);
  const childLink = new ChildLink(childSide);

  const req: ExecuteRequest = { kind: "execute", seq: 1, runId: "run-1", toolCallId: "tc-timeout" };

  await assert.rejects(
    childLink.request<ExecuteResult>(req, "execute-result", 50),
    (err: unknown) => {
      assert.ok(err instanceof Error, "must reject with an Error");
      assert.ok(
        (err as Error).message.includes("timeout"),
        `error message must mention timeout, got: ${(err as Error).message}`,
      );
      return true;
    },
  );
});

// ── unknown kind handling ──────────────────────────────────────────────────────

test("CP1 unknown kind: an unrecognised message kind is logged, not thrown into the event loop", async () => {
  // WHY: an unknown kind thrown synchronously (or via uncaughtException) would
  // crash the parent process, taking down all active sessions. Log-and-discard
  // is the correct recovery posture for unrecognised variants.
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);

  const warnings: unknown[] = [];
  parentLink.onUnknownKind((msg) => warnings.push(msg));

  // Send something that is structurally a message but with an unknown kind.
  childSide.send({ kind: "definitely-not-a-real-kind", runId: "run-x" } as unknown as IpcMessage);

  await new Promise((r) => setImmediate(r));

  // The event loop is still running (we got here) and the warning was captured.
  assert.equal(warnings.length, 1, "unknown kind must be surfaced via onUnknownKind, not thrown");
});

test("CP1 unknown kind on ChildLink: same graceful handling in the child direction", async () => {
  const [parentSide, childSide] = makePairedChannel();
  const childLink = new ChildLink(childSide);

  const warnings: unknown[] = [];
  childLink.onUnknownKind((msg) => warnings.push(msg));

  parentSide.send({ kind: "bogus-parent-kind" } as unknown as IpcMessage);

  await new Promise((r) => setImmediate(r));
  assert.equal(warnings.length, 1);
});

// ── dispatch routing ───────────────────────────────────────────────────────────

test("CP1 dispatch: a message routes only to the handler registered for its kind", async () => {
  // WHY: if kind routing is broken (e.g. a wildcard that fires all handlers),
  // the parent bridge would attempt to service unrelated messages, leading to
  // undefined behaviour on the pending map.
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const childLink = new ChildLink(childSide);

  const gateCount = { n: 0 };
  const executeCount = { n: 0 };
  parentLink.on("gate", () => { gateCount.n++; });
  parentLink.on("execute", () => { executeCount.n++; });

  childLink.send({ kind: "gate", seq: 1, toolCallId: "x", toolName: "web_fetch", input: {} });
  childLink.send({ kind: "execute", seq: 2, toolCallId: "y" });
  childLink.send({ kind: "gate", seq: 3, toolCallId: "z", toolName: "doc_write", input: {} });

  await new Promise((r) => setImmediate(r));

  assert.equal(gateCount.n, 2, "gate handler must fire exactly for gate messages");
  assert.equal(executeCount.n, 1, "execute handler must fire exactly for execute messages");
});

test("CP1 dispatch: multiple handlers can be registered for the same kind", async () => {
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const childLink = new ChildLink(childSide);

  let count = 0;
  parentLink.on("gate", () => { count++; });
  parentLink.on("gate", () => { count++; });

  childLink.send({ kind: "gate", seq: 1, toolCallId: "x", toolName: "web_fetch", input: {} });

  await new Promise((r) => setImmediate(r));
  assert.equal(count, 2, "both handlers must fire");
});
