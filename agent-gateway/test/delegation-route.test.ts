// POST /delegate input checks: no guessed recipient, no unrequested scope.
import { test } from "node:test";
import assert from "node:assert/strict";
import Fastify from "fastify";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";

import { registerDelegationRoutes } from "../src/routes/delegation.js";

const VERIFY_OPTS = { issuer: "http://localhost:18080/realms/aikonos", audience: "aikonos-broker" };

async function buildApp() {
  const { publicKey, privateKey } = await generateKeyPair("RS256");
  const key = await importJWK({ ...(await exportJWK(publicKey)), alg: "RS256" }, "RS256");
  const sent: Record<string, unknown>[] = [];
  const north = {
    sendEnvelope: (req: Record<string, unknown>) => { sent.push(req); return Promise.resolve({ envelopeId: "env-1" }); },
  };
  const app = Fastify({ logger: false });
  registerDelegationRoutes(app, {
    clients: { north: north as never },
    jwksResolver: () => Promise.resolve(key),
    verifyOpts: VERIFY_OPTS,
  });
  await app.ready();
  const token = await new SignJWT({ sub: "alice@example.com", email: "alice@example.com", tenant_id: "aikonos-dev" })
    .setProtectedHeader({ alg: "RS256", kid: "k1" })
    .setIssuer(VERIFY_OPTS.issuer).setAudience(VERIFY_OPTS.audience)
    .setIssuedAt().setExpirationTime("1h").sign(privateKey);
  const post = (payload: object) =>
    app.inject({ method: "POST", url: "/delegate", headers: { authorization: `Bearer ${token}` }, payload });
  return { app, sent, post };
}

test("POST /delegate without a recipient is refused, not sent to a default user", async () => {
  const { app, sent, post } = await buildApp();
  const res = await post({ intent: "x" });
  assert.equal(res.statusCode, 400);
  assert.equal(sent.length, 0);
  assert.equal((await post({ to: "bob@example.com", group: "g1", intent: "x" })).statusCode, 400, "both is ambiguous");
  await app.close();
});

test("POST /delegate defaults to no scopes and a 50-unit cost cap", async () => {
  const { app, sent, post } = await buildApp();
  const res = await post({ to: "bob@example.com", intent: "x" });
  assert.equal(res.statusCode, 200);
  const deleg = sent[0].delegation as { attenuatedScopes: string[]; maxCostUnits: number };
  assert.deepEqual(deleg.attenuatedScopes, []);
  assert.equal(deleg.maxCostUnits, 50);
  assert.deepEqual(sent[0].recipient, { userId: "bob@example.com" });
  await app.close();
});

test("POST /delegate rejects malformed scopes and cost", async () => {
  const { app, sent, post } = await buildApp();
  assert.equal((await post({ to: "bob@example.com", scopes: "siem:read" })).statusCode, 400);
  assert.equal((await post({ to: "bob@example.com", maxCost: -1 })).statusCode, 400);
  assert.equal((await post({ group: "g1", maxCost: "5" })).statusCode, 400);
  assert.equal(sent.length, 0);
  await app.close();
});
