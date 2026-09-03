// CP3 (F9) tests: /readyz semantics. evaluateReadyz() is the pure function
// the app.ts route wraps — tested directly here against fake broker/audit
// states so the 200/503 boundary doesn't depend on a real gRPC channel or NATS
// connection.
import { test } from "node:test";
import assert from "node:assert/strict";
import { evaluateReadyz } from "../src/readyz.js";
import { connectivityState } from "../src/broker/north.js";

test("200-equivalent: broker READY + audit connected", () => {
  const result = evaluateReadyz({
    brokerState: () => connectivityState.READY,
    auditStatus: () => ({ state: "connected" }),
  });
  assert.equal(result.ok, true);
  assert.equal(result.checks.broker, "ok");
  assert.equal(result.checks.audit, "ok");
});

test("200-equivalent: broker IDLE (lazily-connectable, not broken) + audit disabled", () => {
  const result = evaluateReadyz({
    brokerState: () => connectivityState.IDLE,
    auditStatus: () => ({ state: "disabled" }),
  });
  assert.equal(result.ok, true);
});

test("503-equivalent: broker TRANSIENT_FAILURE fails even when audit is fine", () => {
  const result = evaluateReadyz({
    brokerState: () => connectivityState.TRANSIENT_FAILURE,
    auditStatus: () => ({ state: "connected" }),
  });
  assert.equal(result.ok, false);
  assert.match(result.checks.broker, /not-ready/);
  assert.equal(result.checks.audit, "ok");
});

test("503-equivalent: audit disconnected fails even when broker is READY", () => {
  const result = evaluateReadyz({
    brokerState: () => connectivityState.READY,
    auditStatus: () => ({ state: "disconnected", lastError: "connection refused" }),
  });
  assert.equal(result.ok, false);
  assert.equal(result.checks.broker, "ok");
  assert.match(result.checks.audit, /not-ready/);
  assert.match(result.checks.audit, /connection refused/);
});
