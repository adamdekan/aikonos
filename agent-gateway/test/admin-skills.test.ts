// Tests for skill admin routes:
//   GET    /admin/skills    — read-only tool vocabulary
//   POST   /admin/skills    — upsert (create/update) a skill overlay
//   DELETE /admin/skills/:toolId — remove a skill overlay row
//
// Strategy: mirror admin-provisioning.test.ts — inline the handler logic with
// a fake req/reply + stub north client, no full Fastify server required.
import { test } from "node:test";
import assert from "node:assert/strict";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import { requireUser } from "../src/auth/require-user.js";
import type { JwksResolver } from "../src/auth/verify.js";
import type {
  ListSkillsRequest,
  ListSkillsResponse,
  UpsertSkillRequest,
  UpsertSkillResponse,
  DeleteSkillRequest,
  DeleteSkillResponse,
  AdminSkill,
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

interface SkillJson {
  toolId: string;
  scope: string;
  enabled: boolean;
  effectClass: string;
  displayName: string;
  description: string;
  executorKind: string;
}

interface SkillsBody { skills: SkillJson[] }
interface UpsertBody { skill: SkillJson | null }
interface DeleteBody { success: boolean }

type ReplyBody = SkillsBody | UpsertBody | DeleteBody | Record<string, unknown> | undefined;

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

function asSkillsBody(b: ReplyBody): SkillsBody {
  assert.ok(b != null && typeof b === "object" && "skills" in b, "expected skills body");
  return b as SkillsBody;
}

function asUpsertBody(b: ReplyBody): UpsertBody {
  assert.ok(b != null && typeof b === "object" && "skill" in b, "expected upsert body");
  return b as UpsertBody;
}

// ── Stub north client ─────────────────────────────────────────────────────────

type NorthCall =
  | { method: "listSkills"; req: ListSkillsRequest; token: string | undefined }
  | { method: "upsertSkill"; req: UpsertSkillRequest; token: string | undefined }
  | { method: "deleteSkill"; req: DeleteSkillRequest; token: string | undefined };

function makeNorth(opts?: { rejectWith?: { code: number; message: string } }) {
  const calls: NorthCall[] = [];

  const fakeSkills: AdminSkill[] = [
    {
      toolId: "doc.write",
      scope: "doc:write",
      enabled: true,
      effectClass: "write_local",
      displayName: "Write Document",
      description: "Writes a document to the workspace",
      executorKind: "builtin",
    },
    {
      toolId: "web.fetch",
      scope: "web:read",
      enabled: true,
      effectClass: "read_external",
      displayName: "Fetch URL",
      description: "Fetches content from a URL",
      executorKind: "builtin",
    },
  ];

  const fakeUpserted: AdminSkill = {
    toolId: "web.fetch",
    scope: "web:read",
    enabled: false,
    effectClass: "read_external",
    displayName: "Fetch URL",
    description: "Updated description",
    executorKind: "builtin",
  };

  return {
    calls,
    listSkills(req: ListSkillsRequest, token?: string): Promise<ListSkillsResponse> {
      calls.push({ method: "listSkills", req, token });
      if (opts?.rejectWith) return Promise.reject(opts.rejectWith);
      return Promise.resolve({ skills: fakeSkills });
    },
    upsertSkill(req: UpsertSkillRequest, token?: string): Promise<UpsertSkillResponse> {
      calls.push({ method: "upsertSkill", req, token });
      if (opts?.rejectWith) return Promise.reject(opts.rejectWith);
      return Promise.resolve({ skill: fakeUpserted });
    },
    deleteSkill(req: DeleteSkillRequest, token?: string): Promise<DeleteSkillResponse> {
      calls.push({ method: "deleteSkill", req, token });
      if (opts?.rejectWith) return Promise.reject(opts.rejectWith);
      return Promise.resolve({});
    },
  };
}

// ── Inline route handlers (mirror admin.ts logic exactly) ─────────────────────

function adminErrorCode(err: unknown): number {
  const e = err as { code?: number };
  // gRPC PERMISSION_DENIED = 7
  return e?.code === 7 ? 403 : 400;
}

async function handleListSkills(
  req: { headers: Record<string, string> },
  reply: FakeReply,
  jwksResolver: JwksResolver,
  north: ReturnType<typeof makeNorth>,
) {
  const principal = await requireUser(req, reply, jwksResolver, VERIFY_OPTS);
  if (!principal) return;
  try {
    const resp = await north.listSkills(
      { tenantId: principal.tenant, userId: principal.sub },
      principal.token,
    );
    reply.send({
      skills: (resp.skills ?? []).map((s) => ({
        toolId: s.toolId,
        scope: s.scope,
        enabled: s.enabled,
        effectClass: s.effectClass,
        displayName: s.displayName,
        description: s.description,
        executorKind: s.executorKind,
      })),
    });
  } catch (err) {
    reply.code(adminErrorCode(err)).send({ error: String(err) });
  }
}

interface UpsertBody2 {
  toolId?: string;
  effectClass?: string;
  displayName?: string;
  description?: string;
  enabled?: boolean;
  scope?: string;
}

async function handleUpsertSkill(
  req: { headers: Record<string, string> },
  reply: FakeReply,
  jwksResolver: JwksResolver,
  north: ReturnType<typeof makeNorth>,
  body: UpsertBody2,
) {
  const principal = await requireUser(req, reply, jwksResolver, VERIFY_OPTS);
  if (!principal) return;
  const b = body;
  try {
    const resp = await north.upsertSkill(
      {
        tenantId: principal.tenant,
        userId: principal.sub,
        toolId: b.toolId ?? "",
        effectClass: b.effectClass ?? "",
        displayName: b.displayName ?? "",
        description: b.description ?? "",
        enabled: b.enabled !== undefined ? b.enabled : true,
        scope: b.scope ?? "",
      },
      principal.token,
    );
    const s = resp.skill;
    reply.send({
      skill: s
        ? {
            toolId: s.toolId,
            scope: s.scope,
            enabled: s.enabled,
            effectClass: s.effectClass,
            displayName: s.displayName,
            description: s.description,
            executorKind: s.executorKind,
          }
        : null,
    });
  } catch (err) {
    reply.code(adminErrorCode(err)).send({ error: String(err) });
  }
}

async function handleDeleteSkill(
  req: { headers: Record<string, string> },
  reply: FakeReply,
  jwksResolver: JwksResolver,
  north: ReturnType<typeof makeNorth>,
  toolId: string,
) {
  const principal = await requireUser(req, reply, jwksResolver, VERIFY_OPTS);
  if (!principal) return;
  try {
    await north.deleteSkill(
      { tenantId: principal.tenant, userId: principal.sub, toolId },
      principal.token,
    );
    reply.send({ success: true });
  } catch (err) {
    reply.code(adminErrorCode(err)).send({ error: String(err) });
  }
}

// ── GET /admin/skills tests ───────────────────────────────────────────────────

test("GET /admin/skills — no bearer → 401", async () => {
  const { jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const reply = fakeReply();

  await handleListSkills(fakeReq(), reply, resolver, north);

  assert.equal(reply.statusCode, 401);
  assert.equal(north.calls.length, 0);
});

test("GET /admin/skills — valid bearer calls listSkills, returns all overlay fields", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleListSkills(fakeReq(`Bearer ${token}`), reply, resolver, north);

  assert.equal(north.calls.length, 1);
  const call = north.calls[0];
  assert.equal(call.method, "listSkills");
  assert.equal(call.token, token);
  assert.equal(call.req.tenantId, "11111111-1111-1111-1111-111111111111");
  assert.equal(call.req.userId, "user-uuid-admin");

  const body = asSkillsBody(reply.body);
  assert.equal(body.skills.length, 2);
  assert.equal(body.skills[0].toolId, "doc.write");
  assert.equal(body.skills[0].scope, "doc:write");
  assert.equal(body.skills[0].enabled, true);
  assert.equal(body.skills[0].effectClass, "write_local");
  assert.equal(body.skills[0].executorKind, "builtin");
  assert.equal(body.skills[1].toolId, "web.fetch");
  assert.equal(body.skills[1].scope, "web:read");
});

test("GET /admin/skills — PermissionDenied (code 7) → 403", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth({ rejectWith: { code: 7, message: "PermissionDenied" } });
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleListSkills(fakeReq(`Bearer ${token}`), reply, resolver, north);

  assert.equal(reply.statusCode, 403);
});

test("GET /admin/skills — non-PermissionDenied error → 400", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth({ rejectWith: { code: 13, message: "Internal error" } });
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleListSkills(fakeReq(`Bearer ${token}`), reply, resolver, north);

  assert.equal(reply.statusCode, 400);
});

// ── POST /admin/skills (upsert) tests ─────────────────────────────────────────

test("POST /admin/skills — no bearer → 401", async () => {
  const { jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const reply = fakeReply();

  await handleUpsertSkill(fakeReq(), reply, resolver, north, { toolId: "web.fetch" });

  assert.equal(reply.statusCode, 401);
  assert.equal(north.calls.length, 0);
});

test("POST /admin/skills — valid bearer calls upsertSkill, returns mapped skill", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleUpsertSkill(fakeReq(`Bearer ${token}`), reply, resolver, north, {
    toolId: "web.fetch",
    enabled: false,
    description: "Updated description",
    effectClass: "read_external",
    displayName: "Fetch URL",
    scope: "web:read",
  });

  assert.equal(north.calls.length, 1);
  const call = north.calls[0];
  assert.equal(call.method, "upsertSkill");
  assert.equal(call.token, token);
  assert.equal(call.req.tenantId, "11111111-1111-1111-1111-111111111111");
  assert.equal(call.req.userId, "user-uuid-admin");
  assert.equal(call.req.toolId, "web.fetch");
  assert.equal(call.req.enabled, false);
  assert.equal(call.req.description, "Updated description");
  assert.equal(call.req.effectClass, "read_external");
  assert.equal(call.req.scope, "web:read");

  const body = asUpsertBody(reply.body);
  assert.ok(body.skill !== null);
  assert.equal(body.skill?.toolId, "web.fetch");
  assert.equal(body.skill?.enabled, false);
  assert.equal(body.skill?.effectClass, "read_external");
  assert.equal(body.skill?.executorKind, "builtin");
});

test("POST /admin/skills — omitted enabled defaults to true", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleUpsertSkill(fakeReq(`Bearer ${token}`), reply, resolver, north, {
    toolId: "web.fetch",
  });

  assert.equal(north.calls.length, 1);
  const call = north.calls[0];
  assert.equal(call.method, "upsertSkill");
  assert.equal(call.req.enabled, true);
});

test("POST /admin/skills — PermissionDenied (code 7) → 403", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth({ rejectWith: { code: 7, message: "PermissionDenied" } });
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleUpsertSkill(fakeReq(`Bearer ${token}`), reply, resolver, north, {
    toolId: "web.fetch",
  });

  assert.equal(reply.statusCode, 403);
});

// ── DELETE /admin/skills/:toolId tests ───────────────────────────────────────

test("DELETE /admin/skills/:toolId — no bearer → 401", async () => {
  const { jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const reply = fakeReply();

  await handleDeleteSkill(fakeReq(), reply, resolver, north, "web.fetch");

  assert.equal(reply.statusCode, 401);
  assert.equal(north.calls.length, 0);
});

test("DELETE /admin/skills/:toolId — valid bearer calls deleteSkill with path param", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleDeleteSkill(fakeReq(`Bearer ${token}`), reply, resolver, north, "web.fetch");

  assert.equal(north.calls.length, 1);
  const call = north.calls[0];
  assert.equal(call.method, "deleteSkill");
  assert.equal(call.token, token);
  assert.equal(call.req.tenantId, "11111111-1111-1111-1111-111111111111");
  assert.equal(call.req.userId, "user-uuid-admin");
  assert.equal(call.req.toolId, "web.fetch");

  assert.deepEqual(reply.body, { success: true });
});

test("DELETE /admin/skills/:toolId — PermissionDenied (code 7) → 403", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth({ rejectWith: { code: 7, message: "PermissionDenied" } });
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleDeleteSkill(fakeReq(`Bearer ${token}`), reply, resolver, north, "web.fetch");

  assert.equal(reply.statusCode, 403);
});
