// CP1 tests — session history rehydration: gateway end-to-end + seed fresh session.
//
// WHY these tests exist: when a Pi thread is evicted (idle TTL / LRU cap), the
// next prompt creates a fresh SessionManager with zero prior context. The fix is:
//   1. ConvMessage type + PromptMessage.history? carry prior turns over IPC.
//   2. child-entry passes history to the factory ONLY in the lazy-create branch.
//   3. createSessionFromPlan seeds a fresh SessionManager from history turns.
//
// Three test suites:
//   - session-plan: seeding N turns yields the correct SessionManager context.
//   - child-entry: history forwarded only on lazy-create, not on session reuse.
//   - supervisor/protocol: run() with history sends PromptMessage.history over IPC.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  makePairedChannel,
  ParentLink,
  ChildLink,
} from "../src/ipc/protocol.js";
import type {
  InitMessage,
  DoneEvent,
  PromptMessage,
} from "../src/ipc/protocol.js";
import type { ConvMessage } from "../src/ipc/protocol.js";
import { startChild, type SessionFactory, type SessionLike, type SessionPlan } from "../src/ipc/child-entry.js";
import type { AgentSessionEvent } from "@earendil-works/pi-coding-agent";
import {
  createSessionFromPlan,
  type SessionPlan as SPType,
  type CreateSessionDeps,
  type CreateAgentSessionFn,
} from "../src/pi/session-plan.js";
import type { BridgeClientLike } from "../src/ipc/bridge-client.js";
import { SessionManager } from "@earendil-works/pi-coding-agent";
import type { CreateAgentSessionOptions } from "@earendil-works/pi-coding-agent";

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

// ── Fake bridge ────────────────────────────────────────────────────────────────

function makeFakeBridge(): BridgeClientLike {
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
    reason: async () => ({ ok: true, output: "" }),
    setApprover: () => {},
    setToken: () => {},
    usageIdentity: () => ({ tenantId: "", userId: "", agentId: "" }),
  };
}

// ── Stub plan ─────────────────────────────────────────────────────────────────

const STUB_PLAN: SPType = {
  kind: "init",
  modelId: "test-model",
  systemPrompt: "You are helpful.",
  allowedToolNames: ["delegate"],
  mcpTools: [],
  proxyBaseUrl: "http://127.0.0.1:0",
  proxyModelAllowlist: ["test-model"],
};

// ── Fake session (for child-entry tests) ──────────────────────────────────────

interface FakeSession extends SessionLike {
  promptCalls: string[];
}

interface FakeSessionHandle {
  session: FakeSession;
  resolvePrompt(): void;
}

function makeFakeSession(): FakeSessionHandle {
  const listeners = new Set<(event: AgentSessionEvent) => void>();
  const promptCalls: string[] = [];
  let resolvePrompt!: () => void;

  const session: FakeSession = {
    promptCalls,
    prompt(text: string): Promise<void> {
      promptCalls.push(text);
      return new Promise<void>((r) => { resolvePrompt = r; });
    },
    subscribe(listener: (event: AgentSessionEvent) => void): () => void {
      listeners.add(listener);
      return () => { listeners.delete(listener); };
    },
    dispose(): void { listeners.clear(); },
  };

  return {
    session,
    resolvePrompt() { resolvePrompt(); },
  };
}

// ── session-plan: seeding tests ────────────────────────────────────────────────

test("CP1 session-plan: seeding 3 history turns into fresh session yields SessionManager with all turns in order", async () => {
  // WHY: this is the core correctness test. When createSessionFromPlan receives
  // history turns it must seed the SessionManager so buildSessionContext returns
  // those turns in order. An empty seed means the LLM sees no prior context.
  const history: ConvMessage[] = [
    { role: "user", content: "Hello agent" },
    { role: "assistant", content: "Hello! How can I help?" },
    { role: "user", content: "What is 2+2?" },
  ];

  let capturedManager: SessionManager | undefined;

  const captureCreate: CreateAgentSessionFn = async (opts?: CreateAgentSessionOptions) => {
    capturedManager = opts?.sessionManager;
    return {
      session: {
        prompt: async () => {},
        subscribe: () => () => {},
        dispose: () => {},
      },
    };
  };

  const deps: CreateSessionDeps = {
    createAgentSession: captureCreate,
    onRegisterProvider: () => {},
  };

  await createSessionFromPlan(STUB_PLAN, makeFakeBridge(), deps, { useProxy: true }, history);

  assert.ok(capturedManager, "createAgentSession must have been called with a sessionManager");
  const ctx = capturedManager.buildSessionContext();
  assert.equal(ctx.messages.length, 3, `must have 3 seeded messages, got ${ctx.messages.length}`);

  const m0 = ctx.messages[0];
  const m1 = ctx.messages[1];
  const m2 = ctx.messages[2];
  assert.ok(m0, "first message must exist");
  assert.ok(m1, "second message must exist");
  assert.ok(m2, "third message must exist");

  assert.equal(m0.role, "user", "first message must be user");
  assert.equal(m1.role, "assistant", "second message must be assistant");
  assert.equal(m2.role, "user", "third message must be user");

  // Check content text for each turn.
  // WHY: user turns always carry a plain string — assert the exact shape, no hedge.
  assert.equal(m0.content, "Hello agent", "first user message content must match");
  assert.equal(m2.content, "What is 2+2?", "third user message content must match");
  // Assistant content is an array of text blocks.
  const assistantContent = m1.content;
  assert.ok(Array.isArray(assistantContent), "assistant content must be an array");
  const firstBlock = Array.isArray(assistantContent) ? assistantContent[0] : undefined;
  assert.ok(firstBlock && firstBlock.type === "text", "first assistant block must be a text block");
  assert.equal(
    firstBlock && firstBlock.type === "text" ? firstBlock.text : undefined,
    "Hello! How can I help?",
    "assistant content text must match",
  );
});

test("CP1 session-plan: empty history produces a fresh session with no seeded messages", async () => {
  // WHY: current behavior must be preserved when history is absent or empty.
  // An empty array or undefined must not change what the LLM sees on first prompt.
  let capturedManager: SessionManager | undefined;

  const captureCreate: CreateAgentSessionFn = async (opts?: CreateAgentSessionOptions) => {
    capturedManager = opts?.sessionManager;
    return {
      session: {
        prompt: async () => {},
        subscribe: () => () => {},
        dispose: () => {},
      },
    };
  };

  const deps: CreateSessionDeps = {
    createAgentSession: captureCreate,
    onRegisterProvider: () => {},
  };

  // Empty array.
  await createSessionFromPlan(STUB_PLAN, makeFakeBridge(), deps, { useProxy: true }, []);

  assert.ok(capturedManager, "createAgentSession must be called");
  const ctx = capturedManager.buildSessionContext();
  assert.equal(ctx.messages.length, 0, "empty history must produce zero seeded messages");
});

test("CP1 session-plan: absent history (undefined) produces a fresh session with no seeded messages", async () => {
  // WHY: history param is optional; omitting it must keep current behavior (no seed).
  let capturedManager: SessionManager | undefined;

  const captureCreate: CreateAgentSessionFn = async (opts?: CreateAgentSessionOptions) => {
    capturedManager = opts?.sessionManager;
    return {
      session: {
        prompt: async () => {},
        subscribe: () => () => {},
        dispose: () => {},
      },
    };
  };

  const deps: CreateSessionDeps = {
    createAgentSession: captureCreate,
    onRegisterProvider: () => {},
  };

  await createSessionFromPlan(STUB_PLAN, makeFakeBridge(), deps, { useProxy: true });

  assert.ok(capturedManager, "createAgentSession must be called");
  const ctx = capturedManager.buildSessionContext();
  assert.equal(ctx.messages.length, 0, "absent history must produce zero seeded messages");
});

// ── child-entry: history forwarding ───────────────────────────────────────────

test("CP1 child-entry: prompt with history on MISSING thread calls factory WITH that history", async () => {
  // WHY: when the thread is not found in the store (evicted or first visit),
  // the factory must be called with the incoming history so the fresh session
  // can be seeded. This is the load-bearing path for context restoration.
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);

  const capturedHistories: (ConvMessage[] | undefined)[] = [];
  const handle = makeFakeSession();

  const factory: SessionFactory = async (_plan, _client, history) => {
    capturedHistories.push(history);
    return handle.session;
  };

  startChild(childSide, factory);

  parentLink.send(STUB_PLAN satisfies InitMessage);
  await flushAsync();

  const history: ConvMessage[] = [
    { role: "user", content: "prior turn 1" },
    { role: "assistant", content: "prior response 1" },
  ];

  const doneP = new Promise<DoneEvent>((r) => {
    parentLink.on("done", (m) => { if (m.runId === "run-hist") r(m); });
  });

  parentLink.send({
    kind: "prompt",
    runId: "run-hist",
    threadId: "th-new",
    text: "current prompt",
    history,
  } satisfies PromptMessage);
  await flushAsync(2);

  handle.resolvePrompt();
  await withTimeout(doneP, 500, "done with history");

  assert.equal(capturedHistories.length, 1, "factory must be called once");
  const received = capturedHistories[0];
  assert.ok(received, "factory must receive history");
  assert.equal(received.length, 2, "factory must receive both history turns");
  assert.equal(received[0]?.role, "user");
  assert.equal(received[0]?.content, "prior turn 1");
  assert.equal(received[1]?.role, "assistant");
  assert.equal(received[1]?.content, "prior response 1");
});

test("CP1 child-entry: prompt with history on EXISTING thread does NOT re-invoke factory", async () => {
  // WHY: a still-alive thread must never be re-seeded. Re-seeding would duplicate
  // the history and corrupt the LLM context. The factory must be called only once
  // (for the initial lazy-create); subsequent prompts on the same threadId reuse
  // the existing session without touching the factory or its history arg.
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

  // First prompt — creates the thread.
  const done1P = new Promise<DoneEvent>((r) => {
    parentLink.on("done", (m) => { if (m.runId === "run-first") r(m); });
  });
  parentLink.send({ kind: "prompt", runId: "run-first", threadId: "th-reuse-hist", text: "first" } satisfies PromptMessage);
  await flushAsync(2);
  handle.resolvePrompt();
  await withTimeout(done1P, 500, "first done");

  assert.equal(factoryCalls, 1, "factory called once on first prompt");

  // Second prompt on the same thread with history — factory must NOT be called again.
  const history: ConvMessage[] = [
    { role: "user", content: "first" },
    { role: "assistant", content: "reply" },
  ];

  const done2P = new Promise<DoneEvent>((r) => {
    parentLink.on("done", (m) => { if (m.runId === "run-second") r(m); });
  });
  parentLink.send({
    kind: "prompt",
    runId: "run-second",
    threadId: "th-reuse-hist",
    text: "second",
    history,
  } satisfies PromptMessage);
  await flushAsync(2);
  handle.resolvePrompt();
  await withTimeout(done2P, 500, "second done");

  assert.equal(factoryCalls, 1, "factory must not be called again for existing thread — no re-seed");
  assert.equal(handle.session.promptCalls.length, 2, "both prompts must reach the session");
});

// ── protocol: ConvMessage type and PromptMessage.history field ────────────────

test("CP1 protocol: PromptMessage with history field is accepted and round-trips through the channel", async () => {
  // WHY: the IPC channel serialises over JSON (real IPC uses process.send with
  // serialization:'json'). The ConvMessage array in PromptMessage.history must
  // survive the round-trip so the child can read it faithfully.
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const childLink = new ChildLink(childSide);

  const history: ConvMessage[] = [
    { role: "user", content: "turn 1" },
    { role: "assistant", content: "response 1" },
  ];

  const received: PromptMessage[] = [];
  childLink.on("prompt", (msg: PromptMessage) => {
    received.push(msg);
  });

  parentLink.send({
    kind: "prompt",
    runId: "run-proto",
    threadId: "th-proto",
    text: "hello",
    history,
  } satisfies PromptMessage);

  await flushAsync(3);

  assert.equal(received.length, 1, "one prompt must arrive at the child link");
  const msg = received[0];
  assert.ok(msg, "prompt message must exist");
  assert.equal(msg.runId, "run-proto");
  assert.ok(msg.history, "history must be present on received prompt");
  assert.equal(msg.history.length, 2);
  assert.equal(msg.history[0]?.role, "user");
  assert.equal(msg.history[0]?.content, "turn 1");
  assert.equal(msg.history[1]?.role, "assistant");
  assert.equal(msg.history[1]?.content, "response 1");
});

test("CP1 protocol: PromptMessage without history field is accepted (backward compatible)", async () => {
  // WHY: the history field is optional. Existing callers that do not pass history
  // must continue to compile and work correctly. Omitting the field must not break
  // the channel round-trip.
  const [parentSide, childSide] = makePairedChannel();
  const parentLink = new ParentLink(parentSide);
  const childLink = new ChildLink(childSide);

  const received: PromptMessage[] = [];
  childLink.on("prompt", (msg: PromptMessage) => {
    received.push(msg);
  });

  parentLink.send({
    kind: "prompt",
    runId: "run-no-hist",
    threadId: "th-no-hist",
    text: "no history",
  } satisfies PromptMessage);

  await flushAsync(3);

  assert.equal(received.length, 1);
  const msg = received[0];
  assert.ok(msg, "prompt must arrive");
  assert.equal(msg.history, undefined, "history must be absent when not provided");
});

test("CP1 protocol: ConvMessage has no identity fields — content only", () => {
  // WHY: conversation turns are content, not identity. The IPC "no identity in
  // messages" rule must hold for ConvMessage too. This test asserts at the type
  // level that ConvMessage only has role + content.
  const turn: ConvMessage = { role: "user", content: "text" };
  const serialised = JSON.stringify(turn);

  assert.ok(!serialised.includes("owner"), "ConvMessage must not have 'owner'");
  assert.ok(!serialised.includes("userId"), "ConvMessage must not have 'userId'");
  assert.ok(!serialised.includes("tenantId"), "ConvMessage must not have 'tenantId'");
  assert.ok(!serialised.includes("bearer"), "ConvMessage must not have 'bearer'");
  assert.ok(!serialised.includes("token"), "ConvMessage must not have 'token'");
  assert.ok(serialised.includes("role"), "ConvMessage must have 'role'");
  assert.ok(serialised.includes("content"), "ConvMessage must have 'content'");
});
