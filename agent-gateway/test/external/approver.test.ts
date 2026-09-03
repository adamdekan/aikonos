// Tests for externalApprover and cachedApprovalMode (external/approver.ts).
// TDD: written before the implementation — run red first, then green.
import { test } from "node:test";
import assert from "node:assert/strict";
import type { ApprovalInfo } from "../../src/broker/governance.js";

function info(toolId: string): ApprovalInfo {
  return {
    toolCallId: "tc-1",
    toolName: toolId,
    toolId,
    effectClass: 0,
    reason: "human approval required",
    args: {},
    stepUp: false,
  };
}

// ── externalApprover ──────────────────────────────────────────────────────────

test("externalApprover: mode=auto, tool in set → allow", async () => {
  const { externalApprover } = await import("../../src/external/approver.js");
  const log = { info: () => {}, warn: () => {}, error: () => {}, debug: () => {} } as unknown as import("../../src/log.js").Logger;

  const approver = externalApprover(
    ["doc.read", "web.fetch"],
    async () => "auto",
    log,
  );
  assert.equal(await approver(info("doc.read")), true);
  assert.equal(await approver(info("web.fetch")), true);
});

test("externalApprover: mode=auto, tool NOT in set → deny", async () => {
  const { externalApprover } = await import("../../src/external/approver.js");
  const log = { info: () => {}, warn: () => {}, error: () => {}, debug: () => {} } as unknown as import("../../src/log.js").Logger;

  const approver = externalApprover(
    ["doc.read"],
    async () => "auto",
    log,
  );
  assert.equal(await approver(info("web.fetch")), false, "tool not in set → deny");
  assert.equal(await approver(info("email.draft")), false, "tool not in set → deny");
});

test("externalApprover: mode=needs_approval → deny even for tool in set", async () => {
  // WHY: mid-run flip from auto → needs_approval must cause subsequent gates to deny,
  // even for tools that are in the approved-tools set. preAuthApprover would have
  // allowed these; externalApprover re-checks mode per gate and fails closed.
  const { externalApprover } = await import("../../src/external/approver.js");
  const log = { info: () => {}, warn: () => {}, error: () => {}, debug: () => {} } as unknown as import("../../src/log.js").Logger;

  const approver = externalApprover(
    ["doc.read", "web.fetch"],
    async () => "needs_approval",
    log,
  );
  assert.equal(await approver(info("doc.read")), false, "needs_approval mode → deny in-set tool");
  assert.equal(await approver(info("web.fetch")), false, "needs_approval mode → deny in-set tool");
});

test("externalApprover: getApprovalMode rejects → deny (fail closed)", async () => {
  // WHY: a south RPC failure must not open the gate. Fail closed — deny.
  const { externalApprover } = await import("../../src/external/approver.js");
  const log = { info: () => {}, warn: () => {}, error: () => {}, debug: () => {} } as unknown as import("../../src/log.js").Logger;

  const approver = externalApprover(
    ["doc.read"],
    async () => { throw new Error("south RPC timeout"); },
    log,
  );
  assert.equal(await approver(info("doc.read")), false, "RPC failure → fail closed → deny");
});

// ── cachedApprovalMode ────────────────────────────────────────────────────────

test("cachedApprovalMode: two calls within TTL → underlying fetch called once", async () => {
  const { cachedApprovalMode } = await import("../../src/external/approver.js");

  let callCount = 0;
  const fetch = async () => { callCount++; return "auto"; };

  // Use a long TTL (10 s) so both calls land within it.
  const get = cachedApprovalMode(fetch, 10_000);

  const first = await get();
  const second = await get();

  assert.equal(first, "auto");
  assert.equal(second, "auto");
  assert.equal(callCount, 1, "fetch must be called only once within TTL");
});

test("cachedApprovalMode: call after TTL → re-fetches", async () => {
  const { cachedApprovalMode } = await import("../../src/external/approver.js");

  let callCount = 0;
  const fetch = async () => { callCount++; return "auto"; };

  // Use a 1 ms TTL and a clock injector so the test doesn't need a real delay.
  let fakeNow = 0;
  const get = cachedApprovalMode(fetch, 1, () => fakeNow);

  await get();            // populates cache at fakeNow=0
  fakeNow = 2;            // advance past TTL
  await get();            // must re-fetch

  assert.equal(callCount, 2, "fetch must be called again after TTL expires");
});

test("cachedApprovalMode: fetch rejection is NOT cached as auto (fail closed)", async () => {
  // WHY: caching a failure as "auto" would open the gate on subsequent calls
  // without re-fetching. We must re-fetch every call after a failure so a
  // transient south error doesn't permanently unlock approval.
  const { cachedApprovalMode } = await import("../../src/external/approver.js");

  let callCount = 0;
  let shouldFail = true;
  const fetch = async () => {
    callCount++;
    if (shouldFail) throw new Error("south error");
    return "auto";
  };

  // TTL long enough that a cache hit would occur if we cached failures.
  const get = cachedApprovalMode(fetch, 10_000);

  // First call: rejection → must not be "auto", not cached.
  const first = await get();
  assert.notEqual(first, "auto", "rejected fetch must not resolve to 'auto'");
  assert.equal(callCount, 1);

  // Second call: rejection still present — must re-fetch, not return cached failure.
  shouldFail = false;
  const second = await get();
  assert.equal(second, "auto", "after failure cleared, second call must re-fetch and get auto");
  assert.equal(callCount, 2, "must re-fetch after a failure (failure not cached)");
});
