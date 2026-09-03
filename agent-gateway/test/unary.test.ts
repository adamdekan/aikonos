// Tests for the shared `unary` promisify helper (CP3, fable-rpc-twins):
// - resolves with the callback's response on success
// - rejects with the callback's error on failure
// - passes the request and metadata through to the wrapped fn unchanged
// - invokes the callback exactly once
//
// CP1 (rpc-twins-tails): every call now carries a CallOptions deadline —
// a fake UnaryCall capturing `options` proves it is present and set to
// approximately now+configured, and that setUnaryDeadlineMs changes it.
import { test } from "node:test";
import assert from "node:assert/strict";
import { Metadata, status } from "@grpc/grpc-js";
import type { CallOptions, ServiceError } from "@grpc/grpc-js";
import { unary, setUnaryDeadlineMs } from "../src/broker/unary.js";

function fakeError(details: string): ServiceError {
  return { code: status.INTERNAL, details, metadata: new Metadata(), name: "Error", message: details };
}

test("unary resolves with the response on success", async () => {
  const response = { ok: true };
  const fn = (
    _req: { id: string },
    _md: Metadata,
    _options: CallOptions,
    cb: (err: ServiceError | null, resp: typeof response) => void,
  ) => cb(null, response);

  const result = await unary(fn, { id: "1" }, new Metadata());

  assert.equal(result, response);
});

test("unary rejects with the error on failure", async () => {
  const err = fakeError("boom");
  const fn = (
    _req: { id: string },
    _md: Metadata,
    _options: CallOptions,
    cb: (err: ServiceError | null, resp: unknown) => void,
  ) => cb(err, undefined);

  await assert.rejects(() => unary(fn, { id: "1" }, new Metadata()), err);
});

test("unary passes the request and metadata through to the wrapped fn", async () => {
  const req = { id: "42" };
  const md = new Metadata();
  md.set("authorization", "Bearer tok");
  let seenReq: unknown;
  let seenMd: Metadata | undefined;
  const fn = (
    r: typeof req,
    m: Metadata,
    _options: CallOptions,
    cb: (err: ServiceError | null, resp: string) => void,
  ) => {
    seenReq = r;
    seenMd = m;
    cb(null, "resp");
  };

  await unary(fn, req, md);

  assert.equal(seenReq, req);
  assert.equal(seenMd, md);
});

test("unary invokes the callback exactly once", async () => {
  let callCount = 0;
  const fn = (
    _req: unknown,
    _md: Metadata,
    _options: CallOptions,
    cb: (err: ServiceError | null, resp: string) => void,
  ) => {
    callCount += 1;
    cb(null, "resp");
  };

  await unary(fn, {}, new Metadata());

  assert.equal(callCount, 1);
});

// ── CP1: deadline enforcement ─────────────────────────────────────────────────

test("unary passes a CallOptions deadline ~= now + module default (30000ms)", async () => {
  const before = Date.now();
  let seenOptions: CallOptions | undefined;
  const fn = (
    _req: unknown,
    _md: Metadata,
    options: CallOptions,
    cb: (err: ServiceError | null, resp: string) => void,
  ) => {
    seenOptions = options;
    cb(null, "resp");
  };

  await unary(fn, {}, new Metadata());
  const after = Date.now();

  assert.ok(seenOptions?.deadline instanceof Date);
  const deadlineMs = seenOptions.deadline.getTime();
  assert.ok(deadlineMs >= before + 30_000, "deadline should be at least now+30000 (before)");
  assert.ok(deadlineMs <= after + 30_000, "deadline should be at most now+30000 (after)");
});

test("setUnaryDeadlineMs changes the deadline applied to subsequent calls", async () => {
  setUnaryDeadlineMs(5_000);
  try {
    const before = Date.now();
    let seenOptions: CallOptions | undefined;
    const fn = (
      _req: unknown,
      _md: Metadata,
      options: CallOptions,
      cb: (err: ServiceError | null, resp: string) => void,
    ) => {
      seenOptions = options;
      cb(null, "resp");
    };

    await unary(fn, {}, new Metadata());
    const after = Date.now();

    assert.ok(seenOptions?.deadline instanceof Date);
    const deadlineMs = seenOptions.deadline.getTime();
    assert.ok(deadlineMs >= before + 5_000);
    assert.ok(deadlineMs <= after + 5_000);
  } finally {
    setUnaryDeadlineMs(30_000);
  }
});
