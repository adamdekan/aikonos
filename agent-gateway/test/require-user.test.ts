// Tests for requireUser — extracts and verifies the bearer from an incoming
// HTTP request. Returns the principal on success; on failure the handler writes
// 401 and the caller bails.
import { test } from "node:test";
import assert from "node:assert/strict";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import { requireUser } from "../src/auth/require-user.js";
import type { JwksResolver } from "../src/auth/verify.js";

// ── Helpers ───────────────────────────────────────────────────────────────────

async function makeKey() {
  const { publicKey, privateKey } = await generateKeyPair("RS256");
  const jwk: JWK = { ...(await exportJWK(publicKey)), kid: "k1", alg: "RS256", use: "sig" };
  return { privateKey, jwk };
}

async function localResolver(jwk: JWK): Promise<JwksResolver> {
  const key = await importJWK(jwk, "RS256");
  return () => Promise.resolve(key);
}

async function mintToken(
  privateKey: Awaited<ReturnType<typeof generateKeyPair>>["privateKey"],
) {
  return new SignJWT({
    sub: "user-uuid-alice",
    email: "alice@example.com",
    tenant_id: "11111111-1111-1111-1111-111111111111",
  })
    .setProtectedHeader({ alg: "RS256", kid: "k1" })
    .setIssuer("http://localhost:18080/realms/aikonos")
    .setAudience("aikonos-broker")
    .setIssuedAt()
    .setExpirationTime("1h")
    .sign(privateKey);
}

// Minimal fake request / reply objects (just enough surface requireUser needs).
function fakeReq(authHeader?: string) {
  return {
    headers: authHeader ? { authorization: authHeader } : {},
  };
}

interface FakeReply {
  statusCode: number;
  body: unknown;
  code(n: number): FakeReply;
  send(b: unknown): void;
}

function fakeReply(): FakeReply {
  const r: FakeReply = {
    statusCode: 0,
    body: undefined,
    code(n) { r.statusCode = n; return r; },
    send(b) { r.body = b; },
  };
  return r;
}

const OPT = {
  issuer: "http://localhost:18080/realms/aikonos",
  audience: "aikonos-broker",
};

// ── Tests ─────────────────────────────────────────────────────────────────────

test("requireUser: missing Authorization header → 401", async () => {
  const { jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const req = fakeReq();
  const reply = fakeReply();

  const result = await requireUser(req, reply, resolver, OPT);

  assert.equal(result, null);
  assert.equal(reply.statusCode, 401);
});

test("requireUser: non-Bearer Authorization header → 401", async () => {
  const { jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const req = fakeReq("Basic dXNlcjpwYXNz");
  const reply = fakeReply();

  const result = await requireUser(req, reply, resolver, OPT);

  assert.equal(result, null);
  assert.equal(reply.statusCode, 401);
});

test("requireUser: invalid/garbage bearer → 401", async () => {
  const { jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const req = fakeReq("Bearer not-a-real-jwt");
  const reply = fakeReply();

  const result = await requireUser(req, reply, resolver, OPT);

  assert.equal(result, null);
  assert.equal(reply.statusCode, 401);
});

test("requireUser: valid bearer → returns principal with sub/email/tenant", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const token = await mintToken(privateKey);
  const req = fakeReq(`Bearer ${token}`);
  const reply = fakeReply();

  const result = await requireUser(req, reply, resolver, OPT);

  assert.notEqual(result, null);
  assert.equal(result!.sub, "user-uuid-alice");
  assert.equal(result!.email, "alice@example.com");
  assert.equal(result!.tenant, "11111111-1111-1111-1111-111111111111");
  assert.equal(result!.token, token);
  // reply was NOT touched (no 401)
  assert.equal(reply.statusCode, 0);
});

test("requireUser: acting user is token sub — header fields are irrelevant", async () => {
  // Even if an attacker passes x-aikonos-user, the principal must come from the token.
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const token = await mintToken(privateKey);
  const req = {
    headers: {
      authorization: `Bearer ${token}`,
      "x-aikonos-user": "mallory@example.com", // should be ignored
    },
  };
  const reply = fakeReply();

  const result = await requireUser(req, reply, resolver, OPT);

  assert.notEqual(result, null);
  assert.equal(result!.email, "alice@example.com"); // from token, not header
});
