// Route-level coverage for GET /schedules under CP2 (F27)'s shared error
// mapper: a broker UNAVAILABLE must surface as 502 (gateway/upstream trouble),
// not the old adminErrorCode fallback of 400 (which conflated "bad request"
// with "broker unreachable").
//
// Registers the real registerScheduleRoutes against a fake north client so a
// mutation to the production route fails this test — no hand-copied handler.
import { test } from "node:test";
import assert from "node:assert/strict";
import Fastify from "fastify";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import { registerScheduleRoutes } from "../src/routes/schedules.js";
import type { ListScheduledRunsResponse } from "../gen/ts/proto/broker.js";
import type { JwksResolver } from "../src/auth/verify.js";

const UNAVAILABLE_CODE = 14;

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

async function mintToken(privateKey: Awaited<ReturnType<typeof generateKeyPair>>["privateKey"]) {
  return new SignJWT({
    sub: "alice@example.com",
    email: "alice@example.com",
    tenant_id: "aikonos-dev",
  })
    .setProtectedHeader({ alg: "RS256", kid: "k1" })
    .setIssuer(VERIFY_OPTS.issuer)
    .setAudience(VERIFY_OPTS.audience)
    .setIssuedAt()
    .setExpirationTime("1h")
    .sign(privateKey);
}

async function buildApp(mode: "ok" | "unavailable") {
  const { privateKey, jwk } = await makeKey();
  const jwksResolver = await localResolver(jwk);
  const token = await mintToken(privateKey);

  const app = Fastify({ logger: false });

  const north = {
    async listScheduledRuns(_req: unknown, _token?: string): Promise<ListScheduledRunsResponse> {
      if (mode === "unavailable") {
        throw Object.assign(new Error("Unavailable"), {
          code: UNAVAILABLE_CODE,
          details: "connect ECONNREFUSED 127.0.0.1:50051",
        });
      }
      return { runs: [], fgaEnabled: true, warnings: [] };
    },
    createScheduledRun: (): never => { throw new Error("not used in this test"); },
    updateScheduledRun: (): never => { throw new Error("not used in this test"); },
    deleteScheduledRun: (): never => { throw new Error("not used in this test"); },
  };

  registerScheduleRoutes(app, { clients: { north }, jwksResolver, verifyOpts: VERIFY_OPTS });

  return { app, token };
}

test("GET /schedules — ok → 200 { schedules: [] }", async () => {
  const { app, token } = await buildApp("ok");
  await app.ready();

  const res = await app.inject({
    method: "GET",
    url: "/schedules",
    headers: { authorization: `Bearer ${token}` },
  });

  assert.equal(res.statusCode, 200);

  await app.close();
});

test("GET /schedules — broker UNAVAILABLE → 502, not the old 400 fallback", async () => {
  const { app, token } = await buildApp("unavailable");
  await app.ready();

  const res = await app.inject({
    method: "GET",
    url: "/schedules",
    headers: { authorization: `Bearer ${token}` },
  });

  assert.equal(res.statusCode, 502, "expect 502 UNAVAILABLE");
  const body = JSON.parse(res.body) as { schedules: unknown[]; error: string };
  assert.deepEqual(body.schedules, [], "schedules stays an empty array on error");
  assert.equal(body.error, "upstream unavailable", "5xx body is generic, never the raw connect-refused detail");

  await app.close();
});

test("GET /schedules — error response body never leaks 'Error:' or connection internals", async () => {
  const { app, token } = await buildApp("unavailable");
  await app.ready();

  const res = await app.inject({
    method: "GET",
    url: "/schedules",
    headers: { authorization: `Bearer ${token}` },
  });

  assert.doesNotMatch(res.body, /Error:/);
  assert.doesNotMatch(res.body, /ECONNREFUSED/);
  assert.doesNotMatch(res.body, /at .+:\d+:\d+/);

  await app.close();
});
