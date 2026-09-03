// CP3 (F9) tests: graceful shutdown orchestration. All deps are fakes — no
// real process, no real signals — gracefulShutdown() is a pure function over
// injected handles, so these assert call order, failure isolation, the
// force-exit deadline, and the double-signal fast-exit path.
import { test } from "node:test";
import assert from "node:assert/strict";
import pino from "pino";
import { gracefulShutdown, type ShutdownDeps } from "../src/shutdown.js";

const log = pino({ level: "silent" });

function fakeDeps(overrides: Partial<ShutdownDeps> = {}): { deps: ShutdownDeps; calls: string[]; exitCodes: number[] } {
  const calls: string[] = [];
  const exitCodes: number[] = [];
  const deps: ShutdownDeps = {
    log,
    stopScheduler: () => calls.push("scheduler"),
    closeApp: async () => { calls.push("http-app"); },
    closeExternal: async () => { calls.push("http-external"); },
    approvals: { drain: (ok) => calls.push(`approvals:${ok}`) },
    supervisor: { dispose: () => calls.push("supervisor") },
    egressProxy: { stop: async () => { calls.push("egress-proxy"); } },
    auditConsumer: { stop: async () => { calls.push("audit-consumer"); } },
    clients: {
      north: { close: () => calls.push("broker-north") },
      south: { close: () => calls.push("broker-south") },
    },
    exit: (code) => exitCodes.push(code),
    ...overrides,
  };
  return { deps, calls, exitCodes };
}

test("gracefulShutdown runs steps in CP3 order and exits 0", async () => {
  const { deps, calls, exitCodes } = fakeDeps();
  const handler = gracefulShutdown(deps);

  handler("SIGTERM");
  // Let the async step chain settle.
  await new Promise((r) => setTimeout(r, 10));

  assert.deepEqual(calls, [
    "scheduler",
    "http-app",
    "http-external",
    "approvals:false",
    "supervisor",
    "egress-proxy",
    "audit-consumer",
    "broker-north",
    "broker-south",
  ]);
  assert.deepEqual(exitCodes, [0]);
});

test("a failing step is logged and does not block the remaining steps", async () => {
  const { deps, calls, exitCodes } = fakeDeps({
    supervisor: {
      dispose: () => {
        calls.push("supervisor");
        throw new Error("boom");
      },
    },
  });
  const handler = gracefulShutdown(deps);

  handler("SIGTERM");
  await new Promise((r) => setTimeout(r, 10));

  // supervisor's step still ran (and recorded) before throwing; every step
  // after it must still have run.
  assert.ok(calls.includes("supervisor"));
  assert.ok(calls.includes("egress-proxy"));
  assert.ok(calls.includes("audit-consumer"));
  assert.ok(calls.includes("broker-north"));
  assert.deepEqual(exitCodes, [0]);
});

test("a second signal while shutdown is in flight forces immediate exit(1)", async () => {
  let resolveHttpClose: () => void = () => {};
  const { deps, exitCodes } = fakeDeps({
    closeApp: () => new Promise<void>((resolve) => { resolveHttpClose = resolve; }),
  });
  const handler = gracefulShutdown(deps);

  handler("SIGTERM");
  // First shutdown is stuck waiting on closeApp — nothing has exited yet.
  await new Promise((r) => setTimeout(r, 5));
  assert.deepEqual(exitCodes, []);

  handler("SIGINT");
  assert.deepEqual(exitCodes, [1]);

  // Let the stuck first shutdown finish so it doesn't leak into other tests.
  resolveHttpClose();
  await new Promise((r) => setTimeout(r, 10));
});

test("force-exit timer fires exit(1) when a step never resolves", async () => {
  const { deps, exitCodes } = fakeDeps({
    egressProxy: { stop: () => new Promise(() => { /* never resolves */ }) },
    forceExitMs: 20,
  });
  const handler = gracefulShutdown(deps);

  handler("SIGTERM");
  assert.deepEqual(exitCodes, []);

  await new Promise((r) => setTimeout(r, 40));
  assert.deepEqual(exitCodes, [1]);
});
