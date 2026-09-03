// Tests for GET /delegatable-users:
//   - forwards principal.sub / principal.tenant to north.listDelegatableUsers
//   - maps proto DelegatableUser[] → { userId, displayName }[]
//   - north error → 400 { users: [] }
//
// Strategy: mirror admin-keys.test.ts — fake req/reply + stub north client,
// no full Fastify server (server.ts does not export `app`).
import { test } from "node:test";
import assert from "node:assert/strict";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import { requireUser } from "../src/auth/require-user.js";
import type { JwksResolver } from "../src/auth/verify.js";
import type {
  ListDelegatableUsersRequest,
  ListDelegatableUsersResponse,
} from "../gen/ts/proto/broker.js";

// ── Key / token helpers (same shape as admin-keys.test.ts) ───────────────────

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
  sub: string,
  tenant: string,
) {
  return new SignJWT({ sub, email: `${sub}@example.com`, tenant_id: tenant })
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

type ReplyBody = { users: { userId: string; displayName: string }[]; groups?: { groupId: string; displayName: string; memberCount: number }[]; error?: string } | undefined;

function fakeReply() {
  let statusCode = 200;
  let body: ReplyBody;
  return {
    code(n: number) { statusCode = n; return this; },
    send(b: ReplyBody) { body = b; return this; },
    get statusCode() { return statusCode; },
    get body() { return body; },
  };
}

// ── Stub north client ─────────────────────────────────────────────────────────

// Only the one method the route calls needs to be stubbed; the rest of NorthClient
// is irrelevant here and excluded intentionally to keep the stub minimal.
function makeNorth(impl: (req: ListDelegatableUsersRequest, token?: string) => Promise<ListDelegatableUsersResponse>) {
  return { listDelegatableUsers: impl };
}

// ── Route handler under test ──────────────────────────────────────────────────
//
// Extracted from server.ts so we can inject the stub north client. This is the
// exact body of the /delegatable-users route; the test owns the handler lifetime.
async function handleDelegatableUsers(
  req: { headers: Record<string, string> },
  reply: ReturnType<typeof fakeReply>,
  north: ReturnType<typeof makeNorth>,
  jwksResolver: JwksResolver,
) {
  const principal = await requireUser(req as never, reply as never, jwksResolver, VERIFY_OPTS);
  if (!principal) return;
  try {
    const resp = await north.listDelegatableUsers(
      { userId: principal.sub, tenantId: principal.tenant },
      principal.token,
    );
    reply.send({
      users: (resp.users ?? []).map((u) => ({ userId: u.userId, displayName: u.displayName })),
      groups: (resp.groups ?? []).map((g) => ({ groupId: g.groupId, displayName: g.displayName, memberCount: g.memberCount })),
    });
  } catch (err) {
    reply.code(400).send({ users: [], groups: [], error: String(err) });
  }
}

// ── Tests ─────────────────────────────────────────────────────────────────────

test("delegatable-users: forwards principal.sub and principal.tenant to north", async () => {
  // WHY: the route must bind the caller's verified identity, not anything from
  // the request body — the broker will re-validate, but the gateway must send
  // the correct ids so the broker query is scoped to the right tenant/user.
  const { privateKey, jwk } = await makeKey();
  const token = await mintToken(privateKey, "alice-uuid", "tenant-abc");
  const jwksResolver = await localResolver(jwk);

  let capturedReq: ListDelegatableUsersRequest | undefined;
  let capturedToken: string | undefined;

  const north = makeNorth(async (req, tok) => {
    capturedReq = req;
    capturedToken = tok;
    return { users: [], groups: [] };
  });

  const reply = fakeReply();
  await handleDelegatableUsers(fakeReq(`Bearer ${token}`), reply, north, jwksResolver);

  assert.equal(capturedReq?.userId, "alice-uuid", "userId must be principal.sub");
  assert.equal(capturedReq?.tenantId, "tenant-abc", "tenantId must be principal.tenant");
  assert.equal(capturedToken, token, "bearer token forwarded to north unchanged");
  assert.equal(reply.statusCode, 200);
});

test("delegatable-users: maps proto users to { userId, displayName }", async () => {
  // WHY: the route projects only the fields the UI needs; proto field names are
  // camelCase (ts-proto) — userId / displayName — so no renaming needed, but
  // the mapping must be explicit so the response contract is stable if the proto
  // message gains extra fields in future.
  const { privateKey, jwk } = await makeKey();
  const token = await mintToken(privateKey, "alice-uuid", "tenant-abc");
  const jwksResolver = await localResolver(jwk);

  const north = makeNorth(async () => ({
    users: [
      { userId: "bob-uuid", displayName: "Bob Smith" },
      { userId: "carol-uuid", displayName: "Carol Jones" },
    ],
    groups: [],
  }));

  const reply = fakeReply();
  await handleDelegatableUsers(fakeReq(`Bearer ${token}`), reply, north, jwksResolver);

  assert.equal(reply.statusCode, 200);
  const body = reply.body;
  assert.ok(body, "response body must be present");
  assert.equal(body.users.length, 2);
  assert.deepEqual(body.users[0], { userId: "bob-uuid", displayName: "Bob Smith" });
  assert.deepEqual(body.users[1], { userId: "carol-uuid", displayName: "Carol Jones" });
});

test("delegatable-users: north error → 400 with { users: [] }", async () => {
  // WHY: north errors are operational (FGA unavailable, broker restart) and
  // must not crash the gateway with a 500; the UI degrades gracefully to an
  // empty popover. The error string is surfaced for operator debugging.
  const { privateKey, jwk } = await makeKey();
  const token = await mintToken(privateKey, "alice-uuid", "tenant-abc");
  const jwksResolver = await localResolver(jwk);

  const north = makeNorth(async () => {
    throw new Error("broker unavailable");
  });

  const reply = fakeReply();
  await handleDelegatableUsers(fakeReq(`Bearer ${token}`), reply, north, jwksResolver);

  assert.equal(reply.statusCode, 400);
  const body = reply.body;
  assert.ok(body, "response body must be present");
  assert.deepEqual(body.users, []);
  assert.deepEqual(body.groups, []);
  assert.ok(typeof body.error === "string" && body.error.includes("broker unavailable"));
});

test("delegatable-users: maps proto groups to { groupId, displayName, memberCount }", async () => {
  // WHY: the route explicitly projects group fields so the response contract is stable
  // if the proto message gains extra fields; camelCase mapping (ts-proto) must be verified.
  const { privateKey, jwk } = await makeKey();
  const token = await mintToken(privateKey, "alice-uuid", "tenant-abc");
  const jwksResolver = await localResolver(jwk);

  const north = makeNorth(async () => ({
    users: [],
    groups: [
      { groupId: "group:security-team", displayName: "group:security-team", memberCount: 3 },
      { groupId: "group:ops", displayName: "Ops Team", memberCount: 1 },
    ],
  }));

  const reply = fakeReply();
  await handleDelegatableUsers(fakeReq(`Bearer ${token}`), reply, north, jwksResolver);

  assert.equal(reply.statusCode, 200);
  const body = reply.body;
  assert.ok(body, "response body must be present");
  assert.ok(body.groups, "groups must be present");
  assert.equal(body.groups.length, 2);
  assert.deepEqual(body.groups[0], { groupId: "group:security-team", displayName: "group:security-team", memberCount: 3 });
  assert.deepEqual(body.groups[1], { groupId: "group:ops", displayName: "Ops Team", memberCount: 1 });
});
