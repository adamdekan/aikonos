// GET /admin/audit/evidence/:eventId — the decision-evidence download.
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
import type { GetDecisionEvidenceRequest, GetDecisionEvidenceResponse } from "../gen/ts/proto/broker.js";

const VERIFY_OPTS = {
  issuer: "http://localhost:18080/realms/aikonos",
  audience: "aikonos-broker",
};

const EVENT_ID = "0199a4f0-3c2e-7d41-9b6a-1f2e3d4c5b6a";

async function buildApp(response: () => Promise<GetDecisionEvidenceResponse>) {
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

  const calls: GetDecisionEvidenceRequest[] = [];
  const clients: BrokerClients = Object.create(BrokerClients.prototype);
  const north: NorthClient = Object.create(NorthClient.prototype);
  Object.assign(north, {
    async getDecisionEvidence(req: GetDecisionEvidenceRequest): Promise<GetDecisionEvidenceResponse> {
      calls.push(req);
      return response();
    },
  });
  clients.north = north;

  const app = Fastify({ logger: false });
  registerAdminRoutes(app, { clients, jwksResolver, verifyOpts: VERIFY_OPTS });
  await app.ready();
  return { app, token, calls };
}

test("evidence download passes the broker's document through unchanged, as an attachment", async () => {
  const doc = '{\n  "kind": "aikonos.decision-evidence/v1",\n  "event": {"event_id": "x"}\n}';
  const { app, token, calls } = await buildApp(async () => ({ evidenceJson: new TextEncoder().encode(doc) }));

  const res = await app.inject({ method: "GET", url: `/admin/audit/evidence/${EVENT_ID}`, headers: { authorization: `Bearer ${token}` } });
  assert.equal(res.statusCode, 200);
  assert.equal(res.body, doc);
  assert.match(String(res.headers["content-type"]), /^application\/json/);
  assert.equal(res.headers["content-disposition"], `attachment; filename="aikonos-evidence-${EVENT_ID}.json"`);
  assert.equal(res.headers["cache-control"], "no-store");
  assert.deepEqual(calls, [{ eventId: EVENT_ID }]);
});

test("evidence download rejects an event id that is not a lowercase UUID, without calling the broker", async () => {
  const { app, token, calls } = await buildApp(async () => ({ evidenceJson: new Uint8Array(0) }));
  for (const bad of ["not-a-uuid", EVENT_ID.toUpperCase(), "..%2F..%2Fetc"]) {
    const res = await app.inject({ method: "GET", url: `/admin/audit/evidence/${bad}`, headers: { authorization: `Bearer ${token}` } });
    assert.equal(res.statusCode, 400, bad);
  }
  assert.equal(calls.length, 0);
});

test("evidence download requires a bearer token", async () => {
  const { app, calls } = await buildApp(async () => ({ evidenceJson: new Uint8Array(0) }));
  const res = await app.inject({ method: "GET", url: `/admin/audit/evidence/${EVENT_ID}` });
  assert.equal(res.statusCode, 401);
  assert.equal(calls.length, 0);
});

test("a broker refusal maps to its HTTP status", async () => {
  const denied = Object.assign(new Error("tenant admin required"), { code: 7 }); // gRPC PERMISSION_DENIED
  const { app, token } = await buildApp(async () => {
    throw denied;
  });
  const res = await app.inject({ method: "GET", url: `/admin/audit/evidence/${EVENT_ID}`, headers: { authorization: `Bearer ${token}` } });
  assert.equal(res.statusCode, 403);
});
