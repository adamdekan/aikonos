// POST /approve/:id + GET /approvals route handlers.
//
// WHY: /approve used to answer any pending approval for whoever knew its id,
// and the id is the model's tool-call id, which shows in the run's stream and
// the saved session. Registers the real registerApprovalRoutes
// (src/routes/approvals.ts) against a real ApprovalRegistry, so a mutation to
// the production route fails these tests — no hand-copied handler.
import { test } from "node:test";
import assert from "node:assert/strict";
import Fastify from "fastify";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import { registerApprovalRoutes } from "../src/routes/approvals.js";
import { ApprovalRegistry } from "../src/agui/hitl.js";
import type { ApprovalInfo } from "../src/broker/governance.js";
import type { JwksResolver } from "../src/auth/verify.js";

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
  sub: string,
) {
  return new SignJWT({ sub, email: sub, tenant_id: "aikonos-dev" })
    .setProtectedHeader({ alg: "RS256", kid: "k1" })
    .setIssuer(VERIFY_OPTS.issuer)
    .setAudience(VERIFY_OPTS.audience)
    .setIssuedAt()
    .setExpirationTime("1h")
    .sign(privateKey);
}

function makeInfo(toolCallId: string): ApprovalInfo {
  return {
    toolCallId,
    toolName: "email.send",
    toolId: "email.send",
    effectClass: 0,
    reason: "test",
    args: {},
    stepUp: true,
  };
}

async function buildApp() {
  const { privateKey, jwk } = await makeKey();
  const jwksResolver = await localResolver(jwk);
  const approvals = new ApprovalRegistry();
  const app = Fastify({ logger: false });
  registerApprovalRoutes(app, { approvals, jwksResolver, verifyOpts: VERIFY_OPTS });
  await app.ready();
  return {
    app,
    approvals,
    alice: await mintToken(privateKey, "alice@example.com"),
    bob: await mintToken(privateKey, "bob@example.com"),
  };
}

function answer(app: Awaited<ReturnType<typeof buildApp>>["app"], id: string, approved: boolean, token?: string) {
  return app.inject({
    method: "POST",
    url: `/approve/${encodeURIComponent(id)}`,
    headers: token ? { authorization: `Bearer ${token}` } : {},
    payload: { approved },
  });
}

test("POST /approve/:id without a bearer is refused and the approval keeps waiting", async () => {
  const { app, approvals } = await buildApp();
  void approvals.await_(makeInfo("call-1"), "alice@example.com", "run-a");

  const res = await answer(app, "call-1", true);

  assert.equal(res.statusCode, 401);
  assert.equal(approvals.listForUser("alice@example.com").length, 1);
  approvals.drain();
  await app.close();
});

test("POST /approve/:id from another user resolves nothing and reads like an unknown id", async () => {
  const { app, approvals, bob } = await buildApp();
  void approvals.await_(makeInfo("call-1"), "alice@example.com", "run-a");

  const theirs = await answer(app, "call-1", true, bob);
  const unknown = await answer(app, "no-such-call", true, bob);

  assert.equal(theirs.statusCode, 200);
  assert.deepEqual(theirs.json(), { resolved: false });
  assert.equal(theirs.body, unknown.body, "the reply must not reveal that someone else's approval exists");
  assert.equal(approvals.listForUser("alice@example.com").length, 1, "Alice's approval must still wait for Alice");
  approvals.drain();
  await app.close();
});

test("POST /approve/:id from the user whose run asked delivers the decision", async () => {
  const { app, approvals, alice } = await buildApp();
  const approved = approvals.await_(makeInfo("call-1"), "alice@example.com", "run-a");
  const denied = approvals.await_(makeInfo("call-2"), "alice@example.com", "run-a");

  assert.deepEqual((await answer(app, "call-1", true, alice)).json(), { resolved: true });
  assert.deepEqual((await answer(app, "call-2", false, alice)).json(), { resolved: true });

  assert.equal(await approved, true);
  assert.equal(await denied, false);
  assert.deepEqual((await answer(app, "call-1", true, alice)).json(), { resolved: false }, "an answered approval is gone");
  await app.close();
});

test("GET /approvals lists only the caller's own approvals", async () => {
  const { app, approvals, alice, bob } = await buildApp();
  void approvals.await_(makeInfo("call-1"), "alice@example.com", "run-a");

  const list = async (token: string) =>
    (await app.inject({ method: "GET", url: "/approvals", headers: { authorization: `Bearer ${token}` } })).json();

  assert.deepEqual((await list(alice)).approvals.map((a: ApprovalInfo) => a.toolCallId), ["call-1"]);
  assert.deepEqual((await list(bob)).approvals, []);
  assert.equal((await app.inject({ method: "GET", url: "/approvals" })).statusCode, 401);
  approvals.drain();
  await app.close();
});
