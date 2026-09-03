// Tests for the LLM-provider catalog admin routes:
//   GET  /admin/providers                     → `defaults` map passes through
//   POST /admin/providers/:id/default-for     body {capability, clear?}
//   POST /admin/providers/test                body {..., mode?}
//
// Registers the real registerAdminRoutes (src/routes/admin.ts) against a fake
// north client that records the requests it received, so a mutation to the
// production handler fails this test — no hand-copied handler logic.
import { test } from "node:test";
import assert from "node:assert/strict";
import Fastify from "fastify";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import { registerAdminRoutes } from "../src/routes/admin.js";
import { BrokerClients } from "../src/broker/clients.js";
import { NorthClient } from "../src/broker/north.js";
import type { JwksResolver } from "../src/auth/verify.js";
import type {
  ListLlmProvidersResponse,
  SetDefaultProviderForRequest,
  SetDefaultProviderForResponse,
  TestLlmProviderRequest,
  TestLlmProviderResponse,
} from "../gen/ts/proto/broker.js";

const INVALID_ARGUMENT_CODE = 3;

const VERIFY_OPTS = {
  issuer: "http://localhost:18080/realms/aikonos",
  audience: "aikonos-broker",
};

async function makeKey() {
  const { publicKey, privateKey } = await generateKeyPair("RS256");
  const jwk: JWK = { ...(await exportJWK(publicKey)), kid: "k1", alg: "RS256", use: "sig" };
  return { privateKey, jwk };
}

async function localResolver(jwk: JWK): Promise<JwksResolver> {
  const key = await importJWK(jwk, "RS256");
  return () => Promise.resolve(key);
}

async function mintToken(privateKey: Awaited<ReturnType<typeof generateKeyPair>>["privateKey"]) {
  return new SignJWT({
    sub: "user-uuid-admin",
    email: "admin@example.com",
    tenant_id: "11111111-1111-1111-1111-111111111111",
  })
    .setProtectedHeader({ alg: "RS256", kid: "k1" })
    .setIssuer(VERIFY_OPTS.issuer)
    .setAudience(VERIFY_OPTS.audience)
    .setIssuedAt()
    .setExpirationTime("1h")
    .sign(privateKey);
}

interface Recorded {
  defaultFor: { req: SetDefaultProviderForRequest; token: string | undefined }[];
  test: { req: TestLlmProviderRequest; token: string | undefined }[];
}

// Object.create(NorthClient.prototype) + Object.assign avoids the real
// constructor (which dials a gRPC channel) while satisfying the nominal class
// type check without a cast — mirrors workflow-delete-route.test.ts.
async function buildApp(opts: { defaultForRejects?: boolean } = {}) {
  const { privateKey, jwk } = await makeKey();
  const jwksResolver = await localResolver(jwk);
  const token = await mintToken(privateKey);

  const recorded: Recorded = { defaultFor: [], test: [] };

  const app = Fastify({ logger: false });
  const clients: BrokerClients = Object.create(BrokerClients.prototype);
  const north: NorthClient = Object.create(NorthClient.prototype);
  Object.assign(north, {
    listLlmProviders(): Promise<ListLlmProvidersResponse> {
      return Promise.resolve({ providers: [], defaults: { chat: "openai-prod", embedding: "voyage" } });
    },
    setDefaultProviderFor(
      req: SetDefaultProviderForRequest,
      t?: string,
    ): Promise<SetDefaultProviderForResponse> {
      recorded.defaultFor.push({ req, token: t });
      if (opts.defaultForRejects) {
        throw Object.assign(new Error("3 INVALID_ARGUMENT: unknown capability"), {
          code: INVALID_ARGUMENT_CODE,
          details: "unknown capability",
        });
      }
      return Promise.resolve({});
    },
    testLlmProvider(req: TestLlmProviderRequest, t?: string): Promise<TestLlmProviderResponse> {
      recorded.test.push({ req, token: t });
      return Promise.resolve({ ok: true, statusCode: 200, error: "", latencyMs: 42 });
    },
  });
  clients.north = north;

  registerAdminRoutes(app, { clients, jwksResolver, verifyOpts: VERIFY_OPTS });
  await app.ready();

  return { app, token, recorded };
}

// ── GET /admin/providers ─────────────────────────────────────────────────────

test("GET /admin/providers — defaults map passes through untouched", async () => {
  // WHY: the Defaults panel seeds every capability select from this map; a route
  // that hand-picks only `providers` silently blanks the panel.
  const { app, token } = await buildApp();

  const res = await app.inject({
    method: "GET",
    url: "/admin/providers",
    headers: { authorization: `Bearer ${token}` },
  });

  assert.equal(res.statusCode, 200);
  const body = JSON.parse(res.body) as { providers: unknown[]; defaults: Record<string, string> };
  assert.deepEqual(body.defaults, { chat: "openai-prod", embedding: "voyage" });

  await app.close();
});

// ── POST /admin/providers/:id/default-for ────────────────────────────────────

test("POST /admin/providers/:id/default-for — forwards capability + path id as providerId", async () => {
  const { app, token, recorded } = await buildApp();

  const res = await app.inject({
    method: "POST",
    url: "/admin/providers/openai-prod/default-for",
    headers: { authorization: `Bearer ${token}` },
    payload: { capability: "embedding" },
  });

  assert.equal(res.statusCode, 200);
  assert.equal(recorded.defaultFor.length, 1);
  assert.deepEqual(recorded.defaultFor[0].req, { capability: "embedding", providerId: "openai-prod" });
  assert.equal(recorded.defaultFor[0].token, token, "bearer forwarded, never minted");

  await app.close();
});

test("POST /admin/providers/:id/default-for — clear:true sends an empty provider id", async () => {
  // WHY: an empty provider id is how the broker deletes the capability's row;
  // sending the path id would re-assign the default the operator just cleared.
  const { app, token, recorded } = await buildApp();

  const res = await app.inject({
    method: "POST",
    url: "/admin/providers/openai-prod/default-for",
    headers: { authorization: `Bearer ${token}` },
    payload: { capability: "vision", clear: true },
  });

  assert.equal(res.statusCode, 200);
  assert.deepEqual(recorded.defaultFor[0].req, { capability: "vision", providerId: "" });

  await app.close();
});

test("POST /admin/providers/:id/default-for — no bearer → 401, broker never called", async () => {
  const { app, recorded } = await buildApp();

  const res = await app.inject({
    method: "POST",
    url: "/admin/providers/openai-prod/default-for",
    payload: { capability: "chat" },
  });

  assert.equal(res.statusCode, 401);
  assert.equal(recorded.defaultFor.length, 0);

  await app.close();
});

test("POST /admin/providers/:id/default-for — capability validation is the broker's; InvalidArgument → 400", async () => {
  // WHY: the provider routes are deliberately thin proxies — the broker owns the
  // capability vocabulary. A missing capability is forwarded as "" and comes back
  // as InvalidArgument, which grpcToHttp maps to 400.
  const { app, token, recorded } = await buildApp({ defaultForRejects: true });

  const res = await app.inject({
    method: "POST",
    url: "/admin/providers/openai-prod/default-for",
    headers: { authorization: `Bearer ${token}` },
    payload: {},
  });

  assert.equal(res.statusCode, 400);
  assert.equal(recorded.defaultFor[0].req.capability, "", "no gateway-side default capability");
  const body = JSON.parse(res.body) as { error: string };
  assert.equal(body.error, "unknown capability", "broker detail surfaces, not String(err)");

  await app.close();
});

// ── POST /admin/providers/test ───────────────────────────────────────────────

test("POST /admin/providers/test — forwards the probing mode", async () => {
  const { app, token, recorded } = await buildApp();

  const res = await app.inject({
    method: "POST",
    url: "/admin/providers/test",
    headers: { authorization: `Bearer ${token}` },
    payload: { apiKey: "k1", mode: "embedding" },
  });

  assert.equal(res.statusCode, 200);
  assert.equal(recorded.test[0].req.mode, "embedding");

  await app.close();
});

test("POST /admin/providers/test — omitted mode forwards \"\" (broker picks the first model's mode)", async () => {
  const { app, token, recorded } = await buildApp();

  const res = await app.inject({
    method: "POST",
    url: "/admin/providers/test",
    headers: { authorization: `Bearer ${token}` },
    payload: { apiKey: "k1" },
  });

  assert.equal(res.statusCode, 200);
  assert.equal(recorded.test[0].req.mode, "");

  await app.close();
});
