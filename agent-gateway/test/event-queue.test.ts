// CP2 tests: bounded async event queue.
//
// WHY these tests exist: the inline queue arrays in core.ts and child-entry.ts
// are unbounded — a slow or disconnected SSE consumer causes unlimited memory
// growth. The EventQueue helper bounds the backlog; overflow is fail-closed
// (consumer sees a terminal error, not silent truncation). These tests prove
// the three observable behaviors: FIFO delivery, bounded overflow (fail-closed),
// and clean close.
import { test } from "node:test";
import assert from "node:assert/strict";
import { EventQueue } from "../src/pi/event-queue.js";

function withTimeout<T>(p: Promise<T>, ms: number, label: string): Promise<T> {
  return Promise.race([
    p,
    new Promise<never>((_, reject) =>
      setTimeout(() => reject(new Error(`timeout: ${label}`)), ms),
    ),
  ]);
}

// ── FIFO delivery ──────────────────────────────────────────────────────────────

test("EventQueue: push N <= cap items then drain delivers them FIFO", async () => {
  // WHY: the bounded queue must behave exactly like an unbounded queue when the
  // consumer keeps up — no items dropped, correct order preserved.
  const q = new EventQueue<number>({ maxSize: 8 });

  q.push(1);
  q.push(2);
  q.push(3);
  q.close();

  const received: number[] = [];
  for await (const item of q.drain()) {
    received.push(item);
  }

  assert.deepEqual(received, [1, 2, 3], "items must arrive FIFO");
});

// ── Overflow: fail-closed ──────────────────────────────────────────────────────

test("EventQueue: push beyond cap with stalled consumer → consumer observes terminal overflow error", async () => {
  // WHY: a disconnected SSE client must not cause unbounded memory growth.
  // Overflow must be fail-closed: the consumer receives an error (not silent
  // truncation), and the backlog must be bounded by maxSize at all times.
  const CAP = 4;
  const q = new EventQueue<string>({ maxSize: CAP });

  // Push one more than the cap without consuming. The (CAP+1)-th push triggers
  // the overflow transition; subsequent pushes are no-ops.
  for (let i = 0; i < CAP + 3; i++) {
    q.push(`item-${i}`);
  }

  let threw = false;
  let errorMessage = "";
  try {
    // The consumer will eventually pull the overflow sentinel.
    for await (const _item of q.drain()) {
      // intentionally stalled — we just need the overflow error
    }
  } catch (err) {
    threw = true;
    errorMessage = err instanceof Error ? err.message : String(err);
  }

  assert.equal(threw, true, "drain must throw on overflow (fail-closed, not silent truncation)");
  assert.ok(
    errorMessage.toLowerCase().includes("overflow"),
    `error message must mention overflow, got: ${errorMessage}`,
  );
});

test("EventQueue: internal backlog does not exceed maxSize after overflow", async () => {
  // WHY: the overflow guard must be enforced at push time — the queue must not
  // silently grow past maxSize even after overflow is detected. This is the
  // memory-bound guarantee the spec requires.
  const CAP = 4;
  const q = new EventQueue<number>({ maxSize: CAP });

  // Push far beyond cap.
  for (let i = 0; i < CAP * 10; i++) {
    q.push(i);
  }

  // The queue exposes its size for testability.
  assert.ok(
    q.size() <= CAP + 1,  // at most CAP items + the overflow sentinel
    `backlog must not exceed maxSize after overflow, size=${q.size()}`,
  );
});

// ── Close: clean drain ─────────────────────────────────────────────────────────

test("EventQueue: close() after some items → consumer drains buffered items then ends", async () => {
  // WHY: a normal run completion must drain all buffered items before the
  // generator terminates — no items may be lost on clean close.
  const q = new EventQueue<string>({ maxSize: 16 });

  q.push("a");
  q.push("b");
  q.push("c");
  q.close();

  const received: string[] = [];
  for await (const item of q.drain()) {
    received.push(item);
  }

  assert.deepEqual(received, ["a", "b", "c"], "all buffered items must be delivered before close");
});

test("EventQueue: drain ends cleanly after close() with empty queue", async () => {
  const q = new EventQueue<number>({ maxSize: 8 });
  q.close();

  const received: number[] = [];
  for await (const item of q.drain()) {
    received.push(item);
  }

  assert.deepEqual(received, [], "empty close must end without items or error");
});

// ── Default maxSize ────────────────────────────────────────────────────────────

test("EventQueue: default maxSize is 1024", async () => {
  // WHY: the spec requires 1024 as the default cap. Verify it is enforced —
  // exactly 1024 pushes without overflow, the 1025th triggers it.
  const q = new EventQueue<number>();

  for (let i = 0; i < 1024; i++) {
    q.push(i);
  }
  // One more should overflow.
  q.push(1024);

  let threw = false;
  try {
    for await (const _item of q.drain()) {}
  } catch {
    threw = true;
  }
  assert.equal(threw, true, "default cap must be 1024 — push #1025 must trigger overflow");
});

// ── Consumer early-stop via generator return() ─────────────────────────────────

test("EventQueue: early generator return() does not throw for the producer", async () => {
  // WHY: when the SSE client disconnects the outer for-await breaks, calling
  // generator.return(). The queue itself must not throw or hang — the producer
  // (which may still push) should see no-ops after consumer stops.
  const q = new EventQueue<number>({ maxSize: 8 });

  q.push(1);
  q.push(2);
  q.push(3);

  const gen = q.drain();
  const first = await withTimeout(gen.next(), 200, "first item");
  assert.equal(first.done, false);
  assert.equal(first.value, 1);

  // Return early — simulates SSE disconnect.
  await gen.return(undefined);

  // Push more — must be no-ops, not throw.
  q.push(4);
  q.push(5);
  q.close();

  // No assertion needed: the test passes if no throw/hang.
});
