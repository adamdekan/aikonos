// Tests for the admin agent API-key routes:
//   POST   /admin/agents/:id/keys       → mint
//   GET    /admin/agents/:id/keys       → list
//   DELETE /admin/agents/:id/keys/:keyId → revoke
//
// Strategy: mirror the require-user.test.ts pattern — fake req/reply objects
// let us test the requireUser 401-gate and north-method dispatch without
// standing up the full Fastify server (app is not exported from server.ts).
//
// The external-surface test builds the `:8090` Fastify via buildExternalApp and
// asserts the admin routes are NOT registered there (404).
import { test } from "node:test";
import assert from "node:assert/strict";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import { requireUser } from "../src/auth/require-user.js";
import type { JwksResolver } from "../src/auth/verify.js";
import type {
  MintAgentApiKeyRequest,
  ListAgentApiKeysRequest,
  RevokeAgentApiKeyRequest,
  MintAgentApiKeyResponse,
  ListAgentApiKeysResponse,
  RevokeAgentApiKeyResponse,
  AgentApiKey,
} from "../gen/ts/proto/broker.js";
import { buildExternalApp } from "../src/external/server.js";
import type { Config } from "../src/config.js";
import { BrokerClients } from "../src/broker/clients.js";
import pino from "pino";

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

function fakeReq(authHeader?: string): { headers: Record<string, string> } {
  const headers: Record<string, string> = {};
  if (authHeader) headers.authorization = authHeader;
  return { headers };
}

interface MintBody { rawKey: string; key: { id: string; keyPrefix: string; label: string } | null }
interface ListBody { keys: { id: string; keyPrefix: string; label: string }[] }
interface RevokeBody { success: boolean }

type ReplyBody = MintBody | ListBody | RevokeBody | Record<string, unknown> | undefined;

interface FakeReply {
  statusCode: number;
  body: ReplyBody;
  code(n: number): FakeReply;
  send(b: ReplyBody): void;
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

function isMintBody(b: ReplyBody): b is MintBody {
  return b != null && typeof b === "object" && "rawKey" in b;
}
function isListBody(b: ReplyBody): b is ListBody {
  return b != null && typeof b === "object" && "keys" in b;
}
function isRevokeBody(b: ReplyBody): b is RevokeBody {
  return b != null && typeof b === "object" && "success" in b;
}

function asMintBody(b: ReplyBody): MintBody {
  assert.ok(isMintBody(b), "expected MintBody (rawKey)");
  return b;
}
function asListBody(b: ReplyBody): ListBody {
  assert.ok(isListBody(b), "expected ListBody (keys)");
  return b;
}
function asRevokeBody(b: ReplyBody): RevokeBody {
  assert.ok(isRevokeBody(b), "expected RevokeBody (success)");
  return b;
}

// ── Stub north client ─────────────────────────────────────────────────────────

type NorthCall =
  | { method: "mintAgentApiKey"; req: MintAgentApiKeyRequest; token: string | undefined }
  | { method: "listAgentApiKeys"; req: ListAgentApiKeysRequest; token: string | undefined }
  | { method: "revokeAgentApiKey"; req: RevokeAgentApiKeyRequest; token: string | undefined };

function makeNorth() {
  const calls: NorthCall[] = [];

  const fakeKey: AgentApiKey = {
    id: "key-uuid-1",
    keyPrefix: "tk_live_xx",
    label: "ci-key",
    createdBy: "admin@example.com",
    createdAt: undefined,
    lastUsedAt: undefined,
  };

  return {
    calls,
    mintAgentApiKey(req: MintAgentApiKeyRequest, token?: string): Promise<MintAgentApiKeyResponse> {
      calls.push({ method: "mintAgentApiKey", req, token });
      return Promise.resolve({ key: fakeKey, rawKey: "tk_live_xxSECRET" });
    },
    listAgentApiKeys(req: ListAgentApiKeysRequest, token?: string): Promise<ListAgentApiKeysResponse> {
      calls.push({ method: "listAgentApiKeys", req, token });
      return Promise.resolve({ keys: [fakeKey] });
    },
    revokeAgentApiKey(req: RevokeAgentApiKeyRequest, token?: string): Promise<RevokeAgentApiKeyResponse> {
      calls.push({ method: "revokeAgentApiKey", req, token });
      return Promise.resolve({ success: true });
    },
  };
}

// ── Inline route handlers (mirror server.ts logic exactly) ───────────────────
//
// We can't import server.ts (it calls loadConfig + BrokerClients at module
// scope, which requires real env / TLS files). Instead we inline the handler
// logic and test the exact same code shape that server.ts registers.
//
// This gives us: 401-gate proof + north-dispatch proof, with no duplicated
// business logic beyond the few lines that are the handler.

async function handleMint(
  req: { headers: Record<string, string> },
  reply: FakeReply,
  jwksResolver: JwksResolver,
  north: ReturnType<typeof makeNorth>,
  agentId: string,
  label: string | undefined,
) {
  const principal = await requireUser(req, reply, jwksResolver, VERIFY_OPTS);
  if (!principal) return;
  try {
    const mintReq: MintAgentApiKeyRequest = { tenantId: principal.tenant, userId: principal.sub, agentId, label: label ?? "" };
    const resp = await north.mintAgentApiKey(mintReq, principal.token);
    reply.send({
      rawKey: resp.rawKey,
      key: resp.key ? { id: resp.key.id, keyPrefix: resp.key.keyPrefix, label: resp.key.label } : null,
    });
  } catch (err) {
    reply.code(500).send({ error: String(err) });
  }
}

async function handleList(
  req: { headers: Record<string, string> },
  reply: FakeReply,
  jwksResolver: JwksResolver,
  north: ReturnType<typeof makeNorth>,
  agentId: string,
) {
  const principal = await requireUser(req, reply, jwksResolver, VERIFY_OPTS);
  if (!principal) return;
  try {
    const listReq: ListAgentApiKeysRequest = { tenantId: principal.tenant, userId: principal.sub, agentId };
    const resp = await north.listAgentApiKeys(listReq, principal.token);
    reply.send({
      keys: (resp.keys ?? []).map((k) => ({ id: k.id, keyPrefix: k.keyPrefix, label: k.label })),
    });
  } catch (err) {
    reply.code(500).send({ error: String(err) });
  }
}

async function handleRevoke(
  req: { headers: Record<string, string> },
  reply: FakeReply,
  jwksResolver: JwksResolver,
  north: ReturnType<typeof makeNorth>,
  agentId: string,
  keyId: string,
) {
  const principal = await requireUser(req, reply, jwksResolver, VERIFY_OPTS);
  if (!principal) return;
  try {
    const revokeReq: RevokeAgentApiKeyRequest = { tenantId: principal.tenant, userId: principal.sub, agentId, keyId };
    const resp = await north.revokeAgentApiKey(revokeReq, principal.token);
    reply.send({ success: resp.success });
  } catch (err) {
    reply.code(500).send({ success: false, error: String(err) });
  }
}

// ── 401-gate tests ────────────────────────────────────────────────────────────

test("POST /admin/agents/:id/keys — no bearer → 401", async () => {
  const { jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const reply = fakeReply();

  await handleMint(fakeReq(), reply, resolver, north, "agent-1", undefined);

  assert.equal(reply.statusCode, 401);
  assert.equal(north.calls.length, 0);
});

test("GET /admin/agents/:id/keys — no bearer → 401", async () => {
  const { jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const reply = fakeReply();

  await handleList(fakeReq(), reply, resolver, north, "agent-1");

  assert.equal(reply.statusCode, 401);
  assert.equal(north.calls.length, 0);
});

test("DELETE /admin/agents/:id/keys/:keyId — no bearer → 401", async () => {
  const { jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const reply = fakeReply();

  await handleRevoke(fakeReq(), reply, resolver, north, "agent-1", "key-1");

  assert.equal(reply.statusCode, 401);
  assert.equal(north.calls.length, 0);
});

// ── north dispatch + bearer forwarding tests ──────────────────────────────────

test("POST /admin/agents/:id/keys — valid bearer calls mintAgentApiKey with forwarded token", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleMint(fakeReq(`Bearer ${token}`), reply, resolver, north, "agent-uuid-1", "ci-key");

  assert.equal(north.calls.length, 1);
  const call = north.calls[0];
  assert.equal(call.method, "mintAgentApiKey");
  assert.equal(call.token, token);
  assert.ok(call.method === "mintAgentApiKey");
  assert.equal(call.req.tenantId, "11111111-1111-1111-1111-111111111111");
  assert.equal(call.req.userId, "user-uuid-admin");
  assert.equal(call.req.agentId, "agent-uuid-1");
  assert.equal(call.req.label, "ci-key");

  const body = asMintBody(reply.body);
  assert.equal(body.rawKey, "tk_live_xxSECRET");
  assert.ok(body.key !== null);
  assert.equal(body.key?.id, "key-uuid-1");
  assert.equal(body.key?.keyPrefix, "tk_live_xx");
});

test("GET /admin/agents/:id/keys — valid bearer calls listAgentApiKeys with forwarded token", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleList(fakeReq(`Bearer ${token}`), reply, resolver, north, "agent-uuid-2");

  assert.equal(north.calls.length, 1);
  const call = north.calls[0];
  assert.equal(call.method, "listAgentApiKeys");
  assert.equal(call.token, token);
  assert.ok(call.method === "listAgentApiKeys");
  assert.equal(call.req.tenantId, "11111111-1111-1111-1111-111111111111");
  assert.equal(call.req.userId, "user-uuid-admin");
  assert.equal(call.req.agentId, "agent-uuid-2");

  const body = asListBody(reply.body);
  assert.equal(body.keys.length, 1);
  assert.equal(body.keys[0].id, "key-uuid-1");
  assert.equal(body.keys[0].label, "ci-key");
});

// ── principal.sub forwarding: userId must be sub, not email ──────────────────
// Spec (oid-authz-principal.md §agent-gateway/src/server.ts): every north call
// forwards principal.sub as userId. The broker mismatch guard 403s if it sees
// the email when the broker principal is the oid.

test("POST /admin/agents/:id/keys — userId forwarded is principal.sub, not email", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleMint(fakeReq(`Bearer ${token}`), reply, resolver, north, "agent-uuid-sub-check", "sub-check");

  assert.equal(north.calls.length, 1);
  const call = north.calls[0];
  assert.ok(call.method === "mintAgentApiKey");
  // sub from token payload ("user-uuid-admin"), not the email ("admin@example.com")
  assert.equal(call.req.userId, "user-uuid-admin");
  assert.notEqual(call.req.userId, "admin@example.com");
});

// ── External surface separation tests ────────────────────────────────────────
//
// The external `:8090` Fastify never mounts OIDC or admin routes.
// A regression that accidentally wires /admin routes there must fail here.

test("external surface: POST /admin/agents/:id/keys → 404 (route absent)", async () => {
  const cfg: Config = {
    openrouterApiKey: "",
    llmModel: "",
    brokerNorthAddr: "",
    brokerSouthAddr: "",
    brokerServerName: "",
    tlsCert: "",
    tlsKey: "",
    tlsCa: "",
    gatewaySpiffeId: "",
    port: 8080,
    defaultTenantId: "",
    oidcIssuer: "",
    oidcJwksUrl: "",
    oidcAudience: "",
    oidcSubjectClaim: "sub",
    oidcTenantClaim: "tenant_id",
    schedulerEnabled: false,
    schedulerTickMs: 30000,
    schedulerClaimLimit: 10,
    schedulerRunTimeoutMs: 180000,
    agentForUserOverrides: {},
    externalPort: 8090,
    externalCorsOrigins: [],
    externalRateLimit: 60,
    threadTtlMs: 1800000,
    maxChildren: 32,
    childTtlMs: 1800000,
    natsUrl: "nats://nats:4222",
    auditSubject: "aikonos.audit.>",
    egressTimeoutMs: 120000, brokerTimeoutMs: 30000, rateLimitBreakerThreshold: 5, workflowReasonMaxTokens: 2048, maxLlmCallsPerRun: 100, approvalTimeoutMs: 900000, memorySemanticRecall: true, memoryEmbedTimeoutMs: 10000, subagentMaxWidth: 3, subagentBranchTimeoutMs: 180000,
  };
  // BrokerClients is not used by buildExternalApp during route registration,
  // only inside route handlers that won't fire for these 404 probes.
  // Object.create avoids calling the constructor (which opens real TLS connections)
  // while satisfying the class type check without a cast.
  const clients: BrokerClients = Object.create(BrokerClients.prototype);
  const silentLog = pino({ level: "silent" });
  const app = buildExternalApp(cfg, clients, silentLog, {} as unknown as import("../src/ipc/supervisor.js").ChildSupervisor);
  await app.ready();

  const post = await app.inject({ method: "POST", url: "/admin/agents/agent-x/keys" });
  assert.equal(post.statusCode, 404, "POST /admin/agents/:id/keys must not be registered on external surface");

  const get = await app.inject({ method: "GET", url: "/admin/agents/agent-x/keys" });
  assert.equal(get.statusCode, 404, "GET /admin/agents/:id/keys must not be registered on external surface");

  const del = await app.inject({ method: "DELETE", url: "/admin/agents/agent-x/keys/key-1" });
  assert.equal(del.statusCode, 404, "DELETE /admin/agents/:id/keys/:keyId must not be registered on external surface");

  await app.close();
});

test("DELETE /admin/agents/:id/keys/:keyId — valid bearer calls revokeAgentApiKey with forwarded token", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleRevoke(fakeReq(`Bearer ${token}`), reply, resolver, north, "agent-uuid-3", "key-uuid-99");

  assert.equal(north.calls.length, 1);
  const call = north.calls[0];
  assert.equal(call.method, "revokeAgentApiKey");
  assert.equal(call.token, token);
  assert.ok(call.method === "revokeAgentApiKey");
  assert.equal(call.req.tenantId, "11111111-1111-1111-1111-111111111111");
  assert.equal(call.req.userId, "user-uuid-admin");
  assert.equal(call.req.agentId, "agent-uuid-3");
  assert.equal(call.req.keyId, "key-uuid-99");

  const body = asRevokeBody(reply.body);
  assert.equal(body.success, true);
});
