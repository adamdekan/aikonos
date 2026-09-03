// Tests for the admin M365 tenant connection routes:
//   GET    /admin/m365
//   PUT    /admin/m365        body {entraTenantId, clientId, clientSecret?, enabled}
//   DELETE /admin/m365
//   POST   /admin/m365/test   body {entraTenantId?, clientId?, clientSecret?, enabled?}
//
// Strategy: mirror admin-provisioning.test.ts/admin-skills.test.ts — inline the
// handler logic (matching src/routes/admin.ts's M365 block exactly) with a fake
// req/reply + stub north client, no full Fastify server required (AdminCtx.clients
// is the concrete BrokerClients class, which cannot be faked without a real
// gRPC channel). Unlike the older provisioning/skills tests, this uses the real
// sendError/grpcToHttp mapper — production's actual error-mapping path (F27) —
// instead of a stale local adminErrorCode reimplementation.
import { test } from "node:test";
import assert from "node:assert/strict";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import { requireUser } from "../src/auth/require-user.js";
import { sendError } from "../src/http-errors.js";
import { log } from "../src/log.js";
import type { JwksResolver } from "../src/auth/verify.js";
import type {
  GetM365ConnectionRequest,
  GetM365ConnectionResponse,
  UpsertM365ConnectionRequest,
  UpsertM365ConnectionResponse,
  DeleteM365ConnectionRequest,
  DeleteM365ConnectionResponse,
  TestM365ConnectionRequest,
  TestM365ConnectionResponse,
  M365Connection,
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

async function mintToken(
  privateKey: Awaited<ReturnType<typeof generateKeyPair>>["privateKey"],
) {
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

// ── m365Json (mirrors admin.ts's helper exactly) ─────────────────────────────

function m365Json(c: M365Connection | undefined) {
  return {
    entraTenantId: c?.entraTenantId ?? "",
    clientId: c?.clientId ?? "",
    hasSecret: c?.hasSecret ?? false,
    enabled: c?.enabled ?? false,
    updatedBy: c?.updatedBy ?? "",
    updatedAt: c?.updatedAt ?? "",
  };
}

// ── Stub north client ─────────────────────────────────────────────────────────

type NorthCall =
  | { method: "getM365Connection"; req: GetM365ConnectionRequest; token: string | undefined }
  | { method: "upsertM365Connection"; req: UpsertM365ConnectionRequest; token: string | undefined }
  | { method: "deleteM365Connection"; req: DeleteM365ConnectionRequest; token: string | undefined }
  | { method: "testM365Connection"; req: TestM365ConnectionRequest; token: string | undefined };

function makeNorth(opts?: {
  rejectWith?: { code: number; message: string };
  getResponse?: GetM365ConnectionResponse;
  upsertResponse?: UpsertM365ConnectionResponse;
  testResponse?: TestM365ConnectionResponse;
}) {
  const calls: NorthCall[] = [];

  return {
    calls,
    getM365Connection(req: GetM365ConnectionRequest, token?: string): Promise<GetM365ConnectionResponse> {
      calls.push({ method: "getM365Connection", req, token });
      if (opts?.rejectWith) return Promise.reject(opts.rejectWith);
      return Promise.resolve(opts?.getResponse ?? { connection: undefined });
    },
    upsertM365Connection(req: UpsertM365ConnectionRequest, token?: string): Promise<UpsertM365ConnectionResponse> {
      calls.push({ method: "upsertM365Connection", req, token });
      if (opts?.rejectWith) return Promise.reject(opts.rejectWith);
      return Promise.resolve(
        opts?.upsertResponse ?? { connection: { ...req.connection!, hasSecret: req.clientSecret !== "" } },
      );
    },
    deleteM365Connection(req: DeleteM365ConnectionRequest, token?: string): Promise<DeleteM365ConnectionResponse> {
      calls.push({ method: "deleteM365Connection", req, token });
      if (opts?.rejectWith) return Promise.reject(opts.rejectWith);
      return Promise.resolve({});
    },
    testM365Connection(req: TestM365ConnectionRequest, token?: string): Promise<TestM365ConnectionResponse> {
      calls.push({ method: "testM365Connection", req, token });
      if (opts?.rejectWith) return Promise.reject(opts.rejectWith);
      return Promise.resolve(opts?.testResponse ?? { ok: true, detail: "OBO exchange succeeded" });
    },
  };
}

// ── Inline route handlers (mirror src/routes/admin.ts's M365 block exactly) ──

interface M365Body { entraTenantId?: string; clientId?: string; clientSecret?: string; enabled?: boolean }

async function handleGet(
  req: { headers: Record<string, string> },
  reply: FakeReply,
  jwksResolver: JwksResolver,
  north: ReturnType<typeof makeNorth>,
) {
  const principal = await requireUser(req, reply, jwksResolver, VERIFY_OPTS);
  if (!principal) return;
  try {
    const resp = await north.getM365Connection({}, principal.token);
    reply.send({ connection: m365Json(resp.connection) });
  } catch (err) {
    sendError(reply, log, err, { route: "GET /admin/m365" });
  }
}

async function handlePut(
  req: { headers: Record<string, string>; body?: M365Body },
  reply: FakeReply,
  jwksResolver: JwksResolver,
  north: ReturnType<typeof makeNorth>,
) {
  const principal = await requireUser(req, reply, jwksResolver, VERIFY_OPTS);
  if (!principal) return;
  const b = req.body ?? {};
  try {
    const resp = await north.upsertM365Connection(
      {
        connection: {
          entraTenantId: b.entraTenantId ?? "",
          clientId: b.clientId ?? "",
          hasSecret: false,
          enabled: b.enabled ?? false,
          updatedBy: "",
          updatedAt: "",
        },
        clientSecret: b.clientSecret ?? "",
      },
      principal.token,
    );
    reply.send({ connection: m365Json(resp.connection) });
  } catch (err) {
    sendError(reply, log, err, { route: "PUT /admin/m365" });
  }
}

async function handleDelete(
  req: { headers: Record<string, string> },
  reply: FakeReply,
  jwksResolver: JwksResolver,
  north: ReturnType<typeof makeNorth>,
) {
  const principal = await requireUser(req, reply, jwksResolver, VERIFY_OPTS);
  if (!principal) return;
  try {
    await north.deleteM365Connection({}, principal.token);
    reply.send({});
  } catch (err) {
    sendError(reply, log, err, { route: "DELETE /admin/m365" });
  }
}

async function handleTest(
  req: { headers: Record<string, string>; body?: M365Body },
  reply: FakeReply,
  jwksResolver: JwksResolver,
  north: ReturnType<typeof makeNorth>,
) {
  const principal = await requireUser(req, reply, jwksResolver, VERIFY_OPTS);
  if (!principal) return;
  const b = req.body ?? {};
  try {
    const resp = await north.testM365Connection(
      {
        connection: {
          entraTenantId: b.entraTenantId ?? "",
          clientId: b.clientId ?? "",
          hasSecret: false,
          enabled: b.enabled ?? false,
          updatedBy: "",
          updatedAt: "",
        },
        clientSecret: b.clientSecret ?? "",
      },
      principal.token,
    );
    reply.send({ ok: resp.ok, detail: resp.detail });
  } catch (err) {
    sendError(reply, log, err, { route: "POST /admin/m365/test" });
  }
}

// ── 401-gate tests ────────────────────────────────────────────────────────────

test("GET /admin/m365 — no bearer → 401", async () => {
  const { jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const reply = fakeReply();

  await handleGet(fakeReq(), reply, resolver, north);

  assert.equal(reply.statusCode, 401);
  assert.equal(north.calls.length, 0);
});

test("PUT /admin/m365 — no bearer → 401", async () => {
  const { jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const reply = fakeReply();

  await handlePut(fakeReq(), reply, resolver, north);

  assert.equal(reply.statusCode, 401);
  assert.equal(north.calls.length, 0);
});

// ── Happy-path + forwarding tests ─────────────────────────────────────────────

test("GET /admin/m365 — valid bearer forwards {} + bearer, returns zero-value connection when unconfigured", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth({ getResponse: { connection: undefined } });
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleGet(fakeReq(`Bearer ${token}`), reply, resolver, north);

  assert.equal(north.calls.length, 1);
  const call = north.calls[0];
  assert.ok(call.method === "getM365Connection");
  assert.deepEqual(call.req, {});
  assert.equal(call.token, token);
  assert.deepEqual(reply.body, {
    connection: { entraTenantId: "", clientId: "", hasSecret: false, enabled: false, updatedBy: "", updatedAt: "" },
  });
});

test("GET /admin/m365 — returns the configured connection's has_secret without the secret itself", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth({
    getResponse: {
      connection: {
        entraTenantId: "tenant-1", clientId: "client-1", hasSecret: true,
        enabled: true, updatedBy: "admin@example.com", updatedAt: "2026-07-10T00:00:00Z",
      },
    },
  });
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleGet(fakeReq(`Bearer ${token}`), reply, resolver, north);

  const body = reply.body as { connection: M365Connection };
  assert.equal(body.connection.hasSecret, true);
  assert.equal(body.connection.entraTenantId, "tenant-1");
  // The secret itself never appears anywhere on the wire response.
  assert.equal(JSON.stringify(body).includes("clientSecret"), false);
});

test("PUT /admin/m365 — forwards entraTenantId/clientId/enabled + clientSecret from the body", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handlePut(
    fakeReq(`Bearer ${token}`, { entraTenantId: "tenant-1", clientId: "client-1", clientSecret: "s3cr3t", enabled: true }),
    reply,
    resolver,
    north,
  );

  assert.equal(north.calls.length, 1);
  const call = north.calls[0];
  assert.ok(call.method === "upsertM365Connection");
  assert.deepEqual(call.req, {
    connection: {
      entraTenantId: "tenant-1", clientId: "client-1", hasSecret: false,
      enabled: true, updatedBy: "", updatedAt: "",
    },
    clientSecret: "s3cr3t",
  });
});

test("PUT /admin/m365 — blank client_secret on edit forwards clientSecret: \"\" (broker preserves the stored secret)", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  // No clientSecret field at all in the body — the webui omits it when the
  // admin leaves the password input untouched on an edit.
  await handlePut(
    fakeReq(`Bearer ${token}`, { entraTenantId: "tenant-1", clientId: "client-1", enabled: true }),
    reply,
    resolver,
    north,
  );

  const call = north.calls[0];
  assert.ok(call.method === "upsertM365Connection");
  assert.equal(call.req.clientSecret, "");
});

test("DELETE /admin/m365 — valid bearer calls deleteM365Connection, returns 200 empty", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleDelete(fakeReq(`Bearer ${token}`), reply, resolver, north);

  assert.equal(north.calls.length, 1);
  assert.equal(north.calls[0].method, "deleteM365Connection");
  assert.equal(reply.statusCode, 0);
  assert.deepEqual(reply.body, {});
});

test("POST /admin/m365/test — ok result renders through verbatim", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth({ testResponse: { ok: true, detail: "OBO exchange succeeded" } });
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleTest(
    fakeReq(`Bearer ${token}`, { entraTenantId: "tenant-1", clientId: "client-1", clientSecret: "s3cr3t" }),
    reply,
    resolver,
    north,
  );

  assert.deepEqual(reply.body, { ok: true, detail: "OBO exchange succeeded" });
});

test("POST /admin/m365/test — classified failure detail passes through", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth({
    testResponse: { ok: false, detail: "admin consent required (AADSTS65001) — grant consent for Files.ReadWrite in the Entra portal" },
  });
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleTest(fakeReq(`Bearer ${token}`, {}), reply, resolver, north);

  const body = reply.body as TestM365ConnectionResponse;
  assert.equal(body.ok, false);
  assert.match(body.detail, /AADSTS65001/);
});

// ── Error mapping tests (real grpcToHttp, via sendError) ─────────────────────

test("GET /admin/m365 — PermissionDenied → 403 (non-admin caller)", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth({ rejectWith: { code: 7, message: "7 PERMISSION_DENIED: tenant admin required" } });
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleGet(fakeReq(`Bearer ${token}`), reply, resolver, north);

  assert.equal(reply.statusCode, 403);
});

test("PUT /admin/m365 — InvalidArgument (missing tenant/client id) → 400", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth({
    rejectWith: { code: 3, message: "3 INVALID_ARGUMENT: connection.entra_tenant_id and connection.client_id are required" },
  });
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handlePut(fakeReq(`Bearer ${token}`, {}), reply, resolver, north);

  assert.equal(reply.statusCode, 400);
});

test("POST /admin/m365/test — FailedPrecondition (not configured) → 409", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth({
    rejectWith: { code: 9, message: "9 FAILED_PRECONDITION: m365 connection not configured: entra_tenant_id, client_id, and a client secret are all required" },
  });
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleTest(fakeReq(`Bearer ${token}`, {}), reply, resolver, north);

  assert.equal(reply.statusCode, 409);
});
