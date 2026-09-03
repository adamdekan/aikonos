// CP4.1 — circuit breaker over consecutive
// TRANSPORT FAILURES of the south CheckRateLimit RPC. A fake south client
// lets these tests drive the state machine deterministically: closed ->
// (N consecutive transport failures) -> open (fast-fails, no RPC call,
// except every 10th open-state request which probes) -> (probe fails) ->
// open -> (probe succeeds) -> closed. Explicit allowed=false responses must
// never trip the breaker.
import { test } from "node:test";
import assert from "node:assert/strict";

import { createRateLimitBreaker } from "../src/llm/rate-limit-breaker.js";
import type { CheckRateLimitResponse } from "../gen/ts/proto/broker.js";

// Matches the OPEN_STATE_PROBE_INTERVAL constant in rate-limit-breaker.ts —
// duplicated here (not imported, it's not exported) so the test also pins
// the documented interval value.
const PROBE_INTERVAL = 10;

const noopLog = { warn: () => {} };

// Builds a fake south call from a queue of outcomes: "fail" throws a
// transport error, or a CheckRateLimitResponse resolves normally. `calls`
// records every invocation, so tests can assert the RPC was (or was not)
// actually reached for a given breaker call.
function fakeCall(outcomes: Array<"fail" | CheckRateLimitResponse>) {
  const calls: string[] = [];
  const fn = async (tenantId: string, agentId: string, provider: string, userId?: string) => {
    calls.push(`${tenantId}:${agentId}:${provider}:${userId ?? ""}`);
    const next = outcomes.shift();
    if (next === undefined) throw new Error("fakeCall: outcomes exhausted");
    if (next === "fail") throw new Error("transport: connection refused");
    return next;
  };
  return { fn, calls };
}

test("rate-limit breaker: fail-open below threshold", async () => {
  const { fn, calls } = fakeCall(["fail", "fail", "fail", "fail"]);
  const breaker = createRateLimitBreaker(fn, { threshold: 5 }, noopLog);
  // 4 consecutive transport failures, threshold 5 — every call must be
  // allowed (fail-open), not throw, and the RPC is actually attempted each
  // time (below threshold there's no fast-fail).
  for (let i = 0; i < 4; i++) {
    await assert.doesNotReject(breaker("t1", "a1", "openrouter"));
  }
  assert.equal(calls.length, 4);
});

test("rate-limit breaker: opens after threshold consecutive transport failures", async () => {
  const { fn } = fakeCall(["fail", "fail", "fail", "fail", "fail"]);
  const breaker = createRateLimitBreaker(fn, { threshold: 5 }, noopLog);
  for (let i = 0; i < 4; i++) {
    await assert.doesNotReject(breaker("t1", "a1", "openrouter"));
  }
  // 5th consecutive failure trips the breaker — this call must deny.
  await assert.rejects(breaker("t1", "a1", "openrouter"));
});

test("rate-limit breaker: while open, fast-fails WITHOUT calling the RPC", async () => {
  const { fn, calls } = fakeCall(["fail", "fail", "fail", "fail", "fail"]);
  const breaker = createRateLimitBreaker(fn, { threshold: 5 }, noopLog);
  for (let i = 0; i < 5; i++) await breaker("t1", "a1", "openrouter").catch(() => {});
  assert.equal(calls.length, 5); // the 5 failures that opened the breaker

  // Breaker is now open. The next (PROBE_INTERVAL - 2) requests are
  // non-probe open-state calls — each must deny immediately AND must not
  // touch the RPC at all (this is the load-shedding property).
  for (let i = 0; i < PROBE_INTERVAL - 2; i++) {
    await assert.rejects(breaker("t1", "a1", "openrouter"), /circuit breaker open/);
  }
  assert.equal(calls.length, 5, "no additional RPC calls while open and not on a probe boundary");
});

test("rate-limit breaker: probe fires on the interval and closes on success", async () => {
  // 5 failures to open, then (PROBE_INTERVAL - 1) fast-failed non-probe
  // calls, then the PROBE_INTERVAL-th open-state call is the probe.
  const outcomes: Array<"fail" | CheckRateLimitResponse> = ["fail", "fail", "fail", "fail", "fail"];
  const { fn, calls } = fakeCall(outcomes);
  const breaker = createRateLimitBreaker(fn, { threshold: 5 }, noopLog);
  for (let i = 0; i < 5; i++) await breaker("t1", "a1", "openrouter").catch(() => {});
  assert.equal(calls.length, 5);

  for (let i = 0; i < PROBE_INTERVAL - 1; i++) {
    await assert.rejects(breaker("t1", "a1", "openrouter"), /circuit breaker open/);
  }
  assert.equal(calls.length, 5, "still no RPC calls before the probe boundary");

  // The probe call: queue an allow response for it, then invoke.
  outcomes.push({ allowed: true, limitType: "" });
  await assert.doesNotReject(breaker("t1", "a1", "openrouter"));
  assert.equal(calls.length, 6, "the probe call did invoke the RPC");

  // Breaker is closed again — the very next call is allowed and the RPC is
  // invoked directly (no more fast-fail).
  outcomes.push({ allowed: true, limitType: "" });
  await assert.doesNotReject(breaker("t1", "a1", "openrouter"));
  assert.equal(calls.length, 7);
});

test("rate-limit breaker: probe failure keeps it open", async () => {
  const outcomes: Array<"fail" | CheckRateLimitResponse> = ["fail", "fail", "fail", "fail", "fail"];
  const { fn, calls } = fakeCall(outcomes);
  const breaker = createRateLimitBreaker(fn, { threshold: 5 }, noopLog);
  for (let i = 0; i < 5; i++) await breaker("t1", "a1", "openrouter").catch(() => {});

  for (let i = 0; i < PROBE_INTERVAL - 1; i++) {
    await breaker("t1", "a1", "openrouter").catch(() => {});
  }
  assert.equal(calls.length, 5);

  // The probe call fails — breaker must stay open and deny.
  outcomes.push("fail");
  await assert.rejects(breaker("t1", "a1", "openrouter"), /circuit breaker open/);
  assert.equal(calls.length, 6, "the probe call did invoke the RPC even though it failed");

  // Immediately after a failed probe, the breaker is still open — the very
  // next call must fast-fail without invoking the RPC again.
  await assert.rejects(breaker("t1", "a1", "openrouter"), /circuit breaker open/);
  assert.equal(calls.length, 6, "no RPC call on the request right after a failed probe");
});

test("Spend-caps CP4 rate-limit breaker: userId is forwarded to the wrapped call untouched", async () => {
  const { fn, calls } = fakeCall([{ allowed: true, limitType: "" }]);
  const breaker = createRateLimitBreaker(fn, { threshold: 5 }, noopLog);
  await assert.doesNotReject(breaker("t1", "a1", "openrouter", "svc-agent-uuid"));
  assert.deepEqual(calls, ["t1:a1:openrouter:svc-agent-uuid"]);
});

test("Spend-caps CP4 rate-limit breaker: omitting userId forwards undefined (not a default string)", async () => {
  const { fn, calls } = fakeCall([{ allowed: true, limitType: "" }]);
  const breaker = createRateLimitBreaker(fn, { threshold: 5 }, noopLog);
  await assert.doesNotReject(breaker("t1", "a1", "openrouter"));
  assert.deepEqual(calls, ["t1:a1:openrouter:"]);
});

test("rate-limit breaker: explicit allowed=false never trips the breaker", async () => {
  const deny: CheckRateLimitResponse = { allowed: false, limitType: "tpm_agent" };
  const outcomes: Array<"fail" | CheckRateLimitResponse> = Array(10).fill(deny);
  const { fn } = fakeCall(outcomes);
  const breaker = createRateLimitBreaker(fn, { threshold: 5 }, noopLog);

  // 10 explicit denials in a row, well past the transport-failure threshold —
  // the breaker must stay closed and each call must surface as a normal deny
  // (not a breaker-open error), proving allowed=false never increments the
  // transport-failure counter.
  for (let i = 0; i < 10; i++) {
    await assert.rejects(breaker("t1", "a1", "openrouter"), /rate limit exceeded/);
  }

  // Since the counter never moved, a single subsequent transport failure
  // must still fail open (proves it's nowhere near threshold).
  const { fn: fn2 } = fakeCall(["fail"]);
  const breaker2 = createRateLimitBreaker(fn2, { threshold: 5 }, noopLog);
  await assert.doesNotReject(breaker2("t1", "a1", "openrouter"));
});
