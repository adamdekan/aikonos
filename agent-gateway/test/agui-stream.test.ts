// Tests for AGUIStream heartbeat (F31): retry hint + periodic ping, and
// write-backpressure overflow protection. Mirrors the audit/stream.ts idiom
// (retry: 3000 once, : ping every interval) but scoped to the per-run
// AGUIStream instance so the interval can be cleared on connection close.
import { test } from "node:test";
import assert from "node:assert/strict";
import { AGUIStream } from "../src/agui/stream.js";

// FakeSink records every chunk written and lets tests control the
// backpressure signal (write's boolean return value).
class FakeSink {
  chunks: string[] = [];
  writable = true;

  write(chunk: string): boolean {
    this.chunks.push(chunk);
    return this.writable;
  }
}

test("stream start writes the retry hint once", () => {
  const sink = new FakeSink();
  const stream = new AGUIStream(sink, "thread-1", "run-1", { pingMs: 1_000 });
  stream.stopHeartbeat();

  assert.deepEqual(sink.chunks, ["retry: 3000\n\n"]);
});

test("a ping comment is emitted after the interval elapses", () => {
  const sink = new FakeSink();
  const stream = new AGUIStream(sink, "thread-1", "run-1", { pingMs: 20 });
  return new Promise<void>((resolve) => {
    setTimeout(() => {
      stream.stopHeartbeat();
      assert.ok(sink.chunks.includes(": ping\n\n"), "expected a ping comment to have been written");
      resolve();
    }, 60);
  });
});

test("overflow: onOverflow fires exactly once past the 4 MiB pending-bytes cap on a never-draining sink", () => {
  const sink = new FakeSink();
  sink.writable = false; // write() always returns false — nothing ever drains
  let overflowCount = 0;
  const stream = new AGUIStream(sink, "thread-1", "run-1", {
    pingMs: 1_000,
    onOverflow: () => {
      overflowCount++;
    },
  });

  // Each textDelta chunk is small; push enough to cross the 4 MiB cap.
  const bigDelta = "x".repeat(64 * 1024); // 64 KiB per call
  for (let i = 0; i < 100; i++) {
    stream.textDelta(bigDelta);
  }
  stream.stopHeartbeat();

  assert.equal(overflowCount, 1);
});

test("toolCall includes toolDescription on the START frame when a description is supplied", () => {
  const sink = new FakeSink();
  const stream = new AGUIStream(sink, "thread-1", "run-1", { pingMs: 1_000 });
  stream.stopHeartbeat();

  stream.toolCall("tc-1", "web_fetch", { url: "https://example.com" }, "Fetch a public web page");
  // The START frame is the first tool chunk (after the retry hint at index 0).
  const startChunk = sink.chunks.find((c) => c.includes("TOOL_CALL_START"));
  assert.ok(startChunk, "a TOOL_CALL_START frame must be written");
  assert.ok(startChunk!.includes('"toolDescription":"Fetch a public web page"'), "START frame must carry toolDescription");
});

test("toolCall omits toolDescription when none is supplied (unchanged legacy shape)", () => {
  const sink = new FakeSink();
  const stream = new AGUIStream(sink, "thread-1", "run-1", { pingMs: 1_000 });
  stream.stopHeartbeat();

  stream.toolCall("tc-2", "web_fetch", {});
  const startChunk = sink.chunks.find((c) => c.includes("TOOL_CALL_START"));
  assert.ok(startChunk, "a TOOL_CALL_START frame must be written");
  assert.ok(!startChunk!.includes("toolDescription"), "no toolDescription field when omitted");
});

test("drain resets the pending-bytes counter so a later sub-cap burst does not overflow", () => {
  const sink = new FakeSink();
  sink.writable = false;
  let overflowCount = 0;
  const stream = new AGUIStream(sink, "thread-1", "run-1", {
    pingMs: 1_000,
    onOverflow: () => {
      overflowCount++;
    },
  });

  const chunk = "x".repeat(64 * 1024);
  // Push a burst well under the 4 MiB cap, then simulate a drain event.
  for (let i = 0; i < 10; i++) stream.textDelta(chunk); // ~640 KiB, still ok
  stream.notifyDrain();
  for (let i = 0; i < 10; i++) stream.textDelta(chunk); // another ~640 KiB after reset
  stream.stopHeartbeat();

  assert.equal(overflowCount, 0);
});
