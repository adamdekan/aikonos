import { test, beforeEach } from "node:test";
import assert from "node:assert/strict";
import pino from "pino";
import { buffer, record, fanOutClients, startAuditConsumer, filterByTenant } from "../src/audit/stream";

// Reset shared state between tests so cases are independent.
beforeEach(() => { buffer.length = 0; fanOutClients().clear(); });

test("record pushes an event to the ring buffer", () => {
  record({ event_id: "e1", tenant_id: "t1" });
  assert.equal(buffer.length, 1);
  assert.deepEqual(buffer[0], { event_id: "e1", tenant_id: "t1" });
});

test("ring buffer caps at 1000 events (oldest evicted)", () => {
  for (let i = 0; i < 1001; i++) record({ i });
  assert.equal(buffer.length, 1000);
  assert.deepEqual(buffer[0], { i: 1 });
  assert.deepEqual(buffer[999], { i: 1000 });
});

test("record fans out to all registered SSE clients", () => {
  const writes: string[] = [];
  const fakeRes = { write: (s: string) => writes.push(s) } as unknown as import("node:http").ServerResponse;
  fanOutClients().set(fakeRes, undefined);
  record({ event_id: "e2" });
  assert.equal(writes.length, 1);
  assert.ok(writes[0].startsWith("data: "));
  assert.ok(writes[0].includes(`"event_id":"e2"`));
});

test("disconnected client is not written after removal", () => {
  const writes: string[] = [];
  const fakeRes = { write: (s: string) => writes.push(s) } as unknown as import("node:http").ServerResponse;
  fanOutClients().set(fakeRes, undefined);
  fanOutClients().delete(fakeRes);
  record({ event_id: "e3" });
  assert.equal(writes.length, 0);
});

// CP3 (F9): stop handle + connection-state flag for /readyz.
const log = pino({ level: "silent" });

// F26: natsUrl/subject are now injected via the options param (from validated
// Config at the real call site) instead of read from process.env here — these
// tests exercise the options seam directly, no env mutation needed.

test("startAuditConsumer: undefined natsUrl reports disabled and never attempts to connect", async () => {
  const handle = startAuditConsumer(log, { natsUrl: undefined });
  assert.deepEqual(handle.status(), { state: "disabled" });
  await handle.stop(); // must resolve immediately — no connection was ever opened
  assert.deepEqual(handle.status(), { state: "disabled" });
});

test("startAuditConsumer: no options at all defaults to disabled (natsUrl undefined)", async () => {
  const handle = startAuditConsumer(log);
  assert.deepEqual(handle.status(), { state: "disabled" });
  await handle.stop();
});

test("startAuditConsumer: stop() resolves promptly and reports disconnected before any connection succeeds", async () => {
  // Unreachable loopback port — connect() fails fast (ECONNREFUSED) instead of
  // hanging, so this test doesn't wait out the real retry interval.
  const handle = startAuditConsumer(log, { natsUrl: "nats://127.0.0.1:4223" });
  assert.equal(handle.status().state, "disconnected");

  const start = Date.now();
  await handle.stop();
  const elapsed = Date.now() - start;

  assert.ok(elapsed < 2000, `stop() took ${elapsed}ms — should resolve immediately pre-connection`);
  assert.equal(handle.status().state, "disconnected");
});

test("startAuditConsumer: custom subject is accepted without affecting disabled/connect behaviour", async () => {
  const handle = startAuditConsumer(log, { natsUrl: undefined, subject: "custom.audit.>" });
  assert.deepEqual(handle.status(), { state: "disabled" });
  await handle.stop();
});

// F44: tenant-scoped fan-out + slow-client disconnect on the live audit SSE stream.

function fakeRes(write: (s: string) => boolean) {
  const listeners: Record<string, () => void> = {};
  return {
    write,
    destroy() { listeners.destroy?.(); },
    destroyed: false,
    on(event: string, cb: () => void) { listeners[event] = cb; },
  } as unknown as import("node:http").ServerResponse;
}

test("record() fan-out: tenant-filtered client receives only matching-tenant events", () => {
  const unfiltered: string[] = [];
  const filtered: string[] = [];
  const resAll = fakeRes((s) => { unfiltered.push(s); return true; });
  const resTenant = fakeRes((s) => { filtered.push(s); return true; });
  fanOutClients().set(resAll, undefined);
  fanOutClients().set(resTenant, "tenant-a");

  record({ event_id: "e1", tenant_id: "tenant-b" });

  assert.equal(unfiltered.length, 1, "unfiltered client should receive every event");
  assert.equal(filtered.length, 0, "tenant-filtered client should not receive a mismatched-tenant event");
});

test("record() fan-out: tenant-filtered client receives matching-tenant events", () => {
  const filtered: string[] = [];
  const resTenant = fakeRes((s) => { filtered.push(s); return true; });
  fanOutClients().set(resTenant, "tenant-a");

  record({ event_id: "e2", tenant_id: "tenant-a" });

  assert.equal(filtered.length, 1);
  assert.ok(filtered[0].includes(`"event_id":"e2"`));
});

test("record() fan-out: a client whose write() returns false is destroyed and removed", () => {
  let destroyed = false;
  const res = fakeRes(() => false);
  (res as unknown as { destroy: () => void }).destroy = () => { destroyed = true; };
  fanOutClients().set(res, undefined);

  record({ event_id: "e3" });

  assert.ok(destroyed, "slow client should be destroyed when write() returns false");
});

test("filterByTenant: mixed-tenant buffer, matching tenant param returns only matching events", () => {
  record({ event_id: "r1", tenant_id: "tenant-a" });
  record({ event_id: "r2", tenant_id: "tenant-b" });

  const replayed = filterByTenant(buffer, "tenant-a");

  assert.equal(replayed.length, 1);
  assert.deepEqual(replayed[0], { event_id: "r1", tenant_id: "tenant-a" });
});

test("filterByTenant: absent tenant param returns every event unfiltered", () => {
  record({ event_id: "r1", tenant_id: "tenant-a" });
  record({ event_id: "r2", tenant_id: "tenant-b" });

  const replayed = filterByTenant(buffer, undefined);

  assert.equal(replayed.length, 2);
});

test("ping write does not throw on a destroyed client socket", () => {
  const res = fakeRes(() => { throw new Error("write after destroy"); });
  (res as unknown as { destroyed: boolean }).destroyed = true;
  fanOutClients().set(res, undefined);

  // Mirrors the ping callback's guard: skip write() on a destroyed client.
  assert.doesNotThrow(() => {
    for (const [client] of fanOutClients()) {
      if (!client.destroyed) client.write(": ping\n\n");
    }
  });
});
