// GET /api/audit + /api/audit/stream route handlers. Registers the real registerAuditRoutes
// (src/audit/stream.ts) so a mutation to the production route fails these
// tests — no hand-copied handler.
//
// Pins: (a) an unauthenticated request is 401, never data; (b) a principal
// only ever sees its own tenant's events; (c) an explicit ?tenant= for a
// foreign tenant is ignored — the broker is single-tenant-per-deployment, so
// there is no such thing as an authorized cross-tenant admin on this surface.
import { test, beforeEach } from "node:test";
import assert from "node:assert/strict";
import { request as httpRequest } from "node:http";
import Fastify from "fastify";
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

async function buildApp(opts: { tenantId?: string } = {}) {
  const { privateKey, jwk } = await makeKey();
  const jwksResolver = await localResolver(jwk);
  const tenantId = opts.tenantId ?? "tenant-a";
  const token = await mintToken(privateKey, tenantId);

  const app = Fastify({ logger: false });
  registerAuditRoutes(app, { jwksResolver, verifyOpts: VERIFY_OPTS });

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
