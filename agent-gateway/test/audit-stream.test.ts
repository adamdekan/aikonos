import { test, beforeEach } from "node:test";
import assert from "node:assert/strict";
import pino from "pino";
import { buffer, record, fanOutClients, natsConnectOptions, startAuditConsumer } from "../src/audit/stream";

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
  fanOutClients().set(resTenant, { tenant: "tenant-a" });

  record({ event_id: "e1", tenant_id: "tenant-b" });

  assert.equal(unfiltered.length, 1, "unfiltered client should receive every event");
  assert.equal(filtered.length, 0, "tenant-filtered client should not receive a mismatched-tenant event");
});

test("record() fan-out: tenant-filtered client receives matching-tenant events", () => {
  const filtered: string[] = [];
  const resTenant = fakeRes((s) => { filtered.push(s); return true; });
  fanOutClients().set(resTenant, { tenant: "tenant-a" });

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

// H-07: a member's stream carries only events they acted in or that were
// done on their behalf; an admin scope (no user) carries the whole tenant.
test("record() fan-out: member scope receives only own and on-behalf-of events", () => {
  const got: string[] = [];
  fanOutClients().set(fakeRes((s) => { got.push(s); return true; }), { tenant: "tenant-a", user: "alice" });

  record({ event_id: "own", tenant_id: "tenant-a", actor_user_id: "alice" });
  record({ event_id: "obo", tenant_id: "tenant-a", actor_user_id: "bob", context: { obo: { subject: "alice" } } });
  record({ event_id: "colleague", tenant_id: "tenant-a", actor_user_id: "bob" });
  record({ event_id: "foreign", tenant_id: "tenant-b", actor_user_id: "alice" });

  assert.deepEqual(got.map((l) => (JSON.parse(l.slice(6)) as { event_id: string }).event_id), ["own", "obo"]);
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

// nats.js ignores URL credentials, so compose's nats://<token>@nats:4222 must be
// split into an explicit token, and the logged server URL must not carry it.
test("natsConnectOptions: URL token becomes connect token and leaves the server URL", () => {
  assert.deepEqual(natsConnectOptions("nats://s3cr3t@nats:4222"), { servers: "nats://nats:4222", token: "s3cr3t" });
  assert.deepEqual(natsConnectOptions("nats://u:p%40ss@nats:4222"), { servers: "nats://nats:4222", user: "u", pass: "p@ss" });
  assert.deepEqual(natsConnectOptions("nats://nats:4222"), { servers: "nats://nats:4222" });
});
