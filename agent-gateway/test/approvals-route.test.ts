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
    stepUp: true, argsDigest: `digest-${toolCallId}`, requiresN: 1, approvals: 0, ownerMayApprove: true,
  };
}

// Records broker calls; the decide route must forward the caller's own token.
class FakeNorth {
  calls: { method: string; req: Record<string, unknown>; token?: string }[] = [];
  listPendingApprovals(req: object, token?: string) {
    this.calls.push({ method: "list", req: { ...req }, token });
    return Promise.resolve({ approvals: [{ taskId: "task-1", argsDigest: "d1", steps: [] }] } as never);
  }
  approveTask(req: object, token?: string) {
    this.calls.push({ method: "approve", req: { ...req }, token });
    return Promise.resolve({ success: true, newStatus: 4, capabilityTokenIds: { 1: "secret-cap" } } as never);
  }
  cancelTask(req: object, token?: string) {
    this.calls.push({ method: "cancel", req: { ...req }, token });
    return Promise.resolve({ success: true } as never);
  }
}

async function buildApp() {
  const { privateKey, jwk } = await makeKey();
  const jwksResolver = await localResolver(jwk);
  const approvals = new ApprovalRegistry();
  const north = new FakeNorth();
  const app = Fastify({ logger: false });
  registerApprovalRoutes(app, { approvals, clients: { north }, jwksResolver, verifyOpts: VERIFY_OPTS });
  await app.ready();
  return {
    app,
    approvals,
    north,
    alice: await mintToken(privateKey, "alice@example.com"),
    bob: await mintToken(privateKey, "bob@example.com"),
  };
}

function answer(
  app: Awaited<ReturnType<typeof buildApp>>["app"],
  id: string,
  approved: boolean,
  token?: string,
  argsDigest = `digest-${id}`,
) {
  return app.inject({
    method: "POST",
    url: `/approve/${encodeURIComponent(id)}`,
    headers: token ? { authorization: `Bearer ${token}` } : {},
    payload: { approved, argsDigest },
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

// AG-07 (M-28): an approval must echo the digest of the arguments it was shown.
test("POST /approve/:id with a missing or different args digest is refused and keeps waiting", async () => {
  const { app, approvals, alice } = await buildApp();
  const decision = approvals.await_(makeInfo("call-1"), "alice@example.com", "run-a");

  const wrong = await answer(app, "call-1", true, alice, "digest-of-other-args");
  const missing = await app.inject({
    method: "POST",
    url: "/approve/call-1",
    headers: { authorization: `Bearer ${alice}` },
    payload: { approved: true },
  });

  assert.equal(wrong.statusCode, 409);
  assert.equal(missing.statusCode, 409);
  assert.equal(approvals.listForUser("alice@example.com").length, 1, "the approval must still wait");
  assert.deepEqual((await answer(app, "call-1", false, alice, "")).json(), { resolved: true }, "a denial needs no digest");
  assert.equal(await decision, false);
  await app.close();
});

test("GET /pending-approvals and POST /pending-approvals/:taskId act as the caller and keep tokens server-side", async () => {
  const { app, north, bob } = await buildApp();
  const auth = { authorization: `Bearer ${bob}` };

  const list = await app.inject({ method: "GET", url: "/pending-approvals", headers: auth });
  assert.equal(list.statusCode, 200);
  assert.equal(list.json().approvals[0].taskId, "task-1");

  const decide = await app.inject({
    method: "POST",
    url: "/pending-approvals/task-1",
    headers: auth,
    payload: { approved: true, argsDigest: "d1" },
  });
  assert.equal(decide.statusCode, 200);
  assert.deepEqual(decide.json(), { newStatus: 4 }, "minted capability tokens must not reach the browser");

  const approve = north.calls.find((c) => c.method === "approve")!;
  assert.equal(approve.token, bob);
  assert.equal(approve.req.approverUserId, "bob@example.com");
  assert.equal(approve.req.argsDigest, "d1");
  assert.equal(approve.req.taskId, "task-1");
  assert.equal(north.calls.find((c) => c.method === "list")!.token, bob);

  assert.equal((await app.inject({ method: "GET", url: "/pending-approvals" })).statusCode, 401);
  await app.close();
});

// The requester withdraws their own pending request: the broker's CancelTask
// runs as the caller (it allows only the task owner or an admin), and a
// refusal reaches the browser as 403.
test("POST /pending-approvals/:taskId/withdraw cancels the task as the caller", async () => {
  const { app, north, alice } = await buildApp();
  const auth = { authorization: `Bearer ${alice}` };
  const res = await app.inject({ method: "POST", url: "/pending-approvals/task-1/withdraw", headers: auth });
  assert.equal(res.statusCode, 200);
  assert.deepEqual(res.json(), { withdrawn: true });
  const cancel = north.calls.find((c) => c.method === "cancel")!;
  assert.equal(cancel.token, alice);
  assert.equal(cancel.req.taskId, "task-1");
  assert.equal(cancel.req.tenantId, "aikonos-dev");

  const { status } = await import("@grpc/grpc-js");
  north.cancelTask = () =>
    Promise.reject(Object.assign(new Error("7 PERMISSION_DENIED: only the task owner or a tenant admin may cancel this task"), {
      code: status.PERMISSION_DENIED, details: "only the task owner or a tenant admin may cancel this task",
    }));
  const denied = await app.inject({ method: "POST", url: "/pending-approvals/task-1/withdraw", headers: auth });
  assert.equal(denied.statusCode, 403);
  assert.equal((await app.inject({ method: "POST", url: "/pending-approvals/task-1/withdraw" })).statusCode, 401);
  await app.close();
});

// AG-13: the broker's step_up_required refusal reaches the browser as a 401
// naming the tenant's acr, so the web UI signs in again with MFA and retries.
test("POST /pending-approvals/:taskId maps step_up_required to 401 with the acr", async () => {
  const { app, north, bob } = await buildApp();
  const { Metadata, status } = await import("@grpc/grpc-js");
  const md = new Metadata();
  md.set("aikonos-step-up-acr", "mfa");
  north.approveTask = () =>
    Promise.reject(Object.assign(new Error("7 PERMISSION_DENIED: step_up_required"), {
      code: status.PERMISSION_DENIED, details: "step_up_required", metadata: md,
    }));
  const res = await app.inject({
    method: "POST",
    url: "/pending-approvals/task-1",
    headers: { authorization: `Bearer ${bob}` },
    payload: { approved: true, argsDigest: "d1" },
  });
  assert.equal(res.statusCode, 401);
  assert.deepEqual(res.json(), { error: "step_up_required", acr: "mfa" });
  await app.close();
});

test("GET /approvals?threadId= lists only that chat's approvals, not the same user's other tabs", async () => {
  const { app, approvals, alice } = await buildApp();
  void approvals.await_({ ...makeInfo("call-1"), threadId: "thread-a" }, "alice@example.com", "run-a");
  void approvals.await_({ ...makeInfo("call-2"), threadId: "thread-b" }, "alice@example.com", "run-b");

  const one = await app.inject({ method: "GET", url: "/approvals?threadId=thread-a", headers: { authorization: `Bearer ${alice}` } });
  assert.deepEqual(one.json().approvals.map((a: ApprovalInfo) => a.toolCallId), ["call-1"]);
  const all = await app.inject({ method: "GET", url: "/approvals", headers: { authorization: `Bearer ${alice}` } });
  assert.equal(all.json().approvals.length, 2);
  approvals.drain();
  await app.close();
});
