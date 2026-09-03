// Tests for the admin provisioning-rule routes:
//   GET    /admin/provisioning
//   POST   /admin/provisioning        body {matcher, groups}
//   POST   /admin/provisioning/delete body {id}
//
// Strategy: mirror admin-keys.test.ts — inline the handler logic with a fake
// req/reply + stub north client, no full Fastify server required.
import { test } from "node:test";
import assert from "node:assert/strict";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import { requireUser } from "../src/auth/require-user.js";
import type { JwksResolver } from "../src/auth/verify.js";
import type {
  ListProvisioningRulesRequest,
  ListProvisioningRulesResponse,
  AddProvisioningRuleRequest,
  AddProvisioningRuleResponse,
  DeleteProvisioningRuleRequest,
  DeleteProvisioningRuleResponse,
  ProvisioningRule,
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

function fakeReq(authHeader?: string): { headers: Record<string, string> } {
  const headers: Record<string, string> = {};
  if (authHeader) headers.authorization = authHeader;
  return { headers };
}

interface ListBody { rules: { id: string; matcher: string; groups: string[]; createdBy: string; createdAt: string | null }[] }
interface AddBody { rule: { id: string; matcher: string; groups: string[]; createdBy: string; createdAt: string | null } | null; appliedCount: number }
interface DeleteBody { }

type ReplyBody = ListBody | AddBody | DeleteBody | Record<string, unknown> | undefined;

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

function isListBody(b: ReplyBody): b is ListBody {
  return b != null && typeof b === "object" && "rules" in b;
}

function isAddBody(b: ReplyBody): b is AddBody {
  return b != null && typeof b === "object" && "appliedCount" in b;
}

function asListBody(b: ReplyBody): ListBody {
  assert.ok(isListBody(b), "expected ListBody (rules)");
  return b;
}

function asAddBody(b: ReplyBody): AddBody {
  assert.ok(isAddBody(b), "expected AddBody (appliedCount)");
  return b;
}

// ── Stub north client ─────────────────────────────────────────────────────────

type NorthCall =
  | { method: "listProvisioningRules"; req: ListProvisioningRulesRequest; token: string | undefined }
  | { method: "addProvisioningRule"; req: AddProvisioningRuleRequest; token: string | undefined }
  | { method: "deleteProvisioningRule"; req: DeleteProvisioningRuleRequest; token: string | undefined };

function makeNorth(opts?: { rejectWith?: { code: number; message: string } }) {
  const calls: NorthCall[] = [];

  const fakeRule: ProvisioningRule = {
    id: "rule-uuid-1",
    matcher: "*@contoso.com",
    groups: ["security-team"],
    createdBy: "admin@example.com",
    createdAt: "",
  };

  return {
    calls,
    listProvisioningRules(
      req: ListProvisioningRulesRequest,
      token?: string,
    ): Promise<ListProvisioningRulesResponse> {
      calls.push({ method: "listProvisioningRules", req, token });
      if (opts?.rejectWith) return Promise.reject(opts.rejectWith);
      return Promise.resolve({ rules: [fakeRule] });
    },
    addProvisioningRule(
      req: AddProvisioningRuleRequest,
      token?: string,
    ): Promise<AddProvisioningRuleResponse> {
      calls.push({ method: "addProvisioningRule", req, token });
      if (opts?.rejectWith) return Promise.reject(opts.rejectWith);
      return Promise.resolve({ rule: fakeRule, appliedCount: 0 });
    },
    deleteProvisioningRule(
      req: DeleteProvisioningRuleRequest,
      token?: string,
    ): Promise<DeleteProvisioningRuleResponse> {
      calls.push({ method: "deleteProvisioningRule", req, token });
      if (opts?.rejectWith) return Promise.reject(opts.rejectWith);
      return Promise.resolve({});
    },
  };
}

// ── Inline route handlers (mirror server.ts logic exactly) ───────────────────

function adminErrorCode(err: unknown): number {
  const e = err as { code?: number };
  // gRPC PERMISSION_DENIED = 7
  return e?.code === 7 ? 403 : 400;
}

interface ProvisioningRuleJson {
  id: string;
  matcher: string;
  groups: string[];
  createdBy: string;
  createdAt?: string | Date;
}

function provRuleJson(r: ProvisioningRuleJson) {
  const createdAt = r.createdAt instanceof Date ? r.createdAt.toISOString() : (r.createdAt ?? null);
  return {
    id: r.id,
    matcher: r.matcher,
    groups: r.groups,
    createdBy: r.createdBy,
    createdAt,
  };
}

async function handleList(
  req: { headers: Record<string, string> },
  reply: FakeReply,
  jwksResolver: JwksResolver,
  north: ReturnType<typeof makeNorth>,
) {
  const principal = await requireUser(req, reply, jwksResolver, VERIFY_OPTS);
  if (!principal) return;
  try {
    const resp = await north.listProvisioningRules(
      { tenantId: principal.tenant, userId: principal.sub },
      principal.token,
    );
    reply.send({ rules: (resp.rules ?? []).map(provRuleJson) });
  } catch (err) {
    reply.code(adminErrorCode(err)).send({ rules: [], error: String(err) });
  }
}

async function handleAdd(
  req: { headers: Record<string, string> },
  reply: FakeReply,
  jwksResolver: JwksResolver,
  north: ReturnType<typeof makeNorth>,
  matcher: string,
  groups: string[],
) {
  const principal = await requireUser(req, reply, jwksResolver, VERIFY_OPTS);
  if (!principal) return;
  try {
    const resp = await north.addProvisioningRule(
      { tenantId: principal.tenant, userId: principal.sub, matcher, groups },
      principal.token,
    );
    reply.send({
      rule: resp.rule ? provRuleJson(resp.rule) : null,
      appliedCount: resp.appliedCount,
    });
  } catch (err) {
    reply.code(adminErrorCode(err)).send({ error: String(err) });
  }
}

async function handleDelete(
  req: { headers: Record<string, string> },
  reply: FakeReply,
  jwksResolver: JwksResolver,
  north: ReturnType<typeof makeNorth>,
  id: string,
) {
  const principal = await requireUser(req, reply, jwksResolver, VERIFY_OPTS);
  if (!principal) return;
  try {
    await north.deleteProvisioningRule(
      { tenantId: principal.tenant, userId: principal.sub, id },
      principal.token,
    );
    reply.send({});
  } catch (err) {
    reply.code(adminErrorCode(err)).send({ error: String(err) });
  }
}

// ── 401-gate tests ────────────────────────────────────────────────────────────

test("GET /admin/provisioning — no bearer → 401", async () => {
  const { jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const reply = fakeReply();

  await handleList(fakeReq(), reply, resolver, north);

  assert.equal(reply.statusCode, 401);
  assert.equal(north.calls.length, 0);
});

test("POST /admin/provisioning — no bearer → 401", async () => {
  const { jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const reply = fakeReply();

  await handleAdd(fakeReq(), reply, resolver, north, "*@contoso.com", ["security-team"]);

  assert.equal(reply.statusCode, 401);
  assert.equal(north.calls.length, 0);
});

test("POST /admin/provisioning/delete — no bearer → 401", async () => {
  const { jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const reply = fakeReply();

  await handleDelete(fakeReq(), reply, resolver, north, "rule-uuid-1");

  assert.equal(reply.statusCode, 401);
  assert.equal(north.calls.length, 0);
});

// ── Happy-path tests ──────────────────────────────────────────────────────────

test("GET /admin/provisioning — valid bearer calls listProvisioningRules, returns rules", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleList(fakeReq(`Bearer ${token}`), reply, resolver, north);

  assert.equal(north.calls.length, 1);
  const call = north.calls[0];
  assert.ok(call.method === "listProvisioningRules");
  assert.equal(call.token, token);
  assert.equal(call.req.tenantId, "11111111-1111-1111-1111-111111111111");
  assert.equal(call.req.userId, "user-uuid-admin");

  const body = asListBody(reply.body);
  assert.equal(body.rules.length, 1);
  assert.equal(body.rules[0].id, "rule-uuid-1");
  assert.equal(body.rules[0].matcher, "*@contoso.com");
  assert.deepEqual(body.rules[0].groups, ["security-team"]);
});

test("POST /admin/provisioning — valid bearer calls addProvisioningRule, returns rule+appliedCount", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleAdd(fakeReq(`Bearer ${token}`), reply, resolver, north, "*@contoso.com", ["security-team"]);

  assert.equal(north.calls.length, 1);
  const call = north.calls[0];
  assert.ok(call.method === "addProvisioningRule");
  assert.equal(call.token, token);
  assert.equal(call.req.tenantId, "11111111-1111-1111-1111-111111111111");
  assert.equal(call.req.userId, "user-uuid-admin");
  assert.equal(call.req.matcher, "*@contoso.com");
  assert.deepEqual(call.req.groups, ["security-team"]);

  const body = asAddBody(reply.body);
  assert.ok(body.rule !== null);
  assert.equal(body.rule?.id, "rule-uuid-1");
  assert.equal(body.appliedCount, 0);
});

test("POST /admin/provisioning/delete — valid bearer calls deleteProvisioningRule, returns 200 empty", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleDelete(fakeReq(`Bearer ${token}`), reply, resolver, north, "rule-uuid-1");

  assert.equal(north.calls.length, 1);
  const call = north.calls[0];
  assert.ok(call.method === "deleteProvisioningRule");
  assert.equal(call.token, token);
  assert.equal(call.req.tenantId, "11111111-1111-1111-1111-111111111111");
  assert.equal(call.req.userId, "user-uuid-admin");
  assert.equal(call.req.id, "rule-uuid-1");

  // 204-equivalent: statusCode never set (0) and body is empty object
  assert.equal(reply.statusCode, 0);
  assert.deepEqual(reply.body, {});
});

// ── Error mapping tests ───────────────────────────────────────────────────────

test("GET /admin/provisioning — PermissionDenied (code 7) → 403", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth({ rejectWith: { code: 7, message: "PermissionDenied" } });
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleList(fakeReq(`Bearer ${token}`), reply, resolver, north);

  assert.equal(reply.statusCode, 403);
});

test("POST /admin/provisioning — InvalidArgument (code 3) → 400", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth({ rejectWith: { code: 3, message: "InvalidArgument: bad matcher" } });
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleAdd(fakeReq(`Bearer ${token}`), reply, resolver, north, "bad!", []);

  assert.equal(reply.statusCode, 400);
});
