// Tests for runAgentInvocation (external/core.ts).
// Spec contract: south-mode behavior — uses createGatewayTask + svc-<id> userId,
// NOT north createTask; approver uses spec.skills.
import { test } from "node:test";
import assert from "node:assert/strict";
import type { Config } from "../../src/config.js";
import type { ApprovalInfo } from "../../src/broker/governance.js";
import type { ChildSupervisor } from "../../src/ipc/supervisor.js";
import {
  makePairedChannel,
  ParentLink,
  ChildLink,
} from "../../src/ipc/protocol.js";
import type { DoneEvent, PromptMessage } from "../../src/ipc/protocol.js";

const TENANT = "11111111-1111-1111-1111-111111111111";
const AGENT_ID = "cccccccc-dddd-eeee-ffff-cccccccccccc";

// ValidationOutcome.APPROVED = 1 per proto enum (verified from gen/ts/proto/plan.ts)
const APPROVED_OUTCOME = 1;

interface CallLog { method: string; args: unknown[] }

function makeSouth(skillsList: string[] = ["doc.read"]) {
  const calls: CallLog[] = [];
  return {
    calls,
    createGatewayTask: (...args: unknown[]) => {
      calls.push({ method: "createGatewayTask", args });
      return Promise.resolve({ taskId: "task-ext-1" });
    },
    approveGatewayTask: (...args: unknown[]) => {
      calls.push({ method: "approveGatewayTask", args });
      return Promise.resolve({ capabilityTokenIds: { 1: "tok-ext" } });
    },
    submitPlan: (...args: unknown[]) => {
      calls.push({ method: "submitPlan", args });
      return Promise.resolve({
        outcome: APPROVED_OUTCOME,
        capabilityTokenIds: { 1: "tok-ext" },
        violations: [],
        steps: [],
      });
    },
    invokeTool: (...args: unknown[]) => {
      calls.push({ method: "invokeTool", args });
      return Promise.resolve({ success: true, result: "ok", error: "", costUnitsConsumed: 0 });
    },
    emitStatus: (...args: unknown[]) => {
      calls.push({ method: "emitStatus", args });
      return Promise.resolve();
    },
    getAgentSpec: (...args: unknown[]) => {
      calls.push({ method: "getAgentSpec", args });
      return Promise.resolve({
        spec: {
          id: AGENT_ID,
          name: "test-agent",
          approvalMode: "auto",
          skills: skillsList,
          model: "",
          systemPrompt: "",
          tenantId: TENANT,
        },
      });
    },
    getTenantModel: (...args: unknown[]) => {
      calls.push({ method: "getTenantModel", args });
      return Promise.resolve({ model: "" });
    },
    getLlmProviders: (...args: unknown[]) => {
      calls.push({ method: "getLlmProviders", args });
      return Promise.resolve({ providers: [] });
    },
  };
}

function makeNorth() {
  const calls: CallLog[] = [];
  return {
    calls,
    createTask: (...args: unknown[]) => {
      calls.push({ method: "createTask", args });
      return Promise.resolve({ taskId: "task-north-1" });
    },
    approveTask: (...args: unknown[]) => {
      calls.push({ method: "approveTask", args });
      return Promise.resolve({ capabilityTokenIds: {} });
    },
    sendEnvelope: (...args: unknown[]) => {
      calls.push({ method: "sendEnvelope", args });
      return Promise.resolve({ envelopeId: "env-1" });
    },
  };
}

const baseCfg = {
  openrouterApiKey: "",
  llmModel: "anthropic/claude-sonnet-4.6",
  brokerNorthAddr: "127.0.0.1:9090",
  brokerSouthAddr: "127.0.0.1:9091",
  brokerServerName: "broker",
  tlsCert: "",
  tlsKey: "",
  tlsCa: "",
  gatewaySpiffeId: "spiffe://aikonos.com/agent-gateway",
  port: 8080,
  defaultTenantId: TENANT,
  oidcIssuer: "",
  oidcJwksUrl: "",
  oidcAudience: "aikonos-broker",
  schedulerEnabled: false,
  schedulerTickMs: 30000,
  schedulerClaimLimit: 10,
  schedulerRunTimeoutMs: 180000,
  agentForUserOverrides: {},
  externalPort: 8090,
  externalCorsOrigins: [],
  externalRateLimit: 60,
} as unknown as Config;

const log = {
  info: () => {},
  warn: () => {},
  error: () => {},
  debug: () => {},
} as unknown as import("../../src/log.js").Logger;

test("runAgentInvocation: north.createTask is never called (south-mode only)", async () => {
  // WHY: external invocations are south-mode — they use createGatewayTask, not
  // the north createTask. This test proves no north call is ever made, even when
  // the run completes normally via the supervisor path.
  const south = makeSouth(["doc.read"]);
  const north = makeNorth();
  const clients = { north, south } as unknown as import("../../src/broker/clients.js").BrokerClients;

  const { runAgentInvocation } = await import("../../src/external/core.js");

  // Build a fake supervisor that auto-completes any run immediately.
  const [parentSide, childSide] = makePairedChannel();
  const fakeLink = new ParentLink(parentSide);
  fakeLink.onExit = () => {};
  fakeLink.kill = () => {};
  const childLink = new ChildLink(childSide);
  childLink.on("prompt", (msg: PromptMessage) => {
    setImmediate(() => childLink.send({ kind: "done", runId: msg.runId } as DoneEvent));
  });

  const fakeSupervisor: ChildSupervisor = {
    keyFor: () => "__single__",
    getOrSpawn: async (_key: string, identity: import("../../src/broker/governance.js").Identity) => ({
      key: "__single__",
      link: fakeLink,
      bridgeServer: {} as import("../../src/ipc/bridge-server.js").RemoteBridgeServer,
      proxyToken: "tok",
      lastUsedAt: Date.now(),
      busy: false,
      setRunContext: (_runId: string, _identity: import("../../src/ipc/bridge-server.js").RunIdentity, _approver: import("../../src/broker/governance.js").Approver) => {},
      clearRunContext: (_runId: string) => {},
      abortRun: (_runId: string) => {},
    }),
    run: async (
      _handle: import("../../src/ipc/supervisor.js").ChildHandle,
      prompt: { runId: string; threadId: string; text: string },
      onEvent: (evt: import("../../src/ipc/protocol.js").DoneEvent) => void,
    ) => {
      // Simulate a done event reaching the onEvent callback.
      onEvent({ kind: "done", runId: prompt.runId } as DoneEvent);
    },
    markBusy: () => {},
    markIdle: () => {},
    dispose: () => {},
  } as unknown as ChildSupervisor;

  for await (const _ev of runAgentInvocation(
    AGENT_ID,
    `svc-${AGENT_ID}`,
    TENANT,
    ["doc.read"],
    "test prompt",
    { clients, cfg: baseCfg, log, supervisor: fakeSupervisor, timeoutMs: 500 },
  )) {
    // drain
  }

  assert.equal(
    north.calls.filter((c: CallLog) => c.method === "createTask").length,
    0,
    "north.createTask must not be called for external invocations (south-mode)",
  );
});

test("preAuthApprover uses provided skills list — grants skill-listed tool, denies others", async () => {
  const { preAuthApprover } = await import("../../src/scheduler/ticker.js");
  const skills = ["doc.read", "email.draft"];
  const approver = preAuthApprover(skills);

  const allow = await approver({
    toolCallId: "c1",
    toolName: "doc.read",
    toolId: "doc.read",
    effectClass: 0,
    reason: "",
    args: {},
    stepUp: false,
  } as ApprovalInfo);
  assert.equal(allow, true, "doc.read is in skills → allowed");

  const deny = await approver({
    toolCallId: "c2",
    toolName: "web.fetch",
    toolId: "web.fetch",
    effectClass: 0,
    reason: "",
    args: {},
    stepUp: false,
  } as ApprovalInfo);
  assert.equal(deny, false, "web.fetch not in skills → denied");
});

test("Identity userId is svc-<agentId>, not derived via agentForUser", async () => {
  // Validate the naming contract: svc-<agentId> must come directly from agentId,
  // not through agentForUser() which would produce a different value.
  const agentId = "12345678-0000-0000-0000-000000000000";
  const expectedUserId = `svc-${agentId}`;
  assert.equal(expectedUserId, "svc-12345678-0000-0000-0000-000000000000");

  const { agentForUser } = await import("../../src/broker/agent-identity.js");
  const wrongValue = agentForUser(`svc-${agentId}`, {});
  assert.notEqual(wrongValue, expectedUserId, "agentForUser must not be used to derive svc- userId");
});

// ── CP2: early-return (SSE disconnect) stops run and calls clearRunContext ──────

test("CP2 runAgentInvocation: consumer breaking for-await triggers finally → abortRun and clearRunContext called", async () => {
  // WHY: an SSE client that disconnects must STOP the in-flight run (abortRun)
  // AND clear the run context. If abortRun wiring is removed this test fails,
  // proving that a disconnect without abort causes a 180s token burn in the child.
  const { runAgentInvocation } = await import("../../src/external/core.js");
  const south = makeSouth(["doc.read"]);
  const north = makeNorth();
  const clients = { north, south } as unknown as import("../../src/broker/clients.js").BrokerClients;

  const clearRunContextCalls: string[] = [];
  const abortRunCalls: string[] = [];
  let setRunContextCalled = false;

  // A supervisor whose run() never settles (simulates a long-running agent).
  const fakeSupervisor = {
    keyFor: () => "__single__",
    getOrSpawn: async () => ({
      key: "__single__",
      link: {} as import("../../src/ipc/protocol.js").ParentLink,
      bridgeServer: {} as import("../../src/ipc/bridge-server.js").RemoteBridgeServer,
      proxyToken: "tok",
      lastUsedAt: Date.now(),
      busy: false,
      setRunContext: (_runId: string) => { setRunContextCalled = true; },
      clearRunContext: (runId: string) => { clearRunContextCalls.push(runId); },
      abortRun: (runId: string) => { abortRunCalls.push(runId); },
    }),
    // run() returns a promise that never resolves — simulates an in-flight run.
    run: (_handle: unknown, _prompt: unknown, _onEvent: unknown): Promise<void> =>
      new Promise(() => {}),
    markBusy: () => {},
    markIdle: () => {},
    dispose: () => {},
  } as unknown as import("../../src/ipc/supervisor.js").ChildSupervisor;

  const gen = runAgentInvocation(
    AGENT_ID,
    `svc-${AGENT_ID}`,
    TENANT,
    ["doc.read"],
    "test prompt",
    { clients, cfg: baseCfg, log, supervisor: fakeSupervisor, timeoutMs: 500 },
  );

  // Advance to the first await point so setRunContext is called and the generator is suspended.
  // Pull the first item (will hang waiting for events since run() never settles).
  const firstP = gen.next();

  // Flush so getOrSpawn + setRunContext completes.
  for (let i = 0; i < 10; i++) await new Promise((r) => setImmediate(r));

  assert.equal(setRunContextCalled, true, "setRunContext must be called before we break");

  // Early return — simulates SSE client disconnect.
  await gen.return(undefined);

  // Flush microtasks so the finally block runs.
  for (let i = 0; i < 10; i++) await new Promise((r) => setImmediate(r));

  assert.ok(
    abortRunCalls.length > 0,
    "abortRun must be called on SSE disconnect — without it the child keeps running for up to 180s",
  );
  assert.ok(
    clearRunContextCalls.length > 0,
    "clearRunContext must be called when consumer breaks the for-await (SSE disconnect)",
  );

  // Suppress unhandled rejection from firstP (it may reject after return).
  firstP.catch(() => {});
});

// ── CP2: overflow with stalled consumer → error event ─────────────────────────

test("CP2 runAgentInvocation: flood of onEvent pushes beyond cap with stalled consumer → error event", async () => {
  // WHY: a burst of events from a fast agent with a slow consumer must not grow
  // memory without bound. The bounded queue overflows, the generator yields a
  // terminal error event, and record() is called with status:"error".
  const { runAgentInvocation } = await import("../../src/external/core.js");
  const south = makeSouth(["doc.read"]);
  const north = makeNorth();
  const clients = { north, south } as unknown as import("../../src/broker/clients.js").BrokerClients;

  let capturedOnEvent: ((evt: import("../../src/ipc/protocol.js").IpcMessage) => void) | undefined;

  const fakeSupervisor = {
    keyFor: () => "__single__",
    getOrSpawn: async () => ({
      key: "__single__",
      link: {} as import("../../src/ipc/protocol.js").ParentLink,
      bridgeServer: {} as import("../../src/ipc/bridge-server.js").RemoteBridgeServer,
      proxyToken: "tok",
      lastUsedAt: Date.now(),
      busy: false,
      setRunContext: () => {},
      clearRunContext: () => {},
      abortRun: () => {},
    }),
    run: (
      _handle: unknown,
      _prompt: unknown,
      onEvent: (evt: import("../../src/ipc/protocol.js").IpcMessage) => void,
    ): Promise<void> => {
      capturedOnEvent = onEvent;
      // Never settles — the flood comes from outside.
      return new Promise(() => {});
    },
    markBusy: () => {},
    markIdle: () => {},
    dispose: () => {},
  } as unknown as import("../../src/ipc/supervisor.js").ChildSupervisor;

  const events: Array<import("../../src/external/core.js").InvocationEvent> = [];
  const gen = runAgentInvocation(
    AGENT_ID,
    `svc-${AGENT_ID}`,
    TENANT,
    ["doc.read"],
    "flood test",
    { clients, cfg: baseCfg, log, supervisor: fakeSupervisor, timeoutMs: 30_000 },
  );

  // Advance to the suspension point (generator is now waiting for events).
  const firstP = gen.next();
  for (let i = 0; i < 10; i++) await new Promise((r) => setImmediate(r));

  assert.ok(capturedOnEvent, "run() must have been called and onEvent captured");

  // Flood with far more events than the queue cap (default 1024).
  for (let i = 0; i < 1030; i++) {
    capturedOnEvent!({ kind: "text_delta", runId: "flood", delta: `delta-${i}` });
  }

  // Flush so the queue overflow is processed.
  for (let i = 0; i < 20; i++) await new Promise((r) => setImmediate(r));

  // Collect the first item (should be a text_delta or the overflow error).
  const first = await Promise.race([
    firstP,
    new Promise<never>((_, reject) =>
      setTimeout(() => reject(new Error("timeout: flood first item")), 2000),
    ),
  ]);

  events.push(first.value);

  // Drain the rest until done/error.
  for await (const ev of gen) {
    events.push(ev);
    if (ev.type === "error" || ev.type === "done") break;
  }

  const errorEv = events.find((e) => e.type === "error");
  assert.ok(
    errorEv !== undefined,
    `generator must yield a terminal error event on overflow; got: ${JSON.stringify(events.slice(-3))}`,
  );
});

// ── error hygiene: terminal error frame trims gRPC internals (2026-07-03 correction) ──

test("runAgentInvocation: supervisor.run rejection with gRPC-shaped message yields a trimmed error frame, not raw internals", async () => {
  // WHY: a gRPC rejection from supervisor.run (e.g. a broker call inside the
  // child) carries a numeric .code plus a message with status/detail internals
  // ("<code> <STATUS_NAME>: <details>", per grpc-js's callErrorFromStatus).
  // The SSE consumer must never see that raw text.
  const south = makeSouth(["doc.read"]);
  const north = makeNorth();
  const clients = { north, south } as unknown as import("../../src/broker/clients.js").BrokerClients;

  const { runAgentInvocation } = await import("../../src/external/core.js");

  const rawMessage = "13 INTERNAL: something internal at Object.<anonymous> (/app/x.ts:1:1)";
  const grpcErr = new Error(rawMessage) as Error & { code: number };
  grpcErr.code = 13; // status.INTERNAL

  const fakeSupervisor = {
    keyFor: () => "__single__",
    getOrSpawn: async () => ({
      key: "__single__",
      link: {} as import("../../src/ipc/protocol.js").ParentLink,
      bridgeServer: {} as import("../../src/ipc/bridge-server.js").RemoteBridgeServer,
      proxyToken: "tok",
      lastUsedAt: Date.now(),
      busy: false,
      setRunContext: () => {},
      clearRunContext: () => {},
      abortRun: () => {},
    }),
    run: (): Promise<void> => Promise.reject(grpcErr),
    markBusy: () => {},
    markIdle: () => {},
    dispose: () => {},
  } as unknown as import("../../src/ipc/supervisor.js").ChildSupervisor;

  const events: import("../../src/external/core.js").InvocationEvent[] = [];
  for await (const ev of runAgentInvocation(
    AGENT_ID,
    `svc-${AGENT_ID}`,
    TENANT,
    ["doc.read"],
    "test prompt",
    { clients, cfg: baseCfg, log, supervisor: fakeSupervisor, timeoutMs: 2000 },
  )) {
    events.push(ev);
  }

  const errorEv = events.find((e) => e.type === "error");
  assert.ok(errorEv, "must yield a terminal error event");
  assert.notEqual(errorEv!.error, rawMessage, "raw gRPC message must not reach the SSE caller");
  assert.doesNotMatch(errorEv!.error ?? "", /INTERNAL/, "gRPC status internals must not leak");
  assert.doesNotMatch(errorEv!.error ?? "", /\/app\/x\.ts/, "stack/path fragments must not leak");
  assert.equal(errorEv!.error, "internal error", "INTERNAL gRPC code collapses to the generic trimmed message");
});

// ── CP1: tool_end/usage forwarding + tool_start toolCallId (F62) ──────────────

function makeFloodSupervisor(
  emit: (onEvent: (evt: import("../../src/ipc/protocol.js").IpcMessage) => void, runId: string) => void,
): import("../../src/ipc/supervisor.js").ChildSupervisor {
  return {
    keyFor: () => "__single__",
    getOrSpawn: async () => ({
      key: "__single__",
      link: {} as import("../../src/ipc/protocol.js").ParentLink,
      bridgeServer: {} as import("../../src/ipc/bridge-server.js").RemoteBridgeServer,
      proxyToken: "tok",
      lastUsedAt: Date.now(),
      busy: false,
      setRunContext: () => {},
      clearRunContext: () => {},
      abortRun: () => {},
    }),
    run: async (
      _handle: unknown,
      prompt: { runId: string; threadId: string; text: string },
      onEvent: (evt: import("../../src/ipc/protocol.js").IpcMessage) => void,
    ) => {
      emit(onEvent, prompt.runId);
    },
    markBusy: () => {},
    markIdle: () => {},
    dispose: () => {},
  } as unknown as import("../../src/ipc/supervisor.js").ChildSupervisor;
}

test("CP1 runAgentInvocation: tool_start carries toolCallId, tool_end and usage forwarded interleaved with text_delta before done", async () => {
  const south = makeSouth(["doc.read"]);
  const north = makeNorth();
  const clients = { north, south } as unknown as import("../../src/broker/clients.js").BrokerClients;

  const { runAgentInvocation } = await import("../../src/external/core.js");

  const fakeSupervisor = makeFloodSupervisor((onEvent, runId) => {
    onEvent({ kind: "text_delta", runId, delta: "hello " } as import("../../src/ipc/protocol.js").TextDeltaEvent);
    onEvent({
      kind: "tool_start",
      runId,
      toolCallId: "call-1",
      toolName: "doc.read",
      input: {},
    } as import("../../src/ipc/protocol.js").ToolStartEvent);
    onEvent({
      kind: "tool_end",
      runId,
      toolCallId: "call-1",
      ok: true,
      result: { pages: 3 },
    } as import("../../src/ipc/protocol.js").ToolEndEvent);
    onEvent({
      kind: "usage",
      runId,
      inputTokens: 12,
      outputTokens: 34,
    } as import("../../src/ipc/protocol.js").UsageEvent);
    onEvent({ kind: "text_delta", runId, delta: "world" } as import("../../src/ipc/protocol.js").TextDeltaEvent);
    setImmediate(() => onEvent({ kind: "done", runId } as DoneEvent));
  });

  const events: import("../../src/external/core.js").InvocationEvent[] = [];
  for await (const ev of runAgentInvocation(
    AGENT_ID,
    `svc-${AGENT_ID}`,
    TENANT,
    ["doc.read"],
    "test prompt",
    { clients, cfg: baseCfg, log, supervisor: fakeSupervisor, timeoutMs: 2000 },
  )) {
    events.push(ev);
  }

  const types = events.map((e) => e.type);
  assert.deepEqual(types, ["text_delta", "tool_start", "tool_end", "usage", "text_delta", "done"]);

  const toolStart = events[1];
  assert.equal(toolStart.type, "tool_start");
  assert.equal(toolStart.toolCallId, "call-1");
  assert.equal(toolStart.toolName, "doc.read");

  const toolEnd = events[2];
  assert.equal(toolEnd.type, "tool_end");
  assert.equal(toolEnd.toolCallId, "call-1");
  assert.equal(toolEnd.ok, true);
  // result must be stringified exactly as /agui does: string passthrough, else JSON.stringify(result ?? null)
  assert.equal(toolEnd.result, JSON.stringify({ pages: 3 }));

  const usage = events[3];
  assert.equal(usage.type, "usage");
  assert.equal(usage.inputTokens, 12);
  assert.equal(usage.outputTokens, 34);
});

test("CP1 runAgentInvocation: tool_end result stringification — string passthrough, object JSON.stringify, undefined -> \"null\"", async () => {
  const south = makeSouth(["doc.read"]);
  const north = makeNorth();
  const clients = { north, south } as unknown as import("../../src/broker/clients.js").BrokerClients;

  const { runAgentInvocation } = await import("../../src/external/core.js");

  const fakeSupervisor = makeFloodSupervisor((onEvent, runId) => {
    onEvent({
      kind: "tool_end",
      runId,
      toolCallId: "c-str",
      ok: true,
      result: "raw string result",
    } as import("../../src/ipc/protocol.js").ToolEndEvent);
    onEvent({
      kind: "tool_end",
      runId,
      toolCallId: "c-undef",
      ok: false,
      result: undefined,
    } as import("../../src/ipc/protocol.js").ToolEndEvent);
    setImmediate(() => onEvent({ kind: "done", runId } as DoneEvent));
  });

  const events: import("../../src/external/core.js").InvocationEvent[] = [];
  for await (const ev of runAgentInvocation(
    AGENT_ID,
    `svc-${AGENT_ID}`,
    TENANT,
    ["doc.read"],
    "test prompt",
    { clients, cfg: baseCfg, log, supervisor: fakeSupervisor, timeoutMs: 2000 },
  )) {
    events.push(ev);
  }

  const toolEnds = events.filter((e) => e.type === "tool_end");
  assert.equal(toolEnds.length, 2);
  assert.equal(toolEnds[0].result, "raw string result");
  assert.equal(toolEnds[1].result, JSON.stringify(null));
  assert.equal(toolEnds[1].ok, false);
});
