// Unit table for the CP2 (F27) gRPC → HTTP error mapping.
// grpcToHttp is the single mapper every route's catch path now uses via sendError.
import { test } from "node:test";
import assert from "node:assert/strict";
import { status } from "@grpc/grpc-js";
import pino from "pino";
import { grpcToHttp, sendError, trimmedErrorMessage } from "../src/http-errors.js";

function grpcErr(code: number, message = "boom"): { code: number; message: string; details: string } {
  return { code, message: `${code} X: ${message}`, details: message };
}

test("grpcToHttp: PERMISSION_DENIED → 403", () => {
  assert.equal(grpcToHttp(grpcErr(status.PERMISSION_DENIED)), 403);
});

test("grpcToHttp: NOT_FOUND → 404", () => {
  assert.equal(grpcToHttp(grpcErr(status.NOT_FOUND)), 404);
});

test("grpcToHttp: INVALID_ARGUMENT → 400", () => {
  assert.equal(grpcToHttp(grpcErr(status.INVALID_ARGUMENT)), 400);
});

test("grpcToHttp: FAILED_PRECONDITION → 409", () => {
  assert.equal(grpcToHttp(grpcErr(status.FAILED_PRECONDITION)), 409);
});

test("grpcToHttp: ALREADY_EXISTS → 409", () => {
  assert.equal(grpcToHttp(grpcErr(status.ALREADY_EXISTS)), 409);
});

test("grpcToHttp: RESOURCE_EXHAUSTED → 429", () => {
  assert.equal(grpcToHttp(grpcErr(status.RESOURCE_EXHAUSTED)), 429);
});

test("grpcToHttp: UNAVAILABLE → 502", () => {
  assert.equal(grpcToHttp(grpcErr(status.UNAVAILABLE)), 502);
});

test("grpcToHttp: DEADLINE_EXCEEDED → 504", () => {
  assert.equal(grpcToHttp(grpcErr(status.DEADLINE_EXCEEDED)), 504);
});

test("grpcToHttp: Internal (13) — not in the contract table → 500", () => {
  assert.equal(grpcToHttp(grpcErr(status.INTERNAL)), 500);
});

test("grpcToHttp: UNKNOWN gRPC code → 500", () => {
  assert.equal(grpcToHttp(grpcErr(status.UNKNOWN)), 500);
});

test("grpcToHttp: plain Error (no numeric code) → 500", () => {
  assert.equal(grpcToHttp(new Error("network failure")), 500);
});

test("grpcToHttp: null → 500", () => {
  assert.equal(grpcToHttp(null), 500);
});

test("grpcToHttp: undefined → 500", () => {
  assert.equal(grpcToHttp(undefined), 500);
});

test("grpcToHttp: plain string thrown → 500", () => {
  assert.equal(grpcToHttp("oops"), 500);
});

// ── sendError: body-shape / leak-shape assertions ─────────────────────────────

const silentLog = pino({ level: "silent" });

function fakeReply() {
  const r = { statusCode: 0, body: undefined as unknown };
  return {
    code(n: number) {
      r.statusCode = n;
      return { send: (b: unknown) => { r.body = b; } };
    },
    result: r,
  };
}

test("sendError: 4xx body carries the gRPC detail, not String(err)", () => {
  const reply = fakeReply();
  sendError(reply, silentLog, grpcErr(status.NOT_FOUND, "agent xyz not found"));

  assert.equal(reply.result.statusCode, 404);
  assert.deepEqual(reply.result.body, { error: "agent xyz not found" });
});

test("sendError: 5xx body is a fixed generic message, never String(err)", () => {
  const reply = fakeReply();
  const err = new Error("pq: connection refused at 10.0.0.5:5432 — internal stack detail");
  sendError(reply, silentLog, err);

  assert.equal(reply.result.statusCode, 500);
  assert.deepEqual(reply.result.body, { error: "internal error" });
});

test("sendError: UNAVAILABLE body is 'upstream unavailable', never the raw message", () => {
  const reply = fakeReply();
  sendError(reply, silentLog, grpcErr(status.UNAVAILABLE, "connect ECONNREFUSED 127.0.0.1:9090"));

  assert.equal(reply.result.statusCode, 502);
  assert.deepEqual(reply.result.body, { error: "upstream unavailable" });
});

test("sendError: extra body fields (opts.body) are preserved alongside error", () => {
  const reply = fakeReply();
  sendError(reply, silentLog, grpcErr(status.PERMISSION_DENIED), { body: { schedules: [] } });

  assert.equal(reply.result.statusCode, 403);
  assert.deepEqual(reply.result.body, { schedules: [], error: "boom" });
});

// ── trimmedErrorMessage: the shared helper agui.ts's RUN_ERROR frame uses ────
// (CP2 follow-up finding: agui.ts:431 sent String(err) straight into the
// user-facing SSE frame, bypassing sendError's trim entirely).

test("trimmedErrorMessage: gRPC UNAVAILABLE never leaks the raw '14 UNAVAILABLE: ...' shape", () => {
  const err = grpcErr(status.UNAVAILABLE, "connect ECONNREFUSED 127.0.0.1:9090");
  const message = trimmedErrorMessage(err);

  assert.equal(message, "upstream unavailable");
  assert.doesNotMatch(message, /\d+ [A-Z_]+:/);
});

test("trimmedErrorMessage: caller-class gRPC error (4xx-mapped) returns the trimmed detail", () => {
  const err = grpcErr(status.NOT_FOUND, "agent xyz not found");
  assert.equal(trimmedErrorMessage(err), "agent xyz not found");
});

test("trimmedErrorMessage: non-gRPC Error never leaks stack/message text", () => {
  const err = new Error("pq: connection refused at 10.0.0.5:5432 — internal stack detail");
  err.stack = "Error: pq: connection refused\n    at Object.<anonymous> (/app/src/foo.ts:12:5)";
  const message = trimmedErrorMessage(err);

  assert.equal(message, "internal error");
  assert.doesNotMatch(message, /Error:/);
  assert.doesNotMatch(message, /at .+:\d+:\d+/);
});

test("sendError: response body never leaks a 'Error:' / stack-shaped string", () => {
  const reply = fakeReply();
  const err = new Error("boom");
  err.stack = "Error: boom\n    at Object.<anonymous> (/app/src/foo.ts:12:5)";
  sendError(reply, silentLog, err);

  const serialized = JSON.stringify(reply.result.body);
  assert.doesNotMatch(serialized, /Error:/);
  assert.doesNotMatch(serialized, /at .+:\d+:\d+/);
});
