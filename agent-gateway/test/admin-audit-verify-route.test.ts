// GET /admin/audit/verify — the chain-verification verdict.
//
// Registers the real registerAdminRoutes against a fake north client, so a
// change to the production route fails these tests.
import { test } from "node:test";
import assert from "node:assert/strict";
import Fastify from "fastify";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import { registerAdminRoutes } from "../src/routes/admin.js";
import { BrokerClients } from "../src/broker/clients.js";
import { NorthClient } from "../src/broker/north.js";
import type { JwksResolver } from "../src/auth/verify.js";
import type { VerifyAuditChainResponse } from "../gen/ts/proto/broker.js";

const VERIFY_OPTS = {
  issuer: "http://localhost:18080/realms/aikonos",
  audience: "aikonos-broker",
};

const BASE: VerifyAuditChainResponse = {
  total: 7, ok: true, signed: true, storeConfigured: true,
  headEventId: "h", tailEventId: "t", breaks: [], signatureFailures: [],
  truncated: false, skipped: 0,
};

async function verify(resp: Partial<VerifyAuditChainResponse>) {
  const { publicKey, privateKey } = await generateKeyPair("RS256");
  const jwk: JWK = { ...(await exportJWK(publicKey)), kid: "k1", alg: "RS256", use: "sig" };
  const key = await importJWK(jwk, "RS256");
  const jwksResolver: JwksResolver = () => Promise.resolve(key);
  const token = await new SignJWT({ sub: "admin@example.com", email: "admin@example.com", tenant_id: "11111111-1111-1111-1111-111111111111" })
    .setProtectedHeader({ alg: "RS256", kid: "k1" })
    .setIssuer(VERIFY_OPTS.issuer)
    .setAudience(VERIFY_OPTS.audience)
    .setIssuedAt()
    .setExpirationTime("1h")
    .sign(privateKey);

  const clients: BrokerClients = Object.create(BrokerClients.prototype);
  const north: NorthClient = Object.create(NorthClient.prototype);
  Object.assign(north, {
    async verifyAuditChain(): Promise<VerifyAuditChainResponse> {
      return { ...BASE, ...resp };
    },
  });
  clients.north = north;

  const app = Fastify({ logger: false });
  registerAdminRoutes(app, { clients, jwksResolver, verifyOpts: VERIFY_OPTS });
  await app.ready();
  const res = await app.inject({ method: "GET", url: "/admin/audit/verify", headers: { authorization: `Bearer ${token}` } });
  assert.equal(res.statusCode, 200);
  return res.json();
}

test("a fully verified chain is intact", async () => {
  const body = await verify({});
  assert.equal(body.verdict, "intact");
  assert.equal(body.ok, true);
});

test("a truncated walk is incomplete, never intact, even when the broker says ok", async () => {
  const body = await verify({ ok: true, truncated: true });
  assert.equal(body.verdict, "incomplete");
  assert.equal(body.ok, false);
  assert.equal(body.truncated, true);
});

test("skipped objects make the verdict incomplete and are forwarded", async () => {
  const body = await verify({ ok: true, skipped: 3 });
  assert.equal(body.verdict, "incomplete");
  assert.equal(body.ok, false);
  assert.equal(body.skipped, 3);
});

test("a break wins over incompleteness", async () => {
  const body = await verify({ ok: false, skipped: 1, breaks: [{ eventId: "e1", detail: "fork" }] });
  assert.equal(body.verdict, "broken");
  assert.equal(body.ok, false);
});
