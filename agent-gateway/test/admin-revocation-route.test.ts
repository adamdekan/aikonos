// AG-06 admin routes: block / unblock a member and revoke all sessions of a
// member or an agent. Registers the real registerAdminRoutes against a fake
// north client that records what it received.
import { test } from "node:test";
import assert from "node:assert/strict";
import Fastify from "fastify";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import { registerAdminRoutes } from "../src/routes/admin.js";
import { BrokerClients } from "../src/broker/clients.js";
import { NorthClient } from "../src/broker/north.js";
import type { JwksResolver } from "../src/auth/verify.js";

const VERIFY_OPTS = { issuer: "http://localhost:18080/realms/aikonos", audience: "aikonos-broker" };

async function buildApp() {
  const { publicKey, privateKey } = await generateKeyPair("RS256");
  const jwk: JWK = { ...(await exportJWK(publicKey)), kid: "k1", alg: "RS256", use: "sig" };
  const key = await importJWK(jwk, "RS256");
  const jwksResolver: JwksResolver = () => Promise.resolve(key);
  const token = await new SignJWT({ sub: "admin-oid", email: "admin@example.com", tenant_id: "11111111-1111-1111-1111-111111111111" })
    .setProtectedHeader({ alg: "RS256", kid: "k1" })
    .setIssuer(VERIFY_OPTS.issuer)
    .setAudience(VERIFY_OPTS.audience)
    .setIssuedAt()
    .setExpirationTime("1h")
    .sign(privateKey);

  const calls: { method: string; req: Record<string, unknown>; token?: string }[] = [];
  const app = Fastify({ logger: false });
  const clients: BrokerClients = Object.create(BrokerClients.prototype);
  const north: NorthClient = Object.create(NorthClient.prototype);
  Object.assign(north, {
    setUserBlocked(req: Record<string, unknown>, t?: string) {
      calls.push({ method: "setUserBlocked", req, token: t });
      return Promise.resolve({ changed: true });
    },
    revokeUserSessions(req: Record<string, unknown>, t?: string) {
      calls.push({ method: "revokeUserSessions", req, token: t });
      return Promise.resolve({ notBefore: "2026-10-28T12:00:00Z" });
    },
    revokeAgentSessions(req: Record<string, unknown>, t?: string) {
      calls.push({ method: "revokeAgentSessions", req, token: t });
      return Promise.resolve({ notBefore: "2026-10-28T12:00:00Z" });
    },
    setAgentStatus(req: Record<string, unknown>, t?: string) {
      calls.push({ method: "setAgentStatus", req, token: t });
      return Promise.resolve({ agent: { id: req.agentId, name: "a", status: req.status, statusReason: req.reason } });
    },
  });
  clients.north = north;
  const evicted: [string, string, boolean | undefined][] = [];
  const supervisor = {
    evictIdleForAgent(agentId: string, reason: string, includeBusy?: boolean) {
      evicted.push([agentId, reason, includeBusy]);
    },
  };
  registerAdminRoutes(app, { clients, jwksResolver, verifyOpts: VERIFY_OPTS, supervisor });
  await app.ready();
  return { app, token, calls, evicted };
}

test("AG-06 revocation routes forward subject, reason and the caller token", async () => {
  const { app, token, calls } = await buildApp();
  const post = (url: string, payload: object) =>
    app.inject({ method: "POST", url, headers: { authorization: `Bearer ${token}` }, payload });

  let res = await post(`/admin/members/${encodeURIComponent("bob@example.com")}/block`, { blocked: true, reason: "left" });
  assert.equal(res.statusCode, 200);
  assert.deepEqual(JSON.parse(res.body), { changed: true });
  res = await post("/admin/members/bob/block", { blocked: false, reason: "back" });
  assert.equal(res.statusCode, 200);
  res = await post("/admin/members/bob/revoke-sessions", { reason: "stolen laptop" });
  assert.deepEqual(JSON.parse(res.body), { notBefore: "2026-10-28T12:00:00Z" });
  res = await post("/admin/agents/agent-1/revoke-sessions", { reason: "key leaked" });
  assert.equal(res.statusCode, 200);

  assert.deepEqual(
    calls.map((c) => [c.method, c.req.subject ?? c.req.agentId, c.req.blocked, c.req.reason, c.req.userId, c.token]),
    [
      ["setUserBlocked", "bob@example.com", true, "left", "admin-oid", token],
      ["setUserBlocked", "bob", false, "back", "admin-oid", token],
      ["revokeUserSessions", "bob", undefined, "stolen laptop", "admin-oid", token],
      ["revokeAgentSessions", "agent-1", undefined, "key leaked", "admin-oid", token],
    ],
  );
  await app.close();
});

test("AG-06 revocation routes refuse a caller without a bearer", async () => {
  const { app, calls } = await buildApp();
  const res = await app.inject({ method: "POST", url: "/admin/members/bob/block", payload: { reason: "x" } });
  assert.equal(res.statusCode, 401);
  assert.equal(calls.length, 0);
  await app.close();
});

test("AG-03 status route forwards status and reason; leaving active evicts busy children too", async () => {
  const { app, token, calls, evicted } = await buildApp();
  const post = (payload: object) =>
    app.inject({ method: "POST", url: "/admin/agents/agent-1/status", headers: { authorization: `Bearer ${token}` }, payload });

  let res = await post({ status: "suspended", reason: "incident" });
  assert.equal(res.statusCode, 200);
  assert.equal(JSON.parse(res.body).agent.status, "suspended");
  res = await post({ status: "active" });
  assert.equal(res.statusCode, 200);

  assert.deepEqual(
    calls.map((c) => [c.method, c.req.agentId, c.req.status, c.req.reason, c.req.userId, c.token]),
    [
      ["setAgentStatus", "agent-1", "suspended", "incident", "admin-oid", token],
      ["setAgentStatus", "agent-1", "active", "", "admin-oid", token],
    ],
  );
  assert.deepEqual(evicted, [["agent-1", "agent suspended", true]], "only the suspension evicts");
  await app.close();
});
