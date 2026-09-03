// Tests for the admin web.search engine config routes:
//   GET    /admin/websearch
//   PUT    /admin/websearch        body {engine, maxResults, apiKey?}
//   DELETE /admin/websearch
//   POST   /admin/websearch/test   body {engine?, maxResults?, apiKey?}
//
// Strategy: mirror admin-m365-route.test.ts exactly — inline the handler logic
// (matching src/routes/admin.ts's websearch block) with a fake req/reply + stub
// north client, no full Fastify server required.
import { test } from "node:test";
import assert from "node:assert/strict";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import { requireUser } from "../src/auth/require-user.js";
import { sendError } from "../src/http-errors.js";
import { log } from "../src/log.js";
import type { JwksResolver } from "../src/auth/verify.js";
import type {
  GetWebSearchConfigRequest,
  GetWebSearchConfigResponse,
  UpsertWebSearchConfigRequest,
  UpsertWebSearchConfigResponse,
  DeleteWebSearchConfigRequest,
  DeleteWebSearchConfigResponse,
  TestWebSearchConfigRequest,
  TestWebSearchConfigResponse,
  WebSearchConfig,
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

// ── webSearchJson (mirrors admin.ts's helper exactly) ────────────────────────

function webSearchJson(c: WebSearchConfig | undefined) {
  return {
    engine: c?.engine ?? "",
    maxResults: c?.maxResults ?? 0,
    hasKey: c?.hasKey ?? false,
    updatedBy: c?.updatedBy ?? "",
    updatedAt: c?.updatedAt ?? "",
  };
}

// ── Stub north client ─────────────────────────────────────────────────────────

type NorthCall =
  | { method: "getWebSearchConfig"; req: GetWebSearchConfigRequest; token: string | undefined }
  | { method: "upsertWebSearchConfig"; req: UpsertWebSearchConfigRequest; token: string | undefined }
  | { method: "deleteWebSearchConfig"; req: DeleteWebSearchConfigRequest; token: string | undefined }
  | { method: "testWebSearchConfig"; req: TestWebSearchConfigRequest; token: string | undefined };

function makeNorth(opts?: {
  rejectWith?: { code: number; message: string };
  getResponse?: GetWebSearchConfigResponse;
  upsertResponse?: UpsertWebSearchConfigResponse;
  testResponse?: TestWebSearchConfigResponse;
}) {
  const calls: NorthCall[] = [];

  return {
    calls,
    getWebSearchConfig(req: GetWebSearchConfigRequest, token?: string): Promise<GetWebSearchConfigResponse> {
      calls.push({ method: "getWebSearchConfig", req, token });
      if (opts?.rejectWith) return Promise.reject(opts.rejectWith);
      return Promise.resolve(opts?.getResponse ?? { config: undefined });
    },
    upsertWebSearchConfig(req: UpsertWebSearchConfigRequest, token?: string): Promise<UpsertWebSearchConfigResponse> {
      calls.push({ method: "upsertWebSearchConfig", req, token });
      if (opts?.rejectWith) return Promise.reject(opts.rejectWith);
      return Promise.resolve(
        opts?.upsertResponse ?? { config: { ...req.config!, hasKey: req.apiKey !== "" } },
      );
    },
    deleteWebSearchConfig(req: DeleteWebSearchConfigRequest, token?: string): Promise<DeleteWebSearchConfigResponse> {
      calls.push({ method: "deleteWebSearchConfig", req, token });
      if (opts?.rejectWith) return Promise.reject(opts.rejectWith);
      return Promise.resolve({});
    },
    testWebSearchConfig(req: TestWebSearchConfigRequest, token?: string): Promise<TestWebSearchConfigResponse> {
      calls.push({ method: "testWebSearchConfig", req, token });
      if (opts?.rejectWith) return Promise.reject(opts.rejectWith);
      return Promise.resolve(opts?.testResponse ?? { ok: true, detail: "probe succeeded" });
    },
  };
}

// ── Inline route handlers (mirror src/routes/admin.ts's websearch block exactly) ──

interface WebSearchBody { engine?: string; maxResults?: number; apiKey?: string }

async function handleGet(
  req: { headers: Record<string, string> },
  reply: FakeReply,
  jwksResolver: JwksResolver,
  north: ReturnType<typeof makeNorth>,
) {
  const principal = await requireUser(req, reply, jwksResolver, VERIFY_OPTS);
  if (!principal) return;
  try {
    const resp = await north.getWebSearchConfig({}, principal.token);
    reply.send({ config: webSearchJson(resp.config) });
  } catch (err) {
    sendError(reply, log, err, { route: "GET /admin/websearch" });
  }
}

async function handlePut(
  req: { headers: Record<string, string>; body?: WebSearchBody },
  reply: FakeReply,
  jwksResolver: JwksResolver,
  north: ReturnType<typeof makeNorth>,
) {
  const principal = await requireUser(req, reply, jwksResolver, VERIFY_OPTS);
  if (!principal) return;
  const b = req.body ?? {};
  try {
    const resp = await north.upsertWebSearchConfig(
      {
        config: {
          engine: b.engine ?? "",
          maxResults: b.maxResults ?? 0,
          hasKey: false,
          updatedBy: "",
          updatedAt: "",
        },
        apiKey: b.apiKey ?? "",
      },
      principal.token,
    );
    reply.send({ config: webSearchJson(resp.config) });
  } catch (err) {
    sendError(reply, log, err, { route: "PUT /admin/websearch" });
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
    await north.deleteWebSearchConfig({}, principal.token);
    reply.send({});
  } catch (err) {
    sendError(reply, log, err, { route: "DELETE /admin/websearch" });
  }
}

async function handleTest(
  req: { headers: Record<string, string>; body?: WebSearchBody },
  reply: FakeReply,
  jwksResolver: JwksResolver,
  north: ReturnType<typeof makeNorth>,
) {
  const principal = await requireUser(req, reply, jwksResolver, VERIFY_OPTS);
  if (!principal) return;
  const b = req.body ?? {};
  try {
    const resp = await north.testWebSearchConfig(
      {
        config: {
          engine: b.engine ?? "",
          maxResults: b.maxResults ?? 0,
          hasKey: false,
          updatedBy: "",
          updatedAt: "",
        },
        apiKey: b.apiKey ?? "",
      },
      principal.token,
    );
    reply.send({ ok: resp.ok, detail: resp.detail });
  } catch (err) {
    sendError(reply, log, err, { route: "POST /admin/websearch/test" });
  }
}

// ── 401-gate tests ────────────────────────────────────────────────────────────

test("GET /admin/websearch — no bearer → 401", async () => {
  const { jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const reply = fakeReply();

  await handleGet(fakeReq(), reply, resolver, north);

  assert.equal(reply.statusCode, 401);
  assert.equal(north.calls.length, 0);
});

test("PUT /admin/websearch — no bearer → 401", async () => {
  const { jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const reply = fakeReply();

  await handlePut(fakeReq(), reply, resolver, north);

  assert.equal(reply.statusCode, 401);
  assert.equal(north.calls.length, 0);
});

// ── Happy-path + forwarding tests ─────────────────────────────────────────────

test("GET /admin/websearch — valid bearer forwards {} + bearer, returns zero-value config when unconfigured", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth({ getResponse: { config: undefined } });
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleGet(fakeReq(`Bearer ${token}`), reply, resolver, north);

  assert.equal(north.calls.length, 1);
  const call = north.calls[0];
  assert.ok(call.method === "getWebSearchConfig");
  assert.deepEqual(call.req, {});
  assert.equal(call.token, token);
  assert.deepEqual(reply.body, {
    config: { engine: "", maxResults: 0, hasKey: false, updatedBy: "", updatedAt: "" },
  });
});

test("GET /admin/websearch — returns the configured has_key without the key itself", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth({
    getResponse: {
      config: {
        engine: "brave", maxResults: 10, hasKey: true,
        updatedBy: "admin@example.com", updatedAt: "2026-07-22T00:00:00Z",
      },
    },
  });
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleGet(fakeReq(`Bearer ${token}`), reply, resolver, north);

  const body = reply.body as { config: WebSearchConfig };
  assert.equal(body.config.hasKey, true);
  assert.equal(body.config.engine, "brave");
  // The key itself never appears anywhere on the wire response.
  assert.equal(JSON.stringify(body).toLowerCase().includes("apikey"), false);
});

test("PUT /admin/websearch — forwards engine/maxResults + apiKey from the body", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handlePut(
    fakeReq(`Bearer ${token}`, { engine: "brave", maxResults: 10, apiKey: "s3cr3t" }),
    reply,
    resolver,
    north,
  );

  assert.equal(north.calls.length, 1);
  const call = north.calls[0];
  assert.ok(call.method === "upsertWebSearchConfig");
  assert.deepEqual(call.req, {
    config: { engine: "brave", maxResults: 10, hasKey: false, updatedBy: "", updatedAt: "" },
    apiKey: "s3cr3t",
  });
});

test("PUT /admin/websearch — blank api_key on edit forwards apiKey: \"\" (broker preserves the stored key)", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  // No apiKey field at all in the body — the webui omits it when the admin
  // leaves the password input untouched on an edit.
  await handlePut(
    fakeReq(`Bearer ${token}`, { engine: "brave", maxResults: 10 }),
    reply,
    resolver,
    north,
  );

  const call = north.calls[0];
  assert.ok(call.method === "upsertWebSearchConfig");
  assert.equal(call.req.apiKey, "");
});

test("DELETE /admin/websearch — valid bearer calls deleteWebSearchConfig, returns 200 empty", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleDelete(fakeReq(`Bearer ${token}`), reply, resolver, north);

  assert.equal(north.calls.length, 1);
  assert.equal(north.calls[0].method, "deleteWebSearchConfig");
  assert.equal(reply.statusCode, 0);
  assert.deepEqual(reply.body, {});
});

test("POST /admin/websearch/test — ok result renders through verbatim", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth({ testResponse: { ok: true, detail: "probe succeeded" } });
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleTest(
    fakeReq(`Bearer ${token}`, { engine: "brave", maxResults: 10, apiKey: "s3cr3t" }),
    reply,
    resolver,
    north,
  );

  assert.deepEqual(reply.body, { ok: true, detail: "probe succeeded" });
});

test("POST /admin/websearch/test — failure detail passes through", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth({
    testResponse: { ok: false, detail: "brave API returned 401 unauthorized" },
  });
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleTest(fakeReq(`Bearer ${token}`, {}), reply, resolver, north);

  const body = reply.body as TestWebSearchConfigResponse;
  assert.equal(body.ok, false);
  assert.match(body.detail, /401/);
});

// ── Error mapping tests (real grpcToHttp, via sendError) ─────────────────────

test("GET /admin/websearch — PermissionDenied → 403 (non-admin caller)", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth({ rejectWith: { code: 7, message: "7 PERMISSION_DENIED: tenant admin required" } });
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleGet(fakeReq(`Bearer ${token}`), reply, resolver, north);

  assert.equal(reply.statusCode, 403);
});

test("PUT /admin/websearch — InvalidArgument (unknown engine) → 400", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth({
    rejectWith: { code: 3, message: "3 INVALID_ARGUMENT: unknown engine" },
  });
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handlePut(fakeReq(`Bearer ${token}`, { engine: "bing" }), reply, resolver, north);

  assert.equal(reply.statusCode, 400);
});

test("POST /admin/websearch/test — FailedPrecondition (not configured) → 409", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth({
    rejectWith: { code: 9, message: "9 FAILED_PRECONDITION: web.search not configured" },
  });
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleTest(fakeReq(`Bearer ${token}`, {}), reply, resolver, north);

  assert.equal(reply.statusCode, 409);
});
