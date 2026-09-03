// Tests for ApprovalRegistry.drainForRun (CP3, F7) — closing one run's
// connection must only resolve/remove that run's pending approvals, leaving
// other runs' approvals pending in the map.
import { test } from "node:test";
import assert from "node:assert/strict";
import { ApprovalRegistry } from "../src/agui/hitl.js";
import type { ApprovalInfo } from "../src/broker/governance.js";

function makeInfo(toolCallId: string): ApprovalInfo {
  return {
    toolCallId,
    toolName: "tool",
    toolId: "tool",
    effectClass: 0,
    reason: "test",
    args: {},
    stepUp: false,
  };
}

test("drainForRun resolves and removes only the matching run's pending approvals", async () => {
  const registry = new ApprovalRegistry();

  const promiseA = registry.await_(makeInfo("call-a"), "user-a", "run-a");
  const promiseB = registry.await_(makeInfo("call-b"), "user-b", "run-b");

  registry.drainForRun("run-a");

  assert.equal(await promiseA, false);
  assert.equal(registry.listForUser("user-a").length, 0);

  // B must still be pending and present in the registry.
  assert.equal(registry.listForUser("user-b").length, 1);
  let bSettled = false;
  promiseB.then(() => {
    bSettled = true;
  });
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(bSettled, false);

  // Resolve B directly to avoid leaving a dangling promise.
  registry.resolve("call-b", true);
  assert.equal(await promiseB, true);
});

test("drainForRun with ok=true resolves matching entries to true", async () => {
  const registry = new ApprovalRegistry();
  const promise = registry.await_(makeInfo("call-c"), "user-c", "run-c");

  registry.drainForRun("run-c", true);

  assert.equal(await promise, true);
});

// ── Approval timeout (Config.approvalTimeoutMs) ────────────────────────────────
//
// WHY: before the timeout the only exits were a user POST, shutdown drain, or SSE
// close — an approval card nobody ever answers held its child busy (and its pool
// slot) for the process's lifetime.

test("a never-answered approval times out and is DENIED, not left pending", async () => {
  const registry = new ApprovalRegistry(30);

  // The approval's timer is unref()'d on purpose (see the last test in this file),
  // so waiting on it requires holding the event loop open — otherwise node exits
  // before it can fire.
  const keepAlive = setTimeout(() => {}, 5000);
  const decision = await registry
    .await_(makeInfo("call-timeout"), "user-t", "run-t")
    .finally(() => clearTimeout(keepAlive));

  assert.equal(decision, false, "an unanswered elevation request is not consent — it must deny");
  assert.equal(
    registry.listForUser("user-t").length,
    0,
    "the timed-out entry must be removed from the registry, exactly as a manual deny removes it",
  );
  assert.equal(
    registry.resolve("call-timeout", true),
    false,
    "a later POST for a timed-out approval must find nothing to resolve",
  );
});

test("an answer before the timeout wins, and the timer cannot re-resolve afterwards", async () => {
  const registry = new ApprovalRegistry(40);
  const promise = registry.await_(makeInfo("call-fast"), "user-f", "run-f");

  assert.equal(registry.resolve("call-fast", true), true);
  assert.equal(await promise, true, "the human's approval must be the decision");

  // Past the timeout window: a timer that survived the manual resolve would fire
  // here. It cannot flip an already-settled promise, but it would still be a live
  // timer per approval — so assert the entry is gone and stays gone.
  await new Promise((resolve) => setTimeout(resolve, 80));
  assert.equal(registry.listForUser("user-f").length, 0);
  assert.equal(await promise, true, "the decision must remain the human's, not the timeout's deny");
});

test("drainForRun on a timeout-armed approval leaves no timer behind", async () => {
  const registry = new ApprovalRegistry(40);
  const promise = registry.await_(makeInfo("call-drained"), "user-d", "run-d");

  registry.drainForRun("run-d", false);
  assert.equal(await promise, false);

  await new Promise((resolve) => setTimeout(resolve, 80));
  assert.equal(registry.listForUser("user-d").length, 0);
});

test("a pending approval's timeout timer does not hold the process open", async () => {
  // WHY this is the leak check: the timer is armed for 10 minutes and never
  // resolved or drained. If it were not unref()'d, node --test could not exit
  // until it fired — this file would hang for ten minutes instead of finishing.
  const registry = new ApprovalRegistry(600_000);
  void registry.await_(makeInfo("call-forever"), "user-x", "run-x");
  assert.equal(registry.listForUser("user-x").length, 1, "the approval must be pending, timer armed");
});
