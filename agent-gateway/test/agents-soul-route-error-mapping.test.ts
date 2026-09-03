// Route-level coverage for GET/PUT /agents/:id/soul under CP2 (F27)'s shared
// error mapper. Replaces test/soul-error-code.test.ts, which tested a private
// soulErrorCode function that F27 deletes (agents.ts now shares
// src/http-errors.ts's grpcToHttp/sendError with every other route).
//
// Registers the real registerAgentsRoutes against a fake north client so a
// mutation to the production route fails this test — no hand-copied handler.
import { test } from "node:test";
import assert from "node:assert/strict";
import Fastify from "fastify";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import { registerAgentsRoutes } from "../src/routes/agents.js";
import type { GetAgentSoulResponse } from "../gen/ts/proto/broker.js";
import type { JwksResolver } from "../src/auth/verify.js";

const PERMISSION_DENIED_CODE = 7;
const NOT_FOUND_CODE = 5;

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

function makeFakeSupervisor(): { evictCalls: Array<{ agentId: string; reason: string }>; supervisor: Pick<import("../src/ipc/supervisor.js").ChildSupervisor, "evictIdleForAgent"> } {
  const evictCalls: Array<{ agentId: string; reason: string }> = [];
  return {
    evictCalls,
    supervisor: {
      evictIdleForAgent(agentId: string, reason: string): void {
        evictCalls.push({ agentId, reason });
      },
    },
  };
}

async function buildApp(mode: "ok" | "denied" | "not-found", opts?: { setAgentSoul?: (req: { tenantId: string; userId: string; agentId: string; soul: string }) => Promise<{ soul: string }> }) {
  const { privateKey, jwk } = await makeKey();
  const jwksResolver = await localResolver(jwk);
  const token = await mintToken(privateKey);

  const app = Fastify({ logger: false });
  const fakeSupervisor = makeFakeSupervisor();

  const north = {
    listMyAgents: (): never => { throw new Error("not used in this test"); },
    async getAgentSoul(_req: unknown, _token?: string): Promise<GetAgentSoulResponse> {
      if (mode === "denied") {
        throw Object.assign(new Error("PermissionDenied"), { code: PERMISSION_DENIED_CODE, details: "not your agent" });
      }
      if (mode === "not-found") {
        throw Object.assign(new Error("NotFound"), { code: NOT_FOUND_CODE, details: "agent nope-1 not found" });
      }
      return { soul: "You are a helpful assistant." };
    },
    setAgentSoul: opts?.setAgentSoul ?? ((): never => { throw new Error("not used in this test"); }),
  };

  registerAgentsRoutes(app, { clients: { north }, jwksResolver, verifyOpts: VERIFY_OPTS, supervisor: fakeSupervisor.supervisor });

  return { app, token, evictCalls: fakeSupervisor.evictCalls };
}

test("GET /agents/:id/soul — found → 200 { soul }", async () => {
  const { app, token } = await buildApp("ok");
  await app.ready();

  const res = await app.inject({
    method: "GET",
    url: "/agents/agent-1/soul",
    headers: { authorization: `Bearer ${token}` },
  });

  assert.equal(res.statusCode, 200);
  const body = JSON.parse(res.body) as { soul: string };
  assert.equal(body.soul, "You are a helpful assistant.");

  await app.close();
});

test("GET /agents/:id/soul — broker NOT_FOUND → 404, not a bare 400 (soulErrorCode's old exclusive 404 case)", async () => {
  const { app, token } = await buildApp("not-found");
  await app.ready();

  const res = await app.inject({
    method: "GET",
    url: "/agents/nope-1/soul",
    headers: { authorization: `Bearer ${token}` },
  });

  assert.equal(res.statusCode, 404, "expect 404 NOT_FOUND");
  const body = JSON.parse(res.body) as { error: string };
  assert.equal(body.error, "agent nope-1 not found", "caller-actionable detail, not String(err)");

  await app.close();
});

test("GET /agents/:id/soul — broker PERMISSION_DENIED → 403", async () => {
  const { app, token } = await buildApp("denied");
  await app.ready();

  const res = await app.inject({
    method: "GET",
    url: "/agents/agent-1/soul",
    headers: { authorization: `Bearer ${token}` },
  });

  assert.equal(res.statusCode, 403, "expect 403 PERMISSION_DENIED");

  await app.close();
});

test("PUT /agents/:id/soul — success evicts idle children bound to the edited agent (F28)", async () => {
  const { app, token, evictCalls } = await buildApp("ok", {
    setAgentSoul: async () => ({ soul: "You are terse." }),
  });
  await app.ready();

  const res = await app.inject({
    method: "PUT",
    url: "/agents/agent-1/soul",
    headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
    payload: { soul: "You are terse." },
  });

  assert.equal(res.statusCode, 200);
  assert.equal(evictCalls.length, 1, "evictIdleForAgent must be called exactly once");
  assert.equal(evictCalls[0]?.agentId, "agent-1");
  assert.match(evictCalls[0]?.reason ?? "", /soul/i);

  await app.close();
});

test("PUT /agents/:id/soul — eviction failure does not fail the PUT (best-effort)", async () => {
  const { privateKey, jwk } = await makeKey();
  const jwksResolver = await localResolver(jwk);
  const token = await mintToken(privateKey);

  const app = Fastify({ logger: false });
  const north = {
    listMyAgents: (): never => { throw new Error("not used in this test"); },
    getAgentSoul: (): never => { throw new Error("not used in this test"); },
    setAgentSoul: async () => ({ soul: "You are terse." }),
  };
  const throwingSupervisor = {
    evictIdleForAgent(): void {
      throw new Error("simulated eviction failure");
    },
  };

  registerAgentsRoutes(app, { clients: { north }, jwksResolver, verifyOpts: VERIFY_OPTS, supervisor: throwingSupervisor });
  await app.ready();

  const res = await app.inject({
    method: "PUT",
    url: "/agents/agent-1/soul",
    headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
    payload: { soul: "You are terse." },
  });

  assert.equal(res.statusCode, 200, "eviction failure must not fail the PUT");
  const body = JSON.parse(res.body) as { soul: string };
  assert.equal(body.soul, "You are terse.");

  await app.close();
});

test("GET /agents/:id/soul — error response body never leaks 'Error:' or a stack fragment", async () => {
  const { app, token } = await buildApp("not-found");
  await app.ready();

  const res = await app.inject({
    method: "GET",
    url: "/agents/nope-1/soul",
    headers: { authorization: `Bearer ${token}` },
  });

  assert.doesNotMatch(res.body, /Error:/);
  assert.doesNotMatch(res.body, /at .+:\d+:\d+/);

  await app.close();
});
