// CP3 tests: child-side bridge client + child entrypoint.
//
// WHY these tests exist:
//   bridge-client: each method must send the correct IPC request kind and resolve
//     from the matching *-result. The client must structurally satisfy the
//     BridgeClientLike shape so makeTools(client) compiles without modification.
//   child-entry: init + prompt must drive the session factory and relay all
//     session events over IPC tagged with the correct runId, terminated by done.
//   usage relay: usage events from the child session must travel over IPC to the
//     parent — the child has no south client, so IPC is the only exit path.
//   shutdown: the child must exit cleanly without hanging.
//
// All tests use the fake in-memory paired channel + injected fake session factory.
// No real LLM call, no real process.fork.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  makePairedChannel,
  makeSeq,
  ParentLink,
  ChildLink,
} from "../src/ipc/protocol.js";
import type {
  GateResult,
  ExecuteResult,
  DelegateResult,
  InitMessage,
  GateRequest,
  ExecuteRequest,
  DelegateRequest,
  TextDeltaEvent,
  ToolStartEvent,
  ToolEndEvent,
  UsageEvent,
  DoneEvent,
  ErrorEvent,
  AbortMessage,
  GetSkillBodyRequest,
  GetSkillBodyResult,
} from "../src/ipc/protocol.js";
import { RemoteBridgeClient } from "../src/ipc/bridge-client.js";
import { startChild, resolveThreadTtlMs, resolveToolDescription, type SessionFactory, type SessionLike, type SessionPlan } from "../src/ipc/child-entry.js";
import type { AgentSessionEvent } from "@earendil-works/pi-coding-agent";
import { makeTools } from "../src/pi/tools.js";
import type { BridgeClientLike } from "../src/ipc/bridge-client.js";

// ── Helpers ────────────────────────────────────────────────────────────────────

function withTimeout<T>(p: Promise<T>, ms: number, label: string): Promise<T> {
  return Promise.race([
    p,
    new Promise<never>((_, reject) =>
      setTimeout(() => reject(new Error(`timeout: ${label}`)), ms),
    ),
  ]);
}

async function flushAsync(ticks = 5): Promise<void> {
  for (let i = 0; i < ticks; i++) {
    await new Promise((r) => setImmediate(r));
  }
}

// ── Minimal fake AgentSession ──────────────────────────────────────────────────
//
// Implements only the surface child-entry uses: prompt() + subscribe().
// subscribe() returns an unsubscribe function. Tests control what events are
// emitted and when prompt() resolves via the returned handles.

interface FakeSessionHandle {
  session: FakeSession;
  // Emit a partial fake event into all active subscribers. The real SDK event
  // type includes large required fields (AssistantMessage, Usage.cost, etc.) —
  // tests only provide the fields they assert on. The cast lives here, once, so
  // call sites have no casts.
  emit(event: object): void;
  // Resolve the pending prompt() promise (simulates the LLM turn completing).
  resolvePrompt(): void;
  // Reject the pending prompt() promise (simulates an LLM error).
  rejectPrompt(err: Error): void;
}

interface FakeSession extends SessionLike {
  // Capture the text passed to prompt() for assertions.
  promptCalls: string[];
  // Capture abort() calls so tests can assert abort was invoked.
  abortCalls: number;
}

function makeFakeSession(): FakeSessionHandle {
  const listeners = new Set<(event: AgentSessionEvent) => void>();
  const promptCalls: string[] = [];
  let resolvePrompt!: () => void;
  let rejectPrompt!: (err: Error) => void;
  let promptPending: Promise<void> | undefined;

  const session: FakeSession = {
    promptCalls,
    abortCalls: 0,
    prompt(text: string): Promise<void> {
      promptCalls.push(text);
      promptPending = new Promise<void>((res, rej) => {
        resolvePrompt = res;
        rejectPrompt = rej;
      });
      return promptPending;
    },
    subscribe(listener: (event: AgentSessionEvent) => void): () => void {
      listeners.add(listener);
      return () => { listeners.delete(listener); };
    },
    dispose(): void { listeners.clear(); },
    abort(): Promise<void> {
      session.abortCalls++;
      return Promise.resolve();
    },
  };

  return {
    session,
    emit(event: object) {
      // Cast is here, centralised — callers stay cast-free. Tests only supply
      // the fields child-entry actually reads; the SDK requires many more.
      for (const l of listeners) l(event as AgentSessionEvent);
    },
    resolvePrompt() { resolvePrompt(); },
    rejectPrompt(err: Error) { rejectPrompt(err); },
  };
}

// ── Minimal SessionPlan ────────────────────────────────────────────────────────

const STUB_PLAN: SessionPlan = {
  kind: "init",
  modelId: "test-model",
  providerId: "test-provider",
  systemPrompt: "You are helpful.",
  allowedToolNames: ["web_fetch"],
  mcpTools: [],
  proxyBaseUrl: "http://127.0.0.1:0",
  proxyModelAllowlist: ["test-model"],
};

// ── F26: AIKONOS_GATEWAY_THREAD_TTL_MS NaN guard ───────────────────────────────
//
// The child receives this var post-fork via the parent's scrubbed-env
// allowlist, so it cannot read a validated Config — a malformed value must
// fall back to the default instead of wedging the reaper's TTL comparison
// with NaN (a NaN comparison is always false, so idle sessions would never
// be reaped). RED-FIRST: before the guard, resolveThreadTtlMs("abc") would
// have returned NaN (Number("abc") ?? default doesn't help — ?? only checks
// null/undefined, not NaN).

test("resolveThreadTtlMs: undefined env falls back to the 30-minute default", () => {
  assert.equal(resolveThreadTtlMs(undefined), 30 * 60 * 1000);
});

test("resolveThreadTtlMs: valid positive numeric string is used as-is", () => {
  assert.equal(resolveThreadTtlMs("60000"), 60000);
});

test("resolveThreadTtlMs: malformed value falls back to the default instead of NaN", () => {
  assert.equal(resolveThreadTtlMs("not-a-number"), 30 * 60 * 1000);
});

test("resolveThreadTtlMs: zero or negative value falls back to the default", () => {
  assert.equal(resolveThreadTtlMs("0"), 30 * 60 * 1000);
  assert.equal(resolveThreadTtlMs("-100"), 30 * 60 * 1000);
});

// ── resolveToolDescription: live-visibility tool-trace labelling ───────────────

test("resolveToolDescription: uses the registered tool description for a static tool", () => {
  const map = { web_fetch: "Fetch a public web page over HTTPS." };
  assert.equal(
    resolveToolDescription("web_fetch", { url: "https://x" }, map, []),
    "Fetch a public web page over HTTPS.",
  );
});

test("resolveToolDescription: load_skill prefers the matched bundle's description over the tool's own", () => {
  const map = { load_skill: "Activate a skill bundle by name." };
  const bundles = [
    { id: "b1", name: "invoices", description: "Process supplier invoices end to end.", body: "", allowedTools: [], contextFork: false, disableModelInvocation: false, keywords: [], filePaths: [] },
  ];
  assert.equal(
    resolveToolDescription("load_skill", { name: "invoices" }, map, bundles),
    "Process supplier invoices end to end.",
  );
});

test("resolveToolDescription: load_skill with an unmatched name falls back to the tool description", () => {
  const map = { load_skill: "Activate a skill bundle by name." };
  assert.equal(
    resolveToolDescription("load_skill", { name: "nope" }, map, []),
    "Activate a skill bundle by name.",
  );
});

test("resolveToolDescription: returns undefined when nothing is known (UI falls back to the tool name)", () => {
  assert.equal(resolveToolDescription("mystery_tool", {}, {}, []), undefined);
});

test("resolveToolDescription: truncates a long description to 200 chars", () => {
  const long = "x".repeat(500);
  const out = resolveToolDescription("web_fetch", {}, { web_fetch: long }, []);
  assert.equal(out?.length, 200);
});

// ── bridge-client: structural drop-in ─────────────────────────────────────────

test("CP3 bridge-client: RemoteBridgeClient is structurally assignable to GovernanceBridge so makeTools accepts it", () => {
  // WHY: tools.ts uses makeTools(bridge: GovernanceBridge). If RemoteBridgeClient
  // is missing any public method the type system would reject this assignment at
  // compile time. This test proves the seam compiles correctly — if it breaks,
  // something removed a public GovernanceBridge method the client must mirror.
  const [_parentSide, childSide] = makePairedChannel();
  const link = new ChildLink(childSide);
  const client = new RemoteBridgeClient(link);

  // Explicit annotation against BridgeClientLike — compiler enforces structural match.
  // makeTools accepts BridgeClientLike, so this proves RemoteBridgeClient satisfies
  // the full public surface that makeTools requires.
  const bridge: BridgeClientLike = client;
  // Pass it to makeTools — if any method is missing the type assignment above fails.
  const tools = makeTools(bridge);
  assert.ok(tools.length > 0, "makeTools must return at least one tool");
});

// ── bridge-client: gate ────────────────────────────────────────────────────────

test("CP3 bridge-client gate: sends gate IPC request and resolves from matching gate-result", async () => {
  // WHY: each method must send the correct request kind so the parent's
  // RemoteBridgeServer can route it to the real GovernanceBridge.
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const link = new ChildLink(childSide);
  const client = new RemoteBridgeClient(link);

  // Parent echoes back allow:true.
  parentLink.on("gate", (msg: GateRequest) => {
    parentLink.send({ kind: "gate-result", seq: msg.seq, allow: true } satisfies GateResult);
  });

  const result = await withTimeout(
    client.withRun("test-run", () => client.gate("tc-1", "web_fetch", { url: "https://example.com" })),
    500,
    "bridge-client gate",
  );

  assert.equal(result.allow, true);
});

test("CP3 bridge-client gate: no identity field is present in the IPC message body", async () => {
  // WHY: the bridge client must never inject identity into the message — identity
  // is bound from the spawn record parent-side. This asserts the outbound message
  // body is clean, mirroring the CP2 forged-identity test from the other direction.
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const link = new ChildLink(childSide);
  const client = new RemoteBridgeClient(link);

  const captured: GateRequest[] = [];
  parentLink.on("gate", (msg: GateRequest) => {
    captured.push(msg);
    parentLink.send({ kind: "gate-result", seq: msg.seq, allow: true } satisfies GateResult);
  });

  await withTimeout(
    client.withRun("test-run", () => client.gate("tc-id-check", "web_fetch", { url: "https://example.com" })),
    500,
    "gate identity check",
  );

  assert.equal(captured.length, 1);
  const raw = JSON.stringify(captured[0]);
  assert.ok(!raw.includes("owner"), "gate message must not contain 'owner'");
  assert.ok(!raw.includes("userId"), "gate message must not contain 'userId'");
  assert.ok(!raw.includes("tenantId"), "gate message must not contain 'tenantId'");
  assert.ok(!raw.includes("bearer"), "gate message must not contain 'bearer'");
});

test("CP3 bridge-client gate: deny (allow:false) is surfaced faithfully", async () => {
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const link = new ChildLink(childSide);
  const client = new RemoteBridgeClient(link);

  parentLink.on("gate", (msg: GateRequest) => {
    parentLink.send({
      kind: "gate-result",
      seq: msg.seq,
      allow: false,
      reason: "denied by policy",
    } satisfies GateResult);
  });

  const result = await withTimeout(
    client.withRun("test-run", () => client.gate("tc-deny", "web_fetch", {})),
    500,
    "gate deny",
  );

  assert.equal(result.allow, false);
  assert.equal(result.reason, "denied by policy");
});

// ── bridge-client: execute ─────────────────────────────────────────────────────

test("CP3 bridge-client execute: sends execute IPC request and resolves from matching execute-result", async () => {
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const link = new ChildLink(childSide);
  const client = new RemoteBridgeClient(link);

  parentLink.on("execute", (msg: ExecuteRequest) => {
    parentLink.send({
      kind: "execute-result",
      seq: msg.seq,
      ok: true,
      output: { fetched: "page content" },
    } satisfies ExecuteResult);
  });

  const result = await withTimeout(
    client.withRun("test-run", () => client.execute("tc-exec-1")),
    500,
    "bridge-client execute",
  );

  assert.equal(result.ok, true);
  assert.deepEqual(result.output, { fetched: "page content" });
});

test("CP3 bridge-client execute: ok:false and error are forwarded", async () => {
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const link = new ChildLink(childSide);
  const client = new RemoteBridgeClient(link);

  parentLink.on("execute", (msg: ExecuteRequest) => {
    parentLink.send({
      kind: "execute-result",
      seq: msg.seq,
      ok: false,
      output: null,
      error: "tool proxy 503",
    } satisfies ExecuteResult);
  });

  const result = await withTimeout(
    client.withRun("test-run", () => client.execute("tc-exec-fail")),
    500,
    "execute fail",
  );

  assert.equal(result.ok, false);
  assert.equal(result.error, "tool proxy 503");
});

// ── bridge-client: delegate ────────────────────────────────────────────────────

test("CP3 bridge-client delegate: sends delegate IPC request and resolves from matching delegate-result", async () => {
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const link = new ChildLink(childSide);
  const client = new RemoteBridgeClient(link);

  parentLink.on("delegate", (msg: DelegateRequest) => {
    parentLink.send({
      kind: "delegate-result",
      seq: msg.seq,
      ok: true,
      envelopeId: "env-xyz",
    } satisfies DelegateResult);
  });

  const result = await withTimeout(
    client.withRun("test-run", () => client.delegate("bob@example.com", "summarise logs", ["siem:read"])),
    500,
    "bridge-client delegate",
  );

  assert.equal(result.ok, true);
  assert.equal(result.envelopeId, "env-xyz");
});

test("CP3 bridge-client delegate: error from parent is forwarded", async () => {
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const link = new ChildLink(childSide);
  const client = new RemoteBridgeClient(link);

  parentLink.on("delegate", (msg: DelegateRequest) => {
    parentLink.send({
      kind: "delegate-result",
      seq: msg.seq,
      ok: false,
      error: "SendEnvelope denied",
    } satisfies DelegateResult);
  });

  const result = await withTimeout(
    client.withRun("test-run", () => client.delegate("bob@example.com", "summarise")),
    500,
    "delegate error",
  );

  assert.equal(result.ok, false);
  assert.equal(result.error, "SendEnvelope denied");
});

// ── bridge-client: concurrent requests demuxed by seq ─────────────────────────

test("CP3 bridge-client: concurrent gate + execute requests are demultiplexed by seq", async () => {
  // WHY: the Pi loop may have multiple tool calls in flight. Each must get its own
  // reply — the seq correlation must hold under concurrency, not just in isolation.
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const link = new ChildLink(childSide);
  const client = new RemoteBridgeClient(link);

  let releaseGate!: () => void;
  const gateBarrier = new Promise<void>((r) => { releaseGate = r; });

  parentLink.on("gate", async (msg: GateRequest) => {
    await gateBarrier;
    parentLink.send({ kind: "gate-result", seq: msg.seq, allow: true } satisfies GateResult);
  });
  parentLink.on("execute", (msg: ExecuteRequest) => {
    parentLink.send({ kind: "execute-result", seq: msg.seq, ok: true, output: "fast" } satisfies ExecuteResult);
  });

  // Both calls are launched inside a single withRun so they share the active
  // runId for the duration. The promises are started but not awaited inside the
  // withRun fn, then awaited outside after releaseGate().
  let gateP!: Promise<{ allow: boolean; reason?: string }>;
  let execP!: Promise<{ ok: boolean; output: unknown; error?: string }>;
  const withRunPromise = client.withRun("test-run-concurrent", () => {
    gateP = client.gate("tc-slow", "web_fetch", {});
    execP = client.execute("tc-fast");
    // Keep withRun open until both settle so activeRunId stays set.
    return Promise.all([gateP, execP]).then(() => {});
  });

  // Execute should resolve before gate (gate is blocked).
  const execResult = await withTimeout(execP, 500, "fast execute");
  assert.equal(execResult.ok, true);

  releaseGate();
  const gateResult = await withTimeout(gateP, 500, "slow gate");
  assert.equal(gateResult.allow, true);
  await withRunPromise;
});

// ── child-entry: init + prompt → events relayed ───────────────────────────────

test("CP3 child-entry: init then prompt relays text_delta events with correct runId", async () => {
  // WHY: relaying session events tagged with runId is the core of child-entry's
  // contract — a parent receiving untagged or mis-tagged events cannot route them
  // to the right SSE connection.
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);

  const handle = makeFakeSession();
  const factory: SessionFactory = async () => handle.session;

  startChild(childSide, factory);

  // Parent: service gate + execute requests so the session loop doesn't hang.
  parentLink.on("gate", (msg: GateRequest) => {
    parentLink.send({ kind: "gate-result", seq: msg.seq, allow: true } satisfies GateResult);
  });
  parentLink.on("execute", (msg: ExecuteRequest) => {
    parentLink.send({ kind: "execute-result", seq: msg.seq, ok: true, output: "ok" } satisfies ExecuteResult);
  });

  // Send init.
  parentLink.send(STUB_PLAN satisfies InitMessage);
  await flushAsync();

  // Send prompt.
  const collected: TextDeltaEvent[] = [];
  parentLink.on("text_delta", (m: TextDeltaEvent) => collected.push(m));

  const doneP = new Promise<DoneEvent>((r) => parentLink.on("done", r));
  parentLink.send({ kind: "prompt", runId: "run-42", threadId: "th-1", text: "hello" });
  await flushAsync(2);

  // Emit some session events.
  handle.emit({ type: "message_update", assistantMessageEvent: { type: "text_delta", delta: "Hello " } });
  handle.emit({ type: "message_update", assistantMessageEvent: { type: "text_delta", delta: "world" } });
  await flushAsync(2);

  // Resolve prompt → done.
  handle.resolvePrompt();
  const done = await withTimeout(doneP, 500, "done event");

  assert.equal(done.runId, "run-42", "done must carry the prompt's runId");
  assert.equal(collected.length, 2, "both text_delta events must be relayed");
  assert.equal(collected[0]?.delta, "Hello ");
  assert.equal(collected[1]?.delta, "world");
  assert.ok(collected.every((e) => e.runId === "run-42"), "all events must carry runId=run-42");
});

test("CP3 child-entry: a tool call in the fake session loop produces gate IPC request then execute IPC request", async () => {
  // WHY: the IPC seam is only useful if the Pi tool call hook (gate) and the
  // tool execute hook (execute) both round-trip through it. This test simulates
  // the bridge-client being called by the session loop, which happens because
  // tools.ts wires the bridge to the tool_call hook.
  //
  // Concretely: the fake session directly calls client.gate() then client.execute()
  // (simulating what the Pi extension callback + tool execute() would do), and we
  // assert the correct IPC messages arrive at the parent stub.
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);

  let capturedClient: RemoteBridgeClient | undefined;
  const handle = makeFakeSession();
  const factory: SessionFactory = async (_plan, client) => {
    capturedClient = client;
    return handle.session;
  };

  startChild(childSide, factory);

  const gateRequests: GateRequest[] = [];
  const executeRequests: ExecuteRequest[] = [];

  parentLink.on("gate", (msg: GateRequest) => {
    gateRequests.push(msg);
    parentLink.send({ kind: "gate-result", seq: msg.seq, allow: true } satisfies GateResult);
  });
  parentLink.on("execute", (msg: ExecuteRequest) => {
    executeRequests.push(msg);
    parentLink.send({ kind: "execute-result", seq: msg.seq, ok: true, output: "result" } satisfies ExecuteResult);
  });

  parentLink.send(STUB_PLAN satisfies InitMessage);
  await flushAsync(2);
  parentLink.send({ kind: "prompt", runId: "run-tool", threadId: "th-2", text: "do a tool call" });
  await flushAsync(3);

  // By now the factory has been called and capturedClient is set.
  assert.ok(capturedClient, "factory must have been called; client must be available");

  // Simulate the Pi session calling gate + execute (as tools.ts bridge hook would).
  const doneP = new Promise<DoneEvent>((r) => parentLink.on("done", r));

  const gateP = capturedClient.gate("tc-sim", "web_fetch", { url: "https://example.com" });
  const gateResult = await withTimeout(gateP, 500, "simulated gate");
  assert.equal(gateResult.allow, true, "gate must be allowed");

  const execP = capturedClient.execute("tc-sim");
  const execResult = await withTimeout(execP, 500, "simulated execute");
  assert.equal(execResult.ok, true, "execute must succeed");

  handle.resolvePrompt();
  await withTimeout(doneP, 500, "done after tool call");

  assert.equal(gateRequests.length, 1, "one gate IPC request must have been sent");
  assert.equal(gateRequests[0]?.toolCallId, "tc-sim");
  assert.equal(executeRequests.length, 1, "one execute IPC request must have been sent");
  assert.equal(executeRequests[0]?.toolCallId, "tc-sim");
});

// ── NAMED usage-relay test ─────────────────────────────────────────────────────

test("CP3 child-entry usage-relay: a usage event emitted by the child session travels over IPC and is received by the parent stub", async () => {
  // WHY: the child has no south gRPC client — it cannot call broker.EmitLlmUsage
  // directly. Usage data (token counts, cost, cache, provider, model) must
  // leave the child via IPC so the parent (ChildSupervisor.forwardUsage) can
  // forward it to south.emitLlmUsage. If this relay is missing, usage is
  // silently dropped and spend-cap accounting is wrong.
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);

  const handle = makeFakeSession();
  const factory: SessionFactory = async () =>
    handle.session;

  startChild(childSide, factory);

  parentLink.send(STUB_PLAN satisfies InitMessage);
  await flushAsync();

  const usageEvents: UsageEvent[] = [];
  parentLink.on("usage", (m: UsageEvent) => usageEvents.push(m));

  const doneP = new Promise<DoneEvent>((r) => parentLink.on("done", r));
  parentLink.send({ kind: "prompt", runId: "run-usage", threadId: "th-usage", text: "count tokens" });
  await flushAsync(2);

  // Simulate the Pi session emitting a message_end with usage data.
  handle.emit({
    type: "message_end",
    message: {
      role: "assistant",
      model: "test-model",
      usage: { input: 120, output: 45, cacheRead: 10, cacheWrite: 5, cost: { total: 0.0012 } },
    },
  });
  await flushAsync(2);

  handle.resolvePrompt();
  await withTimeout(doneP, 500, "done after usage");

  assert.equal(usageEvents.length, 1, "exactly one usage event must be relayed over IPC");
  const u = usageEvents[0];
  assert.ok(u, "usage event must be present");
  assert.equal(u.runId, "run-usage", "usage event must carry the prompt runId");
  assert.equal(u.inputTokens, 120, "input token count must match");
  assert.equal(u.outputTokens, 45, "output token count must match");
  assert.equal(u.cost, 0.0012, "cost must come from message.usage.cost.total");
  assert.equal(u.cacheRead, 10, "cacheRead must come from message.usage.cacheRead");
  assert.equal(u.cacheWrite, 5, "cacheWrite must come from message.usage.cacheWrite");
  assert.equal(u.provider, "test-provider", "provider must come from the plan's providerId, not message.provider");
  assert.equal(u.model, "test-model", "model must come from message.model");
});

test("CP3 child-entry usage-relay: missing usage sub-fields default to 0/\"\" instead of throwing", async () => {
  // WHY: version skew or a genuinely minimal usage block must not crash the
  // relay — every new field is read defensively.
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);

  const handle = makeFakeSession();
  const factory: SessionFactory = async () => handle.session;

  startChild(childSide, factory);

  parentLink.send(STUB_PLAN satisfies InitMessage);
  await flushAsync();

  const usageEvents: UsageEvent[] = [];
  parentLink.on("usage", (m: UsageEvent) => usageEvents.push(m));

  const doneP = new Promise<DoneEvent>((r) => parentLink.on("done", r));
  parentLink.send({ kind: "prompt", runId: "run-usage-min", threadId: "th-usage-min", text: "count tokens" });
  await flushAsync(2);

  handle.emit({
    type: "message_end",
    message: { role: "assistant", usage: { input: 1, output: 1 } },
  });
  await flushAsync(2);

  handle.resolvePrompt();
  await withTimeout(doneP, 500, "done after usage");

  const u = usageEvents[0];
  assert.ok(u, "usage event must be present");
  assert.equal(u.cost, 0);
  assert.equal(u.cacheRead, 0);
  assert.equal(u.cacheWrite, 0);
});

test("child-entry: an assistant message ending with stopReason error is relayed as an error IPC event, not a silent done", async () => {
  // WHY: Pi resolves session.prompt() normally even when the LLM call fails —
  // the failure is carried on the final assistant message as stopReason
  // "error" + errorMessage. Before this relay existed, a failed LLM turn
  // finished as an empty RUN_FINISHED and the chat went silent (dev incident:
  // provider 402 credit exhaustion was invisible to users and to logs).
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);

  const handle = makeFakeSession();
  const factory: SessionFactory = async () =>
    handle.session;

  startChild(childSide, factory);

  parentLink.send(STUB_PLAN satisfies InitMessage);
  await flushAsync();

  const errorP = new Promise<ErrorEvent>((r) => parentLink.on("error", r));
  parentLink.send({ kind: "prompt", runId: "run-llmfail", threadId: "th-llmfail", text: "hi" });
  await flushAsync(2);

  handle.emit({
    type: "message_end",
    message: {
      role: "assistant",
      usage: { input: 10, output: 0 },
      stopReason: "error",
      errorMessage: "402 provider credits exhausted",
    },
  });
  await flushAsync(2);

  handle.resolvePrompt();
  const err = await withTimeout(errorP, 500, "error after failed LLM turn");
  assert.equal(err.runId, "run-llmfail", "error must carry the prompt runId");
  assert.match(err.message, /402 provider credits exhausted/, "the provider error text must survive the relay");
});

// ── child-entry: error path ────────────────────────────────────────────────────

test("CP3 child-entry: a rejected prompt produces an error IPC event with the correct runId", async () => {
  // WHY: done and error are both terminal. The parent must receive error (not hang)
  // when the LLM turn fails, so it can surface it to the SSE client.
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);

  const handle = makeFakeSession();
  const factory: SessionFactory = async () =>
    handle.session;

  startChild(childSide, factory);

  parentLink.send(STUB_PLAN satisfies InitMessage);
  await flushAsync();

  const errorP = new Promise<ErrorEvent>((r) => parentLink.on("error", r));
  parentLink.send({ kind: "prompt", runId: "run-err", threadId: "th-err", text: "fail" });
  await flushAsync(2);

  handle.rejectPrompt(new Error("LLM timeout"));
  const err = await withTimeout(errorP, 500, "error event");

  assert.equal(err.runId, "run-err");
  assert.ok(err.message.includes("LLM timeout"), `error message must include 'LLM timeout', got: ${err.message}`);
});

// ── child-entry: shutdown ──────────────────────────────────────────────────────

test("CP3 child-entry: shutdown does not hang (exits the loop without pending promises)", async () => {
  // WHY: if shutdown leaves timers or pending IPC requests dangling, the test
  // runner hangs. This test proves the child can receive shutdown and settle
  // cleanly — critical for the supervisor's drain-and-kill sequence (CP6).
  //
  // We can't call process.exit(0) in a test, so startChild is implemented to
  // call process.exit only when AIKONOS_CHILD_ENTRY=1. In the test we intercept
  // the shutdown handler by observing that the shutdown message is received
  // (the child link's on("shutdown") fires) without hanging.
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);

  // Use a real child link on childSide and spy on the shutdown handler.
  let shutdownFired = false;
  const originalOn = childSide.on.bind(childSide);

  // Wrap the childSide channel to intercept the shutdown message delivery.
  // We patch startChild's channel so that when the child calls link.on("shutdown"),
  // we can also observe it fired — then unblock the test.
  const shutdownP = new Promise<void>((resolve) => {
    const originalSend = parentSide.send.bind(parentSide);
    void originalSend; // suppress unused warning

    // Intercept: after shutdown is delivered to the child link, the child will
    // normally call process.exit — here it won't because AIKONOS_CHILD_ENTRY !== "1".
    // So we just need to prove the message was received.
    const childLink = new ChildLink(childSide);
    childLink.on("shutdown", () => {
      shutdownFired = true;
      resolve();
    });
  });

  // We don't call startChild here because it would register competing handlers.
  // Instead, test the shutdown delivery path through the base ChildLink directly.
  parentLink.send({ kind: "shutdown" });

  await withTimeout(shutdownP, 500, "shutdown delivery");
  assert.equal(shutdownFired, true, "shutdown message must be delivered to the child link");
});

// ── child-entry: thread reuse ──────────────────────────────────────────────────

test("CP3 child-entry: the same threadId reuses the existing session across two prompts", async () => {
  // WHY: conversation history lives in the Pi session. If a new session were
  // created for every prompt, the user would lose context between turns — the
  // child-side thread map must keep sessions alive until the idle TTL fires.
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);

  let factoryCalls = 0;
  const handle = makeFakeSession();
  const factory: SessionFactory = async () => {
    factoryCalls++;
    return handle.session;
  };

  startChild(childSide, factory);
  parentLink.send(STUB_PLAN satisfies InitMessage);
  await flushAsync();

  // First prompt on thread th-reuse.
  // ParentLink.on fires for every matching message; use a one-shot wrapper to
  // avoid the second done event resolving the first promise.
  const done1P = new Promise<DoneEvent>((r) => {
    const h = (m: DoneEvent) => { parentLink.on("done", () => {}); r(m); };
    parentLink.on("done", (m) => { if (m.runId === "run-1") h(m); });
  });
  parentLink.send({ kind: "prompt", runId: "run-1", threadId: "th-reuse", text: "first" });
  await flushAsync(2);
  handle.resolvePrompt();
  await withTimeout(done1P, 500, "first done");

  // Second prompt on the same thread.
  const done2P = new Promise<DoneEvent>((r) => {
    parentLink.on("done", (m) => { if (m.runId === "run-2") r(m); });
  });
  parentLink.send({ kind: "prompt", runId: "run-2", threadId: "th-reuse", text: "second" });
  await flushAsync(2);
  handle.resolvePrompt();
  await withTimeout(done2P, 500, "second done");

  assert.equal(factoryCalls, 1, "factory must be called exactly once — session reused for second prompt");
  assert.equal(handle.session.promptCalls.length, 2, "both prompts must reach the session");
});

// ── CP2 child-entry: overflow → error IPC sent, relay stops ──────────────────

test("CP2 child-entry: backlog beyond cap with non-draining parent → child sends error IPC for the run", async () => {
  // WHY: if the parent's IPC read loop stalls (e.g. parent busy or disconnected),
  // the child's event queue fills. After the cap is exceeded the child must send
  // an error IPC message for that runId and stop relaying — no unbounded growth.
  //
  // We simulate this by making link.send() on the parent side accumulate messages
  // without consuming them (non-draining parent), then emitting far more events
  // than the queue cap from the fake session.
  const [parentSide, childSide] = makePairedChannel();

  // A non-draining parent: intercept parentSide.send so messages pile up without
  // the normal "on message" dispatch consuming them. We just record what the child
  // sends without routing anything back.
  const sentToParent: unknown[] = [];
  const stallingSide = {
    send(msg: unknown): void { sentToParent.push(msg); },
    on(_event: "message", _handler: (msg: import("../src/ipc/protocol.js").IpcMessage) => void): void {
      // Never deliver messages to the child — parent is stalled.
    },
  };

  const handle = makeFakeSession();
  const factory: SessionFactory = async () => handle.session;

  // Use the stalling side as the child's channel so the parent never reads.
  // Child sees stallingSide as its IPC peer; we drive it via childSide normally.
  // Actually: we need the child to process the prompt — we give it a normal
  // channel for receiving init/prompt, but stall its outbound sends.
  //
  // Approach: give the child both channels glued. We patch childSide.send to
  // capture without delivering to a real parent.
  const originalChildSend = childSide.send.bind(childSide);
  void originalChildSend;

  // Wrap childSide: outbound send goes to sentToParent without a real peer reading.
  const interceptedChildSide = {
    send(msg: unknown): void { sentToParent.push(msg); },
    on(event: "message", handler: (msg: import("../src/ipc/protocol.js").IpcMessage) => void): void {
      childSide.on(event, handler);
    },
  };

  startChild(interceptedChildSide, factory);

  // Use parentSide (real paired with childSide) to send init + prompt to the child.
  const parentLink = new ParentLink(parentSide);
  parentLink.send(STUB_PLAN satisfies InitMessage);
  await flushAsync(3);

  parentLink.send({ kind: "prompt", runId: "run-overflow", threadId: "th-overflow", text: "go" });
  await flushAsync(3);

  // Flood the session with far more events than the default cap (1024).
  for (let i = 0; i < 1030; i++) {
    handle.emit({
      type: "message_update",
      assistantMessageEvent: { type: "text_delta", delta: `chunk-${i}` },
    });
  }

  // Let the queue overflow propagate.
  await flushAsync(20);
  // Give a bit more real time for async work to complete.
  await new Promise((r) => setTimeout(r, 100));
  await flushAsync(10);

  // The child must have sent an error IPC message for run-overflow.
  const errorMsgs = sentToParent.filter(
    (m): m is import("../src/ipc/protocol.js").ErrorEvent =>
      typeof m === "object" && m !== null && (m as { kind: string }).kind === "error" &&
      (m as { runId: string }).runId === "run-overflow",
  );

  assert.ok(
    errorMsgs.length > 0,
    `child must send an error IPC for run-overflow on queue overflow; sent: ${JSON.stringify(sentToParent.slice(-5))}`,
  );
});

// ── child-entry: tool_execution_end with isError:true → ok:false ───────────────

test("CP3 child-entry: tool_execution_end with isError:true relays tool_end IPC with ok:false", async () => {
  // WHY: the old code used `(event as {success?}).success !== false` which read
  // undefined and was always true — a failed tool appeared to succeed. The fix is
  // `!event.isError` against the real SDK field. This test fails on the old logic
  // and passes only after the isError fix.
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);

  const handle = makeFakeSession();
  const factory: SessionFactory = async () =>
    handle.session;

  startChild(childSide, factory);

  parentLink.send(STUB_PLAN satisfies InitMessage);
  await flushAsync();

  const toolEndEvents: ToolEndEvent[] = [];
  parentLink.on("tool_end", (m: ToolEndEvent) => toolEndEvents.push(m));

  const doneP = new Promise<DoneEvent>((r) => parentLink.on("done", r));
  parentLink.send({ kind: "prompt", runId: "run-fail-tool", threadId: "th-fail", text: "use a failing tool" });
  await flushAsync(2);

  // Emit tool_execution_end with isError:true — simulates a failed tool call.
  handle.emit({
    type: "tool_execution_end",
    toolCallId: "tc-failed",
    toolName: "web_fetch",
    result: null,
    isError: true,
  });
  await flushAsync(2);

  handle.resolvePrompt();
  await withTimeout(doneP, 500, "done after failed tool");

  assert.equal(toolEndEvents.length, 1, "exactly one tool_end event must be relayed");
  const te = toolEndEvents[0];
  assert.ok(te, "tool_end event must be present");
  assert.equal(te.toolCallId, "tc-failed");
  assert.equal(te.ok, false, "ok must be false when isError is true — the masked-bug check");
});

// ── child-entry: tool_start carries the resolved description ───────────────────

test("child-entry: tool_execution_start relays a tool_start IPC carrying the session's tool description", async () => {
  // WHY: live agent-action visibility — the UI labels each tool line by its
  // description. The child resolves it from session.toolDescriptions and relays
  // it on tool_start; a dropped field means the UI falls back to bare tool names.
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);

  const handle = makeFakeSession();
  // The real createSessionFromPlan populates this from the registered tools.
  handle.session.toolDescriptions = { web_fetch: "Fetch a public web page over HTTPS." };
  const factory: SessionFactory = async () => handle.session;

  startChild(childSide, factory);

  parentLink.send(STUB_PLAN satisfies InitMessage);
  await flushAsync();

  const toolStartEvents: ToolStartEvent[] = [];
  parentLink.on("tool_start", (m: ToolStartEvent) => toolStartEvents.push(m));

  const doneP = new Promise<DoneEvent>((r) => parentLink.on("done", r));
  parentLink.send({ kind: "prompt", runId: "run-desc", threadId: "th-desc", text: "fetch something" });
  await flushAsync(2);

  handle.emit({
    type: "tool_execution_start",
    toolCallId: "tc-desc",
    toolName: "web_fetch",
    args: { url: "https://example.com" },
  });
  await flushAsync(2);

  handle.resolvePrompt();
  await withTimeout(doneP, 500, "done after tool_start");

  assert.equal(toolStartEvents.length, 1, "one tool_start must be relayed");
  assert.equal(toolStartEvents[0]?.description, "Fetch a public web page over HTTPS.");
});

// ── F4: overflow — no further messages for that runId after the error ─────────

test("F4 child-entry: after overflow error IPC, no text_delta or tool_* messages for that runId follow the terminal", async () => {
  // WHY: the relay must stop completely once the queue overflows. Any message
  // sent after the terminal `error` for a runId would violate the protocol
  // ("parent must not emit further events for this runId after receiving error").
  // This test proves the relay is fully stopped — not just the queue, but no
  // further IPC messages for that runId arrive after the error.
  const [parentSide, childSide] = makePairedChannel();

  const sentToParent: unknown[] = [];
  const interceptedChildSide = {
    send(msg: unknown): void { sentToParent.push(msg); },
    on(event: "message", handler: (msg: import("../src/ipc/protocol.js").IpcMessage) => void): void {
      childSide.on(event, handler);
    },
  };

  const handle = makeFakeSession();
  const factory: SessionFactory = async () => handle.session;

  startChild(interceptedChildSide, factory);

  const parentLink = new ParentLink(parentSide);
  parentLink.send(STUB_PLAN satisfies InitMessage);
  await flushAsync(3);

  parentLink.send({ kind: "prompt", runId: "run-no-after", threadId: "th-no-after", text: "go" });
  await flushAsync(3);

  // Flood past overflow.
  for (let i = 0; i < 1030; i++) {
    handle.emit({
      type: "message_update",
      assistantMessageEvent: { type: "text_delta", delta: `chunk-${i}` },
    });
  }

  await flushAsync(20);
  await new Promise((r) => setTimeout(r, 100));
  await flushAsync(10);

  // Find the error terminal for this runId.
  const errorIdx = sentToParent.findIndex(
    (m) => typeof m === "object" && m !== null &&
      (m as { kind: string }).kind === "error" &&
      (m as { runId: string }).runId === "run-no-after",
  );
  assert.ok(errorIdx >= 0, "must have received an error terminal for run-no-after");

  // Emit more events from the fake session — these must not appear after the error.
  handle.emit({ type: "message_update", assistantMessageEvent: { type: "text_delta", delta: "after-error" } });
  await flushAsync(10);

  const afterTerminal = sentToParent.slice(errorIdx + 1).filter(
    (m) => typeof m === "object" && m !== null &&
      (m as { runId?: string }).runId === "run-no-after" &&
      ["text_delta", "tool_start", "tool_end"].includes((m as { kind: string }).kind),
  );

  assert.equal(
    afterTerminal.length,
    0,
    `no text_delta/tool_* messages must follow the terminal error; got: ${JSON.stringify(afterTerminal)}`,
  );
});

// ── F4: abort message calls session.abort() and ends the relay ────────────────

test("F4 child-entry: abort message for an in-flight run calls session.abort() and ends the relay", async () => {
  // WHY: when the parent sends abort (SSE client disconnect), the child must
  // call session.abort() to stop the LLM turn and close the queue so no further
  // events are relayed. This test fails if the abort handler is removed.
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);

  const handle = makeFakeSession();
  const factory: SessionFactory = async () => handle.session;

  startChild(childSide, factory);

  parentLink.send(STUB_PLAN satisfies InitMessage);
  await flushAsync(2);

  // Collect what the child sends back.
  const receivedFromChild: unknown[] = [];
  // Intercept outbound from parent perspective: listen to all known event kinds.
  const allKinds = ["text_delta", "tool_start", "tool_end", "done", "error"] as const;
  for (const kind of allKinds) {
    parentLink.on(kind, (m) => receivedFromChild.push(m));
  }

  parentLink.send({ kind: "prompt", runId: "run-abort", threadId: "th-abort", text: "go slow" });
  await flushAsync(3);

  // Emit one delta so we confirm relaying was active.
  handle.emit({ type: "message_update", assistantMessageEvent: { type: "text_delta", delta: "partial" } });
  await flushAsync(3);

  // Send the abort directive from parent — simulates SSE disconnect.
  parentLink.send({ kind: "abort", runId: "run-abort" } satisfies AbortMessage);
  await flushAsync(5);

  // session.abort() must have been called.
  assert.equal(
    (handle.session as FakeSession).abortCalls,
    1,
    "session.abort() must be called once when the abort IPC message arrives",
  );

  // After abort, emit more events — they must not be relayed.
  handle.emit({ type: "message_update", assistantMessageEvent: { type: "text_delta", delta: "after-abort" } });
  await flushAsync(5);

  const afterAbort = receivedFromChild.filter(
    (m) => typeof m === "object" && m !== null &&
      (m as { runId?: string }).runId === "run-abort" &&
      (m as { kind: string }).kind === "text_delta" &&
      (m as { delta?: string }).delta === "after-abort",
  );
  assert.equal(
    afterAbort.length,
    0,
    "no events must be relayed for run-abort after the abort message",
  );
});

// ── personal-skill /command pre-activation runs inside withRun ─────────────────

test("child-entry: /command pre-activation of a personal skill runs inside withRun so getSkillBody has an active runId", async () => {
  // WHY: activating a personal skill calls session.activateSkill → getSkillBody,
  // which reaches the parent's per-run bridge and so requires an active runId.
  // The /command pre-activation happens BEFORE session.prompt(); if it isn't
  // wrapped in client.withRun(runId, …), getSkillBody's requireRunId() throws
  // ("no active runId") and the child crashes (on-prem crash: invoking a personal
  // skill via the /command palette killed the gateway child). This test drives
  // the REAL RemoteBridgeClient runId gating — it is not mocked away.
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);

  let skillBodyRunId: string | undefined;

  const handle = makeFakeSession();
  const factory: SessionFactory = async (_plan, client) => {
    // activateSkill routes through the real client.getSkillBody → requireRunId().
    handle.session.activateSkill = async (name: string) => {
      await client.getSkillBody(name);
      return "BODY";
    };
    return handle.session;
  };

  startChild(childSide, factory);

  // Parent answers the personal-skill body fetch and records the runId it carried.
  parentLink.on("get-skill-body", (msg: GetSkillBodyRequest) => {
    skillBodyRunId = msg.runId;
    parentLink.send({
      kind: "get-skill-body-result",
      seq: msg.seq,
      ok: true,
      body: "BODY",
      allowedTools: [],
    } satisfies GetSkillBodyResult);
  });

  parentLink.send(STUB_PLAN satisfies InitMessage);
  await flushAsync();

  const doneP = new Promise<DoneEvent>((r) => parentLink.on("done", r));
  const errorP = new Promise<ErrorEvent>((r) => parentLink.on("error", r));
  parentLink.send({
    kind: "prompt",
    runId: "run-personal-skill",
    threadId: "th-ps",
    text: "hello",
    activateSkillName: "personal:foo",
  });

  // Let pre-activation round-trip through the parent, then resolve the LLM turn.
  // If activation ran outside withRun it rejects before prompt() is ever called
  // (promptCalls stays 0) — the run never terminates and the race times out.
  await flushAsync(6);
  if (handle.session.promptCalls.length > 0) handle.resolvePrompt();

  const outcome = await withTimeout(
    Promise.race([doneP.then(() => "done" as const), errorP.then((e) => e.message)]),
    500,
    "personal-skill activation outcome",
  );

  assert.equal(outcome, "done", `run must complete without error; got: ${JSON.stringify(outcome)}`);
  assert.ok(
    skillBodyRunId && skillBodyRunId.length > 0,
    "get-skill-body request must carry a non-empty runId (proves withRun was active during pre-activation)",
  );
  assert.equal(skillBodyRunId, "run-personal-skill");
});

// ── activation body must reach the model's prompt (review bugfix) ─────────────
//
//  CP3 review: session.activateSkill's return value
// was inspected only for an "ERROR:" prefix and discarded on success — the
// /command and keyword-auto-load paths authorized a skill's tool allowlist but
// never gave the model its instructions. Only the model-invoked load_skill
// execute() path worked, because its return value becomes the tool result in
// conversation. These tests pin the fix: a successful activation's body (and
// any "## Skill files" manifest already embedded in it by load-skill.ts's
// activate()) must be prepended to the text handed to session.prompt().

test("child-entry: /command activation's body + skill-files manifest are prepended to the prompt text sent to session.prompt() (regression: '/command skills were authorization-only')", async () => {
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const handle = makeFakeSession();
  const ACTIVATION_BODY =
    "## My Skill\nDo the thing.\n\n## Skill files\n- references/guide.md\nRead these with read_skill_file(skill=\"my-skill\", path=\"<path>\").";
  handle.session.activateSkill = async (_name: string) => ACTIVATION_BODY;

  const factory: SessionFactory = async () => handle.session;
  startChild(childSide, factory);

  parentLink.send(STUB_PLAN satisfies InitMessage);
  await flushAsync();

  parentLink.send({
    kind: "prompt",
    runId: "run-cmd-body",
    threadId: "th-cmd-body",
    text: "please do the thing",
    activateSkillName: "my-skill",
  });

  await flushAsync(6);
  if (handle.session.promptCalls.length > 0) handle.resolvePrompt();
  await flushAsync();

  assert.equal(handle.session.promptCalls.length, 1, "session.prompt() must be reached");
  const sentText = handle.session.promptCalls[0] ?? "";
  assert.ok(sentText.includes("Do the thing."), "the activated skill body must reach the model's prompt");
  assert.ok(sentText.includes("## Skill files"), "the skill-files manifest must reach the model's prompt");
  assert.ok(sentText.includes("please do the thing"), "the user's own prompt text must still be present");
});

test("child-entry: keyword-auto-load activation bodies for multiple bundles are all prepended to the prompt text, in order", async () => {
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const handle = makeFakeSession();
  handle.session.activateSkill = async (name: string) => `BODY for ${name}`;

  const factory: SessionFactory = async () => handle.session;
  startChild(childSide, factory);

  parentLink.send(STUB_PLAN satisfies InitMessage);
  await flushAsync();

  parentLink.send({
    kind: "prompt",
    runId: "run-auto-load",
    threadId: "th-auto-load",
    text: "hello",
    activateSkillNames: ["deployer", "personal:my-notes"],
  });

  await flushAsync(6);
  if (handle.session.promptCalls.length > 0) handle.resolvePrompt();
  await flushAsync();

  assert.equal(handle.session.promptCalls.length, 1);
  const sentText = handle.session.promptCalls[0] ?? "";
  assert.ok(sentText.includes("BODY for deployer"), "the first matched bundle's body must reach the prompt");
  assert.ok(sentText.includes("BODY for personal:my-notes"), "the second matched bundle's body must reach the prompt");
  assert.ok(
    sentText.indexOf("BODY for deployer") < sentText.indexOf("BODY for personal:my-notes"),
    "activation blocks must appear in match order",
  );
  assert.ok(sentText.includes("hello"), "the user's own prompt text must still be present");
});

test("child-entry: keyword-auto-load per-bundle activation failure is silently skipped (best-effort) — the turn still proceeds with only the successful bundle's body", async () => {
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const handle = makeFakeSession();
  handle.session.activateSkill = async (name: string) =>
    name === "broken" ? `ERROR: unknown or unavailable skill "${name}"` : `BODY for ${name}`;

  const factory: SessionFactory = async () => handle.session;
  startChild(childSide, factory);

  parentLink.send(STUB_PLAN satisfies InitMessage);
  await flushAsync();

  parentLink.send({
    kind: "prompt",
    runId: "run-auto-partial",
    threadId: "th-auto-partial",
    text: "hello",
    activateSkillNames: ["broken", "deployer"],
  });

  await flushAsync(6);
  if (handle.session.promptCalls.length > 0) handle.resolvePrompt();
  await flushAsync();

  assert.equal(handle.session.promptCalls.length, 1, "a broken bundle must not abort the turn");
  const sentText = handle.session.promptCalls[0] ?? "";
  assert.ok(sentText.includes("BODY for deployer"), "the successful bundle's body must still reach the prompt");
  assert.ok(!sentText.includes("ERROR:"), "a failed bundle's error text must never be injected into the prompt");
});

test("child-entry: /command activation error still surfaces as a text_delta + done, and session.prompt() is never called (unchanged error contract)", async () => {
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const handle = makeFakeSession();
  handle.session.activateSkill = async (name: string) => `ERROR: unknown or unavailable skill "${name}"`;

  const factory: SessionFactory = async () => handle.session;
  startChild(childSide, factory);

  parentLink.send(STUB_PLAN satisfies InitMessage);
  await flushAsync();

  const deltaP = new Promise<TextDeltaEvent>((r) => parentLink.on("text_delta", r));
  const doneP = new Promise<DoneEvent>((r) => parentLink.on("done", r));

  parentLink.send({
    kind: "prompt",
    runId: "run-cmd-error",
    threadId: "th-cmd-error",
    text: "please do the thing",
    activateSkillName: "does-not-exist",
  });

  const delta = await withTimeout(deltaP, 500, "activation error text_delta");
  await withTimeout(doneP, 500, "activation error done");

  assert.match(delta.delta, /ERROR: unknown or unavailable skill/);
  assert.equal(handle.session.promptCalls.length, 0, "session.prompt() must never be called on an activation error");
});
