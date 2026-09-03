// Regression tests for gateToolCall — the shared tool_call governance hook.
//
// WHY these tests exist:
//   The load_skill exemption must be locked in. The risk is two-fold:
//   (a) load_skill must NOT be blocked even when bridge.gate would deny — it is
//       a gateway-internal read-tool, not a governed skill (spec A2).
//   (b) A normal tool (e.g. web_fetch) must STILL be gated; the exemption must
//       be load_skill-only, not a blanket bypass.
//
//   These tests exercise gateToolCall directly (extracted from session.ts +
//   session-plan.ts) without constructing a full Pi session — fast, isolated,
//   and readable.
import { test } from "node:test";
import assert from "node:assert/strict";
import { gateToolCall } from "../src/pi/gate-tool-call.js";

// ── Fake bridge ────────────────────────────────────────────────────────────────

function makeBridge(allow: boolean, reason?: string) {
  const calls: { toolCallId: string; toolName: string }[] = [];
  return {
    gate: async (toolCallId: string, toolName: string, _input: Record<string, unknown>) => {
      calls.push({ toolCallId, toolName });
      return { allow, reason };
    },
    calls,
  };
}

// ── Fake logger (silent) ────────────────────────────────────────────────────────

const silentLog = { info: (_obj: unknown, _msg?: string) => {} };

// ── Fake tool_call event ───────────────────────────────────────────────────────

function makeEvent(toolName: string, toolCallId = "call-1") {
  return { toolCallId, toolName, input: {} as Record<string, unknown> };
}

// ── Tests ──────────────────────────────────────────────────────────────────────

test("gate-tool-call: load_skill is unconditionally allowed — bridge.gate is NOT called", async () => {
  // WHY: load_skill is gateway-internal (spec A2). Even if bridge.gate would
  // deny it, the hook must return undefined (allow) without calling gate.
  // A future refactor that removes the exemption will break this test.
  const bridge = makeBridge(false, "policy deny");  // denying bridge
  const result = await gateToolCall(bridge, makeEvent("load_skill"), silentLog);

  assert.equal(result, undefined, "load_skill must return undefined (allow), not a block object");
  assert.equal(bridge.calls.length, 0, "bridge.gate must NOT be called for load_skill");
});

test("gate-tool-call: normal tool is blocked when bridge.gate denies", async () => {
  // WHY: the exemption must be load_skill-only. web_fetch and any other
  // governed tool must go through bridge.gate and honour its decision.
  const bridge = makeBridge(false, "user lacks web.fetch");
  const result = await gateToolCall(bridge, makeEvent("web_fetch"), silentLog);

  assert.ok(result !== undefined, "a denying bridge must produce a block result");
  assert.equal(result.block, true, "block must be true when bridge denies");
  assert.ok(
    typeof result.reason === "string" && result.reason.includes("web.fetch"),
    `reason must carry the bridge's denial message; got: ${result.reason}`,
  );
  assert.equal(bridge.calls.length, 1, "bridge.gate must be called exactly once for web_fetch");
  assert.equal(bridge.calls[0]?.toolName, "web_fetch");
});

test("gate-tool-call: normal tool is allowed when bridge.gate approves", async () => {
  // WHY: confirms the allow path — bridge.gate is called and a green-light
  // from the bridge must produce undefined (allow), not a block.
  const bridge = makeBridge(true);
  const result = await gateToolCall(bridge, makeEvent("web_fetch"), silentLog);

  assert.equal(result, undefined, "an allowing bridge must return undefined (allow)");
  assert.equal(bridge.calls.length, 1, "bridge.gate must be called exactly once");
  assert.equal(bridge.calls[0]?.toolName, "web_fetch");
});

test("gate-tool-call: block reason is prefixed with 'aikonos:' from bridge reason", async () => {
  // WHY: the reason string format is the message the user sees in the chat UI
  // when a tool is blocked. The 'aikonos:' prefix is the UX convention; a
  // refactor that drops it would surface confusing raw policy strings to users.
  const bridge = makeBridge(false, "policy: skill not granted");
  const result = await gateToolCall(bridge, makeEvent("doc_write"), silentLog);

  assert.ok(result?.reason?.startsWith("aikonos:"), `reason must start with 'aikonos:'; got: ${result?.reason}`);
  assert.ok(result?.reason?.includes("policy: skill not granted"), "reason must include bridge's message");
});

test("gate-tool-call: block reason falls back to 'denied' when bridge provides no reason", async () => {
  // WHY: bridge.gate may return { allow: false } without a reason string
  // (e.g. early-exit OPA deny). The hook must not produce undefined/null in
  // the reason field — 'aikonos: denied' is the required fallback.
  const bridge = makeBridge(false, undefined);
  const result = await gateToolCall(bridge, makeEvent("email_draft"), silentLog);

  assert.equal(result?.reason, "aikonos: denied", `fallback reason must be 'aikonos: denied'; got: ${result?.reason}`);
});

test("gate-tool-call: workflow_run is allowed when GovernanceBridge short-circuits to allow=true", async () => {
  // WHY: workflow_* tools short-circuit in GovernanceBridge.gate() which returns
  // { allow: true } without touching the broker. gateToolCall trusts bridge.gate's
  // answer — so when bridge.gate returns allow=true for workflow_run, gateToolCall
  // must return undefined (allow) and call bridge.gate exactly once.
  // The test uses an allowing bridge to simulate what GovernanceBridge.gate()
  // produces for workflow_* names. Locking this in ensures the pass-through is
  // preserved even if gateToolCall gains new logic around it.
  const bridge = makeBridge(true);  // simulates GovernanceBridge short-circuit returning allow
  const result = await gateToolCall(bridge, makeEvent("workflow_run"), silentLog);

  assert.equal(result, undefined, "workflow_run must return undefined (allow) when bridge allows");
  assert.equal(bridge.calls.length, 1, "bridge.gate is called once (short-circuit is GovernanceBridge's job)");
  assert.equal(bridge.calls[0]?.toolName, "workflow_run");
});

test("gate-tool-call: web_fetch inside a workflow step still goes through full bridge.gate path", async () => {
  // WHY: the workflow_* short-circuit in GovernanceBridge is narrow — it matches
  // only workflow_save|run|list. A normal tool invoked as a workflow step (e.g.
  // web_fetch) must traverse the full gate→execute path so broker FGA + OPA
  // policy applies per step. This test confirms gateToolCall never exempts
  // non-workflow tool names and always delegates the decision to bridge.gate.
  const bridge = makeBridge(true);
  const result = await gateToolCall(bridge, makeEvent("web_fetch"), silentLog);

  assert.equal(result, undefined, "web_fetch must be allowed via bridge.gate");
  assert.equal(bridge.calls.length, 1, "bridge.gate must be called for the inner step tool");
  assert.equal(bridge.calls[0]?.toolName, "web_fetch");
});
