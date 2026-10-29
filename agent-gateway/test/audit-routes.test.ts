// GET /api/audit + /api/audit/stream route handlers. Registers the real registerAuditRoutes
// (src/audit/stream.ts) so a mutation to the production route fails these
// tests — no hand-copied handler.
//
// Pins: (a) an unauthenticated request is 401, never data; (b) a principal
// only ever sees its own tenant's events; (c) an explicit ?tenant= for a
// foreign tenant is ignored — the broker is single-tenant-per-deployment, so
// there is no such thing as an authorized cross-tenant admin on this surface;
// (d) H-07: only a tenant admin sees the tenant stream, a member sees only
// their own events, and a failed admin probe means member.
import { test, beforeEach } from "node:test";
import assert from "node:assert/strict";
import { request as httpRequest } from "node:http";
import Fastify from "fastify";
import { status } from "@grpc/grpc-js";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import { registerAuditRoutes, buffer, record } from "../src/audit/stream.js";
import type { JwksResolver } from "../src/auth/verify.js";

beforeEach(() => { buffer.length = 0; });

// ── Real bearer auth via a local JWKS resolver (mirrors files-list-route.test.ts) ──

async function makeKey() {
  const { publicKey, privateKey } = await generateKeyPair("RS256");
  const jwk: JWK = { ...(await exportJWK(publicKey)), kid: "k1", alg: "RS256", use: "sig" };
  return { privateKey, jwk };
}

async function localResolver(jwk: JWK): Promise<JwksResolver> {
  const key = await importJWK(jwk, "RS256");
  return () => Promise.resolve(key);
}

const VERIFY_OPTS = {
  issuer: "http://localhost:18080/realms/aikonos",
  audience: "aikonos-broker",
};

async function mintToken(
  privateKey: Awaited<ReturnType<typeof generateKeyPair>>["privateKey"],
  tenantId: string,
) {
  return new SignJWT({
    sub: "alice@example.com",
    email: "alice@example.com",
    tenant_id: tenantId,
  })
    .setProtectedHeader({ alg: "RS256", kid: "k1" })
    .setIssuer(VERIFY_OPTS.issuer)
    .setAudience(VERIFY_OPTS.audience)
    .setIssuedAt()
    .setExpirationTime("1h")
    .sign(privateKey);
}

// Fake broker admin probe: an admin's GetObservabilityInfo succeeds, anyone
// else's is PERMISSION_DENIED (or `probeErr` when given).
function fakeNorth(admin: boolean, probeErr?: Error) {
  return {
    getObservabilityInfo: () =>
      admin
        ? Promise.resolve({ otelEndpoint: "", exportEnabled: false, exportedJob: "" })
        : Promise.reject(probeErr ?? Object.assign(new Error("tenant admin required"), { code: status.PERMISSION_DENIED })),
  };
}

async function buildApp(opts: { tenantId?: string; admin?: boolean; probeErr?: Error } = {}) {
  const { privateKey, jwk } = await makeKey();
  const jwksResolver = await localResolver(jwk);
  const tenantId = opts.tenantId ?? "tenant-a";
  const token = await mintToken(privateKey, tenantId);

  const app = Fastify({ logger: false });
  registerAuditRoutes(app, { jwksResolver, verifyOpts: VERIFY_OPTS, north: fakeNorth(opts.admin ?? true, opts.probeErr) });

  return { app, token, tenantId };
}

test("GET /api/audit — no bearer → 401, no events leaked", async () => {
  const { app } = await buildApp();
  await app.ready();
  record({ event_id: "e1", tenant_id: "tenant-a" });

  const res = await app.inject({ method: "GET", url: "/api/audit" });

  assert.equal(res.statusCode, 401);
  await app.close();
});

test("GET /api/audit — authenticated principal sees only its own tenant's events by default", async () => {
  const { app, token } = await buildApp();
  await app.ready();
  record({ event_id: "mine", tenant_id: "tenant-a" });
  record({ event_id: "other", tenant_id: "tenant-b" });

  const res = await app.inject({ method: "GET", url: "/api/audit", headers: { authorization: `Bearer ${token}` } });

  assert.equal(res.statusCode, 200);
  const body = JSON.parse(res.body) as { events: { event_id: string }[] };
  assert.deepEqual(body.events.map((e) => e.event_id), ["mine"]);
  await app.close();
});

test("GET /api/audit — ?tenant= for a foreign tenant is ignored, own tenant used instead", async () => {
  const { app, token } = await buildApp();
  await app.ready();
  record({ event_id: "mine", tenant_id: "tenant-a" });
  record({ event_id: "other", tenant_id: "tenant-b" });

  const res = await app.inject({
    method: "GET",
    url: "/api/audit?tenant=tenant-b",
    headers: { authorization: `Bearer ${token}` },
  });

  assert.equal(res.statusCode, 200);
  const body = JSON.parse(res.body) as { events: { event_id: string }[] };
  assert.deepEqual(body.events.map((e) => e.event_id), ["mine"], "a foreign ?tenant= must never be honored");
  await app.close();
});

test("GET /api/audit/stream — no bearer → 401, no SSE stream opened", async () => {
  const { app } = await buildApp();
  await app.ready();

  const res = await app.inject({ method: "GET", url: "/api/audit/stream" });

  assert.equal(res.statusCode, 401);
  await app.close();
});

// The stream endpoint never calls reply.raw.end() on the happy path (it stays
// open until client disconnect) — app.inject() would hang waiting for the
// response to finish, so this one test uses a real listener + a manual
// destroy once the replay body has arrived (mirrors egress-proxy.test.ts's
// never-ending-stream pattern).
function readSseSnippet(port: number, token: string, path: string): Promise<{ status: number; body: string }> {
  return new Promise((resolve, reject) => {
    let settled = false;
    const settle = (v: { status: number; body: string }) => {
      if (settled) return;
      settled = true;
      clearTimeout(safety);
      resolve(v);
    };
    // Safety net: fail loud with whatever was collected instead of hanging
    // the whole suite if the replay data never arrives.
    const safety = setTimeout(() => settle({ status: 0, body: "TIMED OUT waiting for replay data" }), 2000);

    const req = httpRequest(
      { host: "127.0.0.1", port, path, method: "GET", headers: { authorization: `Bearer ${token}` } },
      (res) => {
        let buf = "";
        res.on("data", (d: Buffer) => {
          buf += d.toString();
          // Wait for the replay data itself (not just the leading "retry:"
          // hint) before tearing down the still-open connection.
          if (buf.includes('"event_id"')) {
            req.destroy();
            settle({ status: res.statusCode ?? 0, body: buf });
          }
        });
        res.on("end", () => settle({ status: res.statusCode ?? 0, body: buf }));
      },
    );
    req.on("error", (err) => {
      // destroy()ing the request once we have what we need triggers a socket
      // error on some Node versions — benign, already resolved above.
      if (!settled) reject(err);
    });
    req.end();
  });
}

test("GET /api/audit/stream — replay is scoped to the principal's own tenant", async () => {
  const { app, token } = await buildApp();
  record({ event_id: "mine", tenant_id: "tenant-a" });
  record({ event_id: "other", tenant_id: "tenant-b" });

  await app.listen({ port: 0, host: "127.0.0.1" });
  const address = app.server.address();
  if (address === null || typeof address === "string") throw new Error("expected a bound TCP address");

  try {
    const { status, body } = await readSseSnippet(address.port, token, "/api/audit/stream");
    assert.equal(status, 200);
    assert.ok(body.includes(`"event_id":"mine"`));
    assert.ok(!body.includes(`"event_id":"other"`));
  } finally {
    app.server.closeAllConnections();
    await app.close();
  }
});

test("GET /api/audit/stream — ?tenant= for a foreign tenant is ignored in the replay", async () => {
  const { app, token } = await buildApp();
  record({ event_id: "mine", tenant_id: "tenant-a" });
  record({ event_id: "other", tenant_id: "tenant-b" });

  await app.listen({ port: 0, host: "127.0.0.1" });
  const address = app.server.address();
  if (address === null || typeof address === "string") throw new Error("expected a bound TCP address");

  try {
    const { status, body } = await readSseSnippet(address.port, token, "/api/audit/stream?tenant=tenant-b");
    assert.equal(status, 200);
    assert.ok(body.includes(`"event_id":"mine"`), "own-tenant event must still be replayed");
    assert.ok(!body.includes(`"event_id":"other"`), "a foreign ?tenant= must never be honored");
  } finally {
    app.server.closeAllConnections();
    await app.close();
  }
});

// ── H-07: admin-only tenant stream ───────────────────────────────────────────

function seedMixed() {
  record({ event_id: "own", tenant_id: "tenant-a", actor_user_id: "alice@example.com" });
  record({ event_id: "obo", tenant_id: "tenant-a", actor_user_id: "approver@example.com", context: { obo: { subject: "alice@example.com" } } });
  record({ event_id: "colleague", tenant_id: "tenant-a", actor_user_id: "bob@example.com" });
}

async function listIds(opts: { admin?: boolean; probeErr?: Error }) {
  const { app, token } = await buildApp(opts);
  await app.ready();
  seedMixed();
  const res = await app.inject({ method: "GET", url: "/api/audit", headers: { authorization: `Bearer ${token}` } });
  await app.close();
  assert.equal(res.statusCode, 200);
  return (JSON.parse(res.body) as { events: { event_id: string }[] }).events.map((e) => e.event_id).sort();
}

test("GET /api/audit — a tenant admin sees every event in the tenant", async () => {
  assert.deepEqual(await listIds({ admin: true }), ["colleague", "obo", "own"]);
});

test("GET /api/audit — a member sees only their own and on-behalf-of events", async () => {
  assert.deepEqual(await listIds({ admin: false }), ["obo", "own"]);
});

test("GET /api/audit — a failed admin probe falls back to the member scope", async () => {
  assert.deepEqual(await listIds({ admin: false, probeErr: new Error("broker unreachable") }), ["obo", "own"]);
});

test("GET /api/audit/stream — a member's replay omits colleagues' events", async () => {
  const { app, token } = await buildApp({ admin: false });
  seedMixed();
  await app.listen({ port: 0, host: "127.0.0.1" });
  const address = app.server.address();
  if (address === null || typeof address === "string") throw new Error("expected a bound TCP address");
  try {
    const { status: code, body } = await readSseSnippet(address.port, token, "/api/audit/stream");
    assert.equal(code, 200);
    assert.ok(body.includes(`"event_id":"own"`));
    assert.ok(!body.includes(`"event_id":"colleague"`));
  } finally {
    app.server.closeAllConnections();
    await app.close();
  }
});

test("GET /api/audit/stream — open streams per principal are capped (429)", async () => {
  const { app, token } = await buildApp();
  record({ event_id: "e", tenant_id: "tenant-a" });
  await app.listen({ port: 0, host: "127.0.0.1" });
  const address = app.server.address();
  if (address === null || typeof address === "string") throw new Error("expected a bound TCP address");
  const held: import("node:http").ClientRequest[] = [];
  const open = () => new Promise<number>((resolve, reject) => {
    const req = httpRequest({ host: "127.0.0.1", port: address.port, path: "/api/audit/stream", headers: { authorization: `Bearer ${token}` } },
      (res) => resolve(res.statusCode ?? 0));
    req.on("error", reject);
    req.end();
    held.push(req);
  });
  try {
    for (let i = 0; i < 3; i++) assert.equal(await open(), 200);
    assert.equal(await open(), 429);
  } finally {
    for (const r of held) r.destroy();
    app.server.closeAllConnections();
    await app.close();
  }
});

// ── F59: a live stream is re-validated, not fixed at connect time ─────────────

// openStream collects SSE data until the server ends the response (or 4 s).
function openStream(port: number, token: string): { ended: Promise<boolean>; body: () => string } {
  let buf = "";
  const ended = new Promise<boolean>((resolve) => {
    const safety = setTimeout(() => { req.destroy(); resolve(false); }, 4000);
    const req = httpRequest(
      { host: "127.0.0.1", port, path: "/api/audit/stream", method: "GET", headers: { authorization: `Bearer ${token}` } },
      (res) => {
        res.on("data", (d: Buffer) => { buf += d.toString(); });
        res.on("end", () => { clearTimeout(safety); resolve(true); });
      },
    );
    req.on("error", () => {});
    req.end();
  });
  return { ended, body: () => buf };
}

async function listenWith(north: { getObservabilityInfo: () => Promise<unknown> }, expiresIn: string) {
  const { privateKey, jwk } = await makeKey();
  const token = await new SignJWT({ sub: "alice@example.com", email: "alice@example.com", tenant_id: "tenant-a" })
    .setProtectedHeader({ alg: "RS256", kid: "k1" })
    .setIssuer(VERIFY_OPTS.issuer)
    .setAudience(VERIFY_OPTS.audience)
    .setIssuedAt()
    .setExpirationTime(expiresIn)
    .sign(privateKey);
  const app = Fastify({ logger: false });
  registerAuditRoutes(app, { jwksResolver: await localResolver(jwk), verifyOpts: VERIFY_OPTS, north: north as never, recheckMs: 100 });
  await app.listen({ port: 0, host: "127.0.0.1" });
  const address = app.server.address();
  if (address === null || typeof address === "string") throw new Error("expected a bound TCP address");
  return { app, token, port: address.port };
}

test("F59 GET /api/audit/stream — the stream ends when its bearer expires", async () => {
  const { app, token, port } = await listenWith(fakeNorth(true), "2s");
  try {
    const s = openStream(port, token);
    assert.equal(await s.ended, true, "the server must end the stream at the token's exp");
  } finally {
    app.server.closeAllConnections();
    await app.close();
  }
});

test("F59 GET /api/audit/stream — a revoked session or blocked user ends the open stream (no downgrade)", async () => {
  for (const err of [
    Object.assign(new Error("revoked"), { code: status.UNAUTHENTICATED, details: "session revoked; sign in again" }),
    Object.assign(new Error("blocked"), { code: status.PERMISSION_DENIED, details: "user is blocked" }),
  ]) {
    let admin = true;
    const north = { getObservabilityInfo: () => (admin ? Promise.resolve({}) : Promise.reject(err)) };
    const { app, token, port } = await listenWith(north, "1h");
    try {
      const s = openStream(port, token);
      await new Promise((r) => setTimeout(r, 150));
      admin = false;
      assert.equal(await s.ended, true, `${err.details}: the stream must close`);
    } finally {
      app.server.closeAllConnections();
      await app.close();
    }
  }
});

test("F59 GET /api/audit/stream — an admin demoted mid-stream stops receiving other users' events", async () => {
  let admin = true;
  const north = { getObservabilityInfo: () => (admin ? Promise.resolve({}) : Promise.reject(Object.assign(new Error("x"), { code: status.PERMISSION_DENIED, details: "tenant admin required" }))) };
  const { app, token, port } = await listenWith(north, "1h");
  try {
    const s = openStream(port, token);
    await new Promise((r) => setTimeout(r, 150));
    record({ event_id: "bob-before", tenant_id: "tenant-a", actor_user_id: "bob" });
    admin = false;
    await new Promise((r) => setTimeout(r, 300));
    record({ event_id: "bob-after", tenant_id: "tenant-a", actor_user_id: "bob" });
    record({ event_id: "alice-after", tenant_id: "tenant-a", actor_user_id: "alice@example.com" });
    await new Promise((r) => setTimeout(r, 50));
    assert.ok(s.body().includes("bob-before"));
    assert.ok(!s.body().includes("bob-after"), "a demoted admin must not see other users' events");
    assert.ok(s.body().includes("alice-after"), "the member keeps their own events");
  } finally {
    app.server.closeAllConnections();
    await app.close();
  }
});

test("F59 GET /api/audit — a revoked session gets 401, not member-scope events", async () => {
  const { app, token } = await buildApp({ admin: false, probeErr: Object.assign(new Error("r"), { code: status.UNAUTHENTICATED }) });
  await app.ready();
  record({ event_id: "mine", tenant_id: "tenant-a", actor_user_id: "alice@example.com" });
  const res = await app.inject({ method: "GET", url: "/api/audit", headers: { authorization: `Bearer ${token}` } });
  assert.equal(res.statusCode, 401);
  await app.close();
});
