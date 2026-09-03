// Tests for the admin spend-cap routes:
//   GET    /admin/spend-caps           list org/user/agent caps
//   GET    /admin/spend-caps/summary   current-period spend + caps dashboard
//   POST   /admin/spend-caps           body {scope, subjectId, capMicros}
//   DELETE /admin/spend-caps/:id
//
// Strategy: mirror admin-m365-route.test.ts — inline the handler logic
// (matching src/routes/admin.ts's spend-caps block exactly) with a fake
// req/reply + stub north client, using the real sendError/grpcToHttp mapper.
import { test } from "node:test";
import assert from "node:assert/strict";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import { requireUser } from "../src/auth/require-user.js";
import { sendError } from "../src/http-errors.js";
import { log } from "../src/log.js";
import type { JwksResolver } from "../src/auth/verify.js";
import type {
  ListSpendCapsRequest,
  ListSpendCapsResponse,
  SetSpendCapRequest,
  SetSpendCapResponse,
  DeleteSpendCapRequest,
  DeleteSpendCapResponse,
  GetSpendSummaryRequest,
  GetSpendSummaryResponse,
} from "../gen/ts/proto/broker.js";

// ── Shared key / token helpers ────────────────────────────────────────────────

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
    .setIssuer("http://localhost:18080/realms/aikonos")
    .setAudience("aikonos-broker")
    .setIssuedAt()
    .setExpirationTime("1h")
    .sign(privateKey);
}

const VERIFY_OPTS = {
  issuer: "http://localhost:18080/realms/aikonos",
  audience: "aikonos-broker",
};

// ── Minimal fake req / reply ──────────────────────────────────────────────────

function fakeReq<B>(authHeader?: string, body?: B): { headers: Record<string, string>; body?: B } {
  const headers: Record<string, string> = {};
  if (authHeader) headers.authorization = authHeader;
  return { headers, body };
}

interface FakeReply {
  statusCode: number;
  body: unknown;
  code(n: number): FakeReply;
  status(n: number): FakeReply;
  send(b?: unknown): void;
}

function fakeReply(): FakeReply {
  const r: FakeReply = {
    statusCode: 0,
    body: undefined,
    code(n) { r.statusCode = n; return r; },
    status(n) { r.statusCode = n; return r; },
    send(b) { r.body = b; },
  };
  return r;
}

// ── Stub north client ─────────────────────────────────────────────────────────

type NorthCall =
  | { method: "listSpendCaps"; req: ListSpendCapsRequest; token: string | undefined }
  | { method: "setSpendCap"; req: SetSpendCapRequest; token: string | undefined }
  | { method: "deleteSpendCap"; req: DeleteSpendCapRequest; token: string | undefined }
  | { method: "getSpendSummary"; req: GetSpendSummaryRequest; token: string | undefined };

function makeNorth(opts?: {
  rejectWith?: { code: number; message: string };
  listResponse?: ListSpendCapsResponse;
  setResponse?: SetSpendCapResponse;
  summaryResponse?: GetSpendSummaryResponse;
}) {
  const calls: NorthCall[] = [];

  return {
    calls,
    listSpendCaps(req: ListSpendCapsRequest, token?: string): Promise<ListSpendCapsResponse> {
      calls.push({ method: "listSpendCaps", req, token });
      if (opts?.rejectWith) return Promise.reject(opts.rejectWith);
      return Promise.resolve(opts?.listResponse ?? { caps: [] });
    },
    setSpendCap(req: SetSpendCapRequest, token?: string): Promise<SetSpendCapResponse> {
      calls.push({ method: "setSpendCap", req, token });
      if (opts?.rejectWith) return Promise.reject(opts.rejectWith);
      return Promise.resolve(opts?.setResponse ?? { id: "cap-1" });
    },
    deleteSpendCap(req: DeleteSpendCapRequest, token?: string): Promise<DeleteSpendCapResponse> {
      calls.push({ method: "deleteSpendCap", req, token });
      if (opts?.rejectWith) return Promise.reject(opts.rejectWith);
      return Promise.resolve({});
    },
    getSpendSummary(req: GetSpendSummaryRequest, token?: string): Promise<GetSpendSummaryResponse> {
      calls.push({ method: "getSpendSummary", req, token });
      if (opts?.rejectWith) return Promise.reject(opts.rejectWith);
      return Promise.resolve(
        opts?.summaryResponse ?? { orgSpendMicros: 0, orgCapMicros: 0, users: [], agents: [] },
      );
    },
  };
}

// ── Inline route handlers (mirror src/routes/admin.ts's spend-caps block) ────

interface SetCapBody { scope?: string; subjectId?: string; capMicros?: number }

async function handleList(
  req: { headers: Record<string, string> },
  reply: FakeReply,
  jwksResolver: JwksResolver,
  north: ReturnType<typeof makeNorth>,
) {
  const principal = await requireUser(req, reply, jwksResolver, VERIFY_OPTS);
  if (!principal) return;
  try {
    const resp = await north.listSpendCaps({ tenantId: principal.tenant }, principal.token);
    reply.send({ caps: resp.caps ?? [] });
  } catch (err) {
    sendError(reply, log, err, { route: "GET /admin/spend-caps" });
  }
}

async function handleSummary(
  req: { headers: Record<string, string> },
  reply: FakeReply,
  jwksResolver: JwksResolver,
  north: ReturnType<typeof makeNorth>,
) {
  const principal = await requireUser(req, reply, jwksResolver, VERIFY_OPTS);
  if (!principal) return;
  try {
    const resp = await north.getSpendSummary({ tenantId: principal.tenant }, principal.token);
    reply.send({
      orgSpendMicros: resp.orgSpendMicros ?? 0,
      orgCapMicros: resp.orgCapMicros ?? 0,
      users: resp.users ?? [],
      agents: resp.agents ?? [],
    });
  } catch (err) {
    sendError(reply, log, err, { route: "GET /admin/spend-caps/summary" });
  }
}

async function handleSet(
  req: { headers: Record<string, string>; body?: SetCapBody },
  reply: FakeReply,
  jwksResolver: JwksResolver,
  north: ReturnType<typeof makeNorth>,
) {
  const principal = await requireUser(req, reply, jwksResolver, VERIFY_OPTS);
  if (!principal) return;
  const b = req.body ?? {};
  try {
    const resp = await north.setSpendCap(
      {
        tenantId: principal.tenant,
        scope: b.scope ?? "",
        subjectId: b.subjectId ?? "",
        capMicros: b.capMicros ?? 0,
      },
      principal.token,
    );
    reply.send({ id: resp.id });
  } catch (err) {
    sendError(reply, log, err, { route: "POST /admin/spend-caps" });
  }
}

async function handleDelete(
  req: { headers: Record<string, string>; params: { id: string } },
  reply: FakeReply,
  jwksResolver: JwksResolver,
  north: ReturnType<typeof makeNorth>,
) {
  const principal = await requireUser(req, reply, jwksResolver, VERIFY_OPTS);
  if (!principal) return;
  try {
    await north.deleteSpendCap({ tenantId: principal.tenant, id: req.params.id }, principal.token);
    reply.status(204).send();
  } catch (err) {
    sendError(reply, log, err, { route: "DELETE /admin/spend-caps/:id" });
  }
}

// ── 401-gate tests ────────────────────────────────────────────────────────────

test("GET /admin/spend-caps — no bearer → 401", async () => {
  const { jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const reply = fakeReply();

  await handleList(fakeReq(), reply, resolver, north);

  assert.equal(reply.statusCode, 401);
  assert.equal(north.calls.length, 0);
});

test("POST /admin/spend-caps — no bearer → 401", async () => {
  const { jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const reply = fakeReply();

  await handleSet(fakeReq(), reply, resolver, north);

  assert.equal(reply.statusCode, 401);
  assert.equal(north.calls.length, 0);
});

// ── Happy-path + forwarding tests ─────────────────────────────────────────────

test("GET /admin/spend-caps — valid bearer forwards {tenantId} + bearer, returns caps", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const cap = { id: "cap-1", tenantId: "t1", scope: "org", subjectId: "", capMicros: 50_000_000, createdBy: "admin@example.com" };
  const north = makeNorth({ listResponse: { caps: [cap] } });
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleList(fakeReq(`Bearer ${token}`), reply, resolver, north);

  assert.equal(north.calls.length, 1);
  const call = north.calls[0];
  assert.ok(call.method === "listSpendCaps");
  assert.deepEqual(call.req, { tenantId: "11111111-1111-1111-1111-111111111111" });
  assert.equal(call.token, token);
  assert.deepEqual(reply.body, { caps: [cap] });
});

test("GET /admin/spend-caps/summary — forwards {tenantId}, returns org/user/agent rows", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const summary = {
    orgSpendMicros: 12_000_000,
    orgCapMicros: 100_000_000,
    users: [{ userId: "alice@example.com", spendMicros: 5_000_000, capMicros: 20_000_000 }],
    agents: [{ agentId: "agent-1", spendMicros: 7_000_000, capMicros: 0 }],
  };
  const north = makeNorth({ summaryResponse: summary });
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleSummary(fakeReq(`Bearer ${token}`), reply, resolver, north);

  assert.equal(north.calls.length, 1);
  assert.equal(north.calls[0].method, "getSpendSummary");
  assert.deepEqual(reply.body, summary);
});

test("POST /admin/spend-caps — forwards scope/subjectId/capMicros from the body", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleSet(
    fakeReq(`Bearer ${token}`, { scope: "user", subjectId: "alice@example.com", capMicros: 25_000_000 }),
    reply,
    resolver,
    north,
  );

  assert.equal(north.calls.length, 1);
  const call = north.calls[0];
  assert.ok(call.method === "setSpendCap");
  assert.deepEqual(call.req, {
    tenantId: "11111111-1111-1111-1111-111111111111",
    scope: "user",
    subjectId: "alice@example.com",
    capMicros: 25_000_000,
  });
  assert.deepEqual(reply.body, { id: "cap-1" });
});

test("POST /admin/spend-caps — org scope defaults subjectId to empty string", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleSet(fakeReq(`Bearer ${token}`, { scope: "org", capMicros: 100_000_000 }), reply, resolver, north);

  const call = north.calls[0];
  assert.ok(call.method === "setSpendCap");
  assert.equal(call.req.subjectId, "");
});

test("DELETE /admin/spend-caps/:id — valid bearer calls deleteSpendCap, returns 204", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleDelete({ ...fakeReq(`Bearer ${token}`), params: { id: "cap-1" } }, reply, resolver, north);

  assert.equal(north.calls.length, 1);
  const call = north.calls[0];
  assert.ok(call.method === "deleteSpendCap");
  assert.deepEqual(call.req, { tenantId: "11111111-1111-1111-1111-111111111111", id: "cap-1" });
  assert.equal(reply.statusCode, 204);
});

// ── Error mapping tests (real grpcToHttp, via sendError) ─────────────────────

test("POST /admin/spend-caps — PermissionDenied (non-admin caller) → 403", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth({ rejectWith: { code: 7, message: "7 PERMISSION_DENIED: tenant admin required" } });
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleSet(fakeReq(`Bearer ${token}`, { scope: "org", capMicros: 1 }), reply, resolver, north);

  assert.equal(reply.statusCode, 403);
});

test("POST /admin/spend-caps — InvalidArgument (non-positive capMicros) → 400", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth({ rejectWith: { code: 3, message: "3 INVALID_ARGUMENT: cap_micros must be a positive integer" } });
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleSet(fakeReq(`Bearer ${token}`, { scope: "org", capMicros: 0 }), reply, resolver, north);

  assert.equal(reply.statusCode, 400);
});

test("DELETE /admin/spend-caps/:id — NotFound (unknown id) → 404", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth({ rejectWith: { code: 5, message: "5 NOT_FOUND: spend cap not found" } });
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleDelete({ ...fakeReq(`Bearer ${token}`), params: { id: "missing" } }, reply, resolver, north);

  assert.equal(reply.statusCode, 404);
});
