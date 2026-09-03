// Tests for the personal-skills gateway routes.
//
// Two strategies, matching the file's two shapes of problem:
//   - POST /skills/import (content-type dispatch + gateway-local business
//     logic) is tested by driving the exported handleSkillImport directly —
//     mirrors test/skill-upload.test.ts's handleSkillUpload strategy, no
//     Fastify content-type-parser plumbing needed.
//   - The other five routes are thin broker forwards, tested via a live
//     Fastify instance + app.inject — mirrors test/workspace-prefs-route.test.ts.
import { test } from "node:test";
import assert from "node:assert/strict";
import { deflateRawSync } from "node:zlib";
import Fastify from "fastify";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import {
  registerSkillsRoutes,
  handleSkillImport,
  isSafeExtrasPath,
  type ImportReq,
  type ImportReply,
} from "../src/routes/skills.js";
import type { JwksResolver } from "../src/auth/verify.js";
import type {
  ListPersonalSkillsRequest,
  ListPersonalSkillsResponse,
  DeletePersonalSkillRequest,
  DeletePersonalSkillResponse,
  SendSkillTransferRequest,
  SendSkillTransferResponse,
  GetSkillTransferPreviewRequest,
  GetSkillTransferPreviewResponse,
  AcceptSkillTransferRequest,
  AcceptSkillTransferResponse,
  UploadWorkspaceFileRequest,
  UploadWorkspaceFileResponse,
} from "../gen/ts/proto/broker.js";

// ── auth fixtures (mirrors workspace-prefs-route.test.ts / skill-upload.test.ts) ──

async function makeKey() {
  const { publicKey, privateKey } = await generateKeyPair("RS256");
  const jwk: JWK = { ...(await exportJWK(publicKey)), kid: "k1", alg: "RS256", use: "sig" };
  return { privateKey, jwk };
}

async function localResolver(jwk: JWK): Promise<JwksResolver> {
  const key = await importJWK(jwk, "RS256");
  return () => Promise.resolve(key);
}

const VERIFY_OPTS = { issuer: "http://localhost:18080/realms/aikonos", audience: "aikonos-broker" };

async function mintToken(privateKey: Awaited<ReturnType<typeof generateKeyPair>>["privateKey"]) {
  return new SignJWT({ sub: "alice@example.com", email: "alice@example.com", tenant_id: "aikonos-dev" })
    .setProtectedHeader({ alg: "RS256", kid: "k1" })
    .setIssuer(VERIFY_OPTS.issuer)
    .setAudience(VERIFY_OPTS.audience)
    .setIssuedAt()
    .setExpirationTime("1h")
    .sign(privateKey);
}

async function authFixture() {
  const { privateKey, jwk } = await makeKey();
  const jwksResolver = await localResolver(jwk);
  const token = await mintToken(privateKey);
  return { jwksResolver, token };
}

// ── minimal STORED-only ZIP builder (mirrors test/skill-upload.test.ts, no
// central directory needed — listZipEntries/readZipEntry only walk local
// headers) ───────────────────────────────────────────────────────────────────

function writeUInt16LE(buf: Buffer, offset: number, val: number) {
  buf.writeUInt16LE(val, offset);
}
function writeUInt32LE(buf: Buffer, offset: number, val: number) {
  buf.writeUInt32LE(val, offset);
}

function buildStoredZip(entries: { name: string; data: Buffer }[]): Buffer {
  const parts: Buffer[] = [];
  for (const entry of entries) {
    const nameBuf = Buffer.from(entry.name, "utf8");
    const local = Buffer.alloc(30 + nameBuf.length + entry.data.length);
    writeUInt32LE(local, 0, 0x04034b50);
    writeUInt16LE(local, 4, 20);
    writeUInt16LE(local, 6, 0);
    writeUInt16LE(local, 8, 0); // STORED
    writeUInt16LE(local, 10, 0);
    writeUInt16LE(local, 12, 0);
    writeUInt32LE(local, 14, 0);
    writeUInt32LE(local, 18, entry.data.length);
    writeUInt32LE(local, 22, entry.data.length);
    writeUInt16LE(local, 26, nameBuf.length);
    writeUInt16LE(local, 28, 0);
    nameBuf.copy(local, 30);
    entry.data.copy(local, 30 + nameBuf.length);
    parts.push(local);
  }
  return Buffer.concat(parts);
}

// Minimal DEFLATE-only ZIP builder (mirrors buildStoredZip above, local
// headers only) — used by the byte-cap test to keep the raw upload small
// while the decoded content is large.
function buildDeflatedZip(entries: { name: string; data: Buffer }[]): Buffer {
  const parts: Buffer[] = [];
  for (const entry of entries) {
    const nameBuf = Buffer.from(entry.name, "utf8");
    const compressed = deflateRawSync(entry.data);
    const local = Buffer.alloc(30 + nameBuf.length + compressed.length);
    writeUInt32LE(local, 0, 0x04034b50);
    writeUInt16LE(local, 4, 20);
    writeUInt16LE(local, 6, 0);
    writeUInt16LE(local, 8, 8); // DEFLATE
    writeUInt16LE(local, 10, 0);
    writeUInt16LE(local, 12, 0);
    writeUInt32LE(local, 14, 0);
    writeUInt32LE(local, 18, compressed.length);
    writeUInt32LE(local, 22, entry.data.length);
    writeUInt16LE(local, 26, nameBuf.length);
    writeUInt16LE(local, 28, 0);
    nameBuf.copy(local, 30);
    compressed.copy(local, 30 + nameBuf.length);
    parts.push(local);
  }
  return Buffer.concat(parts);
}

// ── handleSkillImport: direct-call tests ─────────────────────────────────────

interface FakeImportReply extends ImportReply {
  statusCode: number;
  body: unknown;
}

function fakeReply(): FakeImportReply {
  const r: FakeImportReply = {
    statusCode: 0,
    body: undefined,
    code(n) { r.statusCode = n; return r; },
    send(b) { r.body = b; },
  };
  return r;
}

type NorthCall =
  | { method: "listPersonalSkills"; req: ListPersonalSkillsRequest }
  | { method: "uploadWorkspaceFile"; req: UploadWorkspaceFileRequest }
  | { method: "deletePersonalSkill"; req: DeletePersonalSkillRequest };

// failUploadOnCall (1-indexed) simulates an RPC failure partway through the
// SKILL.md + extras write loop, to drive the mid-import cleanup path.
function makeImportNorth(existingNames: string[] = [], opts: { failUploadOnCall?: number } = {}) {
  const calls: NorthCall[] = [];
  let uploadCount = 0;
  return {
    calls,
    async listPersonalSkills(req: ListPersonalSkillsRequest): Promise<ListPersonalSkillsResponse> {
      calls.push({ method: "listPersonalSkills", req });
      return {
        skills: existingNames.map((name) => ({
          name, description: "", keywords: [], allowedTools: [],
          disableModelInvocation: false, valid: true, warning: "", sizeBytes: 0,
        })),
      };
    },
    async uploadWorkspaceFile(req: UploadWorkspaceFileRequest): Promise<UploadWorkspaceFileResponse> {
      uploadCount++;
      calls.push({ method: "uploadWorkspaceFile", req });
      if (opts.failUploadOnCall === uploadCount) {
        throw new Error("simulated upload failure");
      }
      return { file: undefined };
    },
    async deletePersonalSkill(req: DeletePersonalSkillRequest): Promise<DeletePersonalSkillResponse> {
      calls.push({ method: "deletePersonalSkill", req });
      return {};
    },
  };
}

const BARE_SKILL_MD = `---
name: my-notes
description: Keeps my notes tidy
allowed-tools:
  - web.fetch
keywords:
  - notes
  - tidy
---

# My Notes

Body text.
`;

test("handleSkillImport: bare text/markdown writes SKILL.md verbatim (frontmatter incl. keywords preserved)", async () => {
  const { jwksResolver, token } = await authFixture();
  const north = makeImportNorth();
  const req: ImportReq = {
    headers: { authorization: `Bearer ${token}`, "content-type": "text/markdown" },
    body: Buffer.from(BARE_SKILL_MD),
  };
  const reply = fakeReply();

  await handleSkillImport(req, reply, jwksResolver, VERIFY_OPTS, north);

  assert.equal(reply.statusCode, 201, `expected 201, got ${reply.statusCode} body=${JSON.stringify(reply.body)}`);
  assert.deepEqual(reply.body, { name: "my-notes" });

  const uploads = north.calls.filter((c): c is { method: "uploadWorkspaceFile"; req: UploadWorkspaceFileRequest } => c.method === "uploadWorkspaceFile");
  assert.equal(uploads.length, 1, "exactly one file written for a bare (no extras) import");
  assert.equal(uploads[0].req.path, "Skills/my-notes/SKILL.md");
  const written = Buffer.from(uploads[0].req.content ?? new Uint8Array()).toString("utf8");
  assert.equal(written, BARE_SKILL_MD, "the raw SKILL.md text must be written verbatim, including the keywords field this parser doesn't model");
});

test("handleSkillImport: zip with a references/ extra writes SKILL.md + the extra file", async () => {
  const { jwksResolver, token } = await authFixture();
  const north = makeImportNorth();
  const zip = buildStoredZip([
    { name: "SKILL.md", data: Buffer.from(BARE_SKILL_MD) },
    { name: "references/cheatsheet.md", data: Buffer.from("# Cheatsheet\n") },
  ]);
  const req: ImportReq = {
    headers: { authorization: `Bearer ${token}`, "content-type": "application/zip" },
    body: zip,
  };
  const reply = fakeReply();

  await handleSkillImport(req, reply, jwksResolver, VERIFY_OPTS, north);

  assert.equal(reply.statusCode, 201, `expected 201, got ${reply.statusCode} body=${JSON.stringify(reply.body)}`);
  const uploads = north.calls.filter((c): c is { method: "uploadWorkspaceFile"; req: UploadWorkspaceFileRequest } => c.method === "uploadWorkspaceFile");
  const paths = uploads.map((u) => u.req.path).sort();
  assert.deepEqual(paths, ["Skills/my-notes/SKILL.md", "Skills/my-notes/references/cheatsheet.md"]);
});

// ── full-tree extraction: every extra type uploaded, binary-intact end to end
// ──────────────────────────

test("handleSkillImport: zip with scripts/ + nested binary asset — every extra uploaded at the right path, byte-identical", async () => {
  const { jwksResolver, token } = await authFixture();
  const north = makeImportNorth();
  const pngBytes = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x01, 0x02, 0x03]);
  const zip = buildStoredZip([
    { name: "SKILL.md", data: Buffer.from(BARE_SKILL_MD) },
    { name: "scripts/run.sh", data: Buffer.from("#!/bin/sh\necho hi\n") },
    { name: "assets/img/logo.png", data: pngBytes },
    { name: "LICENSE.txt", data: Buffer.from("MIT License\n") },
  ]);
  const req: ImportReq = {
    headers: { authorization: `Bearer ${token}`, "content-type": "application/zip" },
    body: zip,
  };
  const reply = fakeReply();

  await handleSkillImport(req, reply, jwksResolver, VERIFY_OPTS, north);

  assert.equal(reply.statusCode, 201, `expected 201, got ${reply.statusCode} body=${JSON.stringify(reply.body)}`);
  const uploads = north.calls.filter((c): c is { method: "uploadWorkspaceFile"; req: UploadWorkspaceFileRequest } => c.method === "uploadWorkspaceFile");
  const byPath = new Map(uploads.map((u) => [u.req.path, u.req.content]));

  assert.deepEqual(
    [...byPath.keys()].sort(),
    ["Skills/my-notes/LICENSE.txt", "Skills/my-notes/SKILL.md", "Skills/my-notes/assets/img/logo.png", "Skills/my-notes/scripts/run.sh"],
  );
  const logoContent = byPath.get("Skills/my-notes/assets/img/logo.png");
  assert.ok(logoContent, "logo.png must have been uploaded");
  assert.ok(Buffer.from(logoContent ?? new Uint8Array()).equals(pngBytes), "binary asset must round-trip byte-identical, not UTF-8 coerced");
});

// ── route-level caps ──────

test("handleSkillImport: total decoded bytes over the 20 MiB cap → 413, no write attempted", async () => {
  const { jwksResolver, token } = await authFixture();
  const north = makeImportNorth();
  // DEFLATE (highly compressible repeated-byte payloads) keeps the raw zip
  // body well under the route's 14 MiB bodyLimit while the *decoded*
  // SKILL.md + extras total exceeds the 20 MiB import cap. The parser's own
  // aggregate-extras cap (20 MiB) is satisfied by 4 extras of ~4.95 MiB each
  // (~19.8 MiB), but SKILL.md (~1.2 MiB) pushes the combined total over the
  // route's cap — the scenario this cap exists for.
  const bigFile = (n: number) => Buffer.alloc(n, 0x61);
  // 4 extras files, each 4,950,000 bytes = 19,800,000 bytes (just under parser's 20 MiB extras cap)
  const skillMdWithLargeBody = `---
name: my-notes
description: Keeps my notes tidy
allowed-tools:
  - web.fetch
keywords:
  - notes
  - tidy
---

# My Notes

${Buffer.alloc(1_200_000, "x").toString("utf8")}
`;
  const zip = buildDeflatedZip([
    { name: "SKILL.md", data: Buffer.from(skillMdWithLargeBody) },
    { name: "assets/a.bin", data: bigFile(4_950_000) },
    { name: "assets/b.bin", data: bigFile(4_950_000) },
    { name: "assets/c.bin", data: bigFile(4_950_000) },
    { name: "assets/d.bin", data: bigFile(4_950_000) },
  ]);
  const req: ImportReq = {
    headers: { authorization: `Bearer ${token}`, "content-type": "application/zip" },
    body: zip,
  };
  const reply = fakeReply();

  await handleSkillImport(req, reply, jwksResolver, VERIFY_OPTS, north);

  assert.equal(reply.statusCode, 413, `expected 413, got ${reply.statusCode} body=${JSON.stringify(reply.body)}`);
  assert.ok((reply.body as { error: string }).error.includes("20971520"), "error must name the byte cap");
  assert.ok(!north.calls.some((c) => c.method === "uploadWorkspaceFile"), "must never write when over the byte cap");
});

// ── zip-slip via extras keys ───────────────────────────────────────────────────────────────

test("isSafeExtrasPath: rejects .. traversal and absolute paths, accepts a normal extras key", () => {
  assert.equal(
    isSafeExtrasPath("my-notes", "references/../../../.agent/Sessions/evil.json"),
    false,
    "a .. chain that climbs out of Skills/<name>/ must be rejected",
  );
  // Unreachable via the real zip flow today — parseSkillMd only ever hands
  // isSafeExtrasPath a "references/"/"assets/"-prefixed (hence relative) key
  // — but a future loosening of that filter must not silently reopen this,
  // so the guard is unit-tested directly rather than left untested.
  assert.equal(isSafeExtrasPath("my-notes", "/etc/passwd"), false, "an absolute path must be rejected outright");
  assert.equal(isSafeExtrasPath("my-notes", "references/cheatsheet.md"), true, "an ordinary extras key stays safe");
});

test("handleSkillImport: zip with a ../-traversal extras key → 400, no write escapes Skills/<name>/", async () => {
  const { jwksResolver, token } = await authFixture();
  const north = makeImportNorth();
  const zip = buildStoredZip([
    { name: "SKILL.md", data: Buffer.from(BARE_SKILL_MD) },
    { name: "references/../../../.agent/Sessions/evil.json", data: Buffer.from('{"pwned":true}') },
  ]);
  const req: ImportReq = {
    headers: { authorization: `Bearer ${token}`, "content-type": "application/zip" },
    body: zip,
  };
  const reply = fakeReply();

  await handleSkillImport(req, reply, jwksResolver, VERIFY_OPTS, north);

  assert.equal(reply.statusCode, 400, `expected 400, got ${reply.statusCode} body=${JSON.stringify(reply.body)}`);
  assert.ok(
    !north.calls.some((c) => c.method === "uploadWorkspaceFile"),
    "a zip-slip extras key must be rejected before any write — including SKILL.md itself",
  );
});

test("handleSkillImport: mid-loop write failure → cleanup attempted on the partial Skills/<name>/", async () => {
  const { jwksResolver, token } = await authFixture();
  // Call 1 (SKILL.md) succeeds; call 2 (the extras file) throws — simulating
  // a partial import where SKILL.md already landed.
  const north = makeImportNorth([], { failUploadOnCall: 2 });
  const zip = buildStoredZip([
    { name: "SKILL.md", data: Buffer.from(BARE_SKILL_MD) },
    { name: "references/cheatsheet.md", data: Buffer.from("# Cheatsheet\n") },
  ]);
  const req: ImportReq = {
    headers: { authorization: `Bearer ${token}`, "content-type": "application/zip" },
    body: zip,
  };
  const reply = fakeReply();

  await handleSkillImport(req, reply, jwksResolver, VERIFY_OPTS, north);

  assert.equal(reply.statusCode, 500, `expected 500, got ${reply.statusCode} body=${JSON.stringify(reply.body)}`);
  const cleanup = north.calls.find((c): c is { method: "deletePersonalSkill"; req: DeletePersonalSkillRequest } => c.method === "deletePersonalSkill");
  assert.ok(cleanup, "a mid-loop write failure must attempt to delete the partial Skills/<name>/ folder");
  assert.equal(cleanup?.req.name, "my-notes");
});

test("handleSkillImport: existing dir → 409 with suggested_name, never overwrites", async () => {
  const { jwksResolver, token } = await authFixture();
  const north = makeImportNorth(["my-notes"]); // caller already has Skills/my-notes/
  const req: ImportReq = {
    headers: { authorization: `Bearer ${token}`, "content-type": "text/markdown" },
    body: Buffer.from(BARE_SKILL_MD),
  };
  const reply = fakeReply();

  await handleSkillImport(req, reply, jwksResolver, VERIFY_OPTS, north);

  assert.equal(reply.statusCode, 409, `expected 409, got ${reply.statusCode} body=${JSON.stringify(reply.body)}`);
  const body = reply.body as { suggested_name?: string };
  assert.equal(body.suggested_name, "my-notes-2");
  assert.ok(!north.calls.some((c) => c.method === "uploadWorkspaceFile"), "must never write when the name already exists");
});

test("handleSkillImport: unsupported content type → 415", async () => {
  const { jwksResolver, token } = await authFixture();
  const north = makeImportNorth();
  const req: ImportReq = {
    headers: { authorization: `Bearer ${token}`, "content-type": "application/octet-stream" },
    body: Buffer.from(BARE_SKILL_MD),
  };
  const reply = fakeReply();

  await handleSkillImport(req, reply, jwksResolver, VERIFY_OPTS, north);

  assert.equal(reply.statusCode, 415);
});

// ── the other five routes: live Fastify + app.inject ─────────────────────────

async function makeAppWithAuth() {
  const { jwksResolver, token } = await authFixture();
  const app = Fastify({ logger: false });
  const calls: Record<string, unknown> = {};
  const errors: Partial<Record<string, Error>> = {};
  let listUserAgentSkillsErr: Error | undefined;

  const north = {
    async listPersonalSkills(req: ListPersonalSkillsRequest): Promise<ListPersonalSkillsResponse> {
      calls.listPersonalSkills = req;
      if (errors.listPersonalSkills) throw errors.listPersonalSkills;
      return { skills: [{ name: "my-notes", description: "d", keywords: [], allowedTools: [], disableModelInvocation: false, valid: true, warning: "", sizeBytes: 12 }] };
    },
    async deletePersonalSkill(req: DeletePersonalSkillRequest): Promise<DeletePersonalSkillResponse> {
      calls.deletePersonalSkill = req;
      if (errors.deletePersonalSkill) throw errors.deletePersonalSkill;
      return {};
    },
    async sendSkillTransfer(req: SendSkillTransferRequest): Promise<SendSkillTransferResponse> {
      calls.sendSkillTransfer = req;
      if (errors.sendSkillTransfer) throw errors.sendSkillTransfer;
      return { envelopeIds: ["env-1"], skippedUserIds: ["carol@example.com"] };
    },
    async getSkillTransferPreview(req: GetSkillTransferPreviewRequest): Promise<GetSkillTransferPreviewResponse> {
      calls.getSkillTransferPreview = req;
      if (errors.getSkillTransferPreview) throw errors.getSkillTransferPreview;
      return {
        skillName: "my-notes", fromUserId: "bob@example.com", body: "# Notes\n",
        manifest: [{ path: "SKILL.md", size: 42 }], flags: ["suspicious-pattern"],
        contentHash: "abc123", conflict: false,
      };
    },
    async acceptSkillTransfer(req: AcceptSkillTransferRequest): Promise<AcceptSkillTransferResponse> {
      calls.acceptSkillTransfer = req;
      if (errors.acceptSkillTransfer) throw errors.acceptSkillTransfer;
      return { installedName: "my-notes" };
    },
    async uploadWorkspaceFile(): Promise<UploadWorkspaceFileResponse> {
      return { file: undefined };
    },
  };

  const south = {
    async listUserAgentSkills(req: { tenantId: string; userId: string }) {
      calls.listUserAgentSkills = req;
      if (listUserAgentSkillsErr) throw listUserAgentSkillsErr;
      return { bundles: [{ id: "b1", name: "deployer", description: "d", body: "", allowedTools: [], contextFork: false, disableModelInvocation: false, keywords: [], createdBy: "admin@example.com", filePaths: [] }] };
    },
  };

  registerSkillsRoutes(app, { clients: { north, south }, jwksResolver, verifyOpts: VERIFY_OPTS });

  return {
    app, calls, token,
    setErr: (method: keyof typeof errors, e: Error) => { errors[method] = e; },
    setListUserAgentSkillsErr: (e: Error) => { listUserAgentSkillsErr = e; },
  };
}

test("GET /skills — returns own skills + granted bundles", async () => {
  const { app, calls, token } = await makeAppWithAuth();
  await app.ready();

  const res = await app.inject({ method: "GET", url: "/skills", headers: { authorization: `Bearer ${token}` } });

  assert.equal(res.statusCode, 200);
  assert.deepEqual(calls.listPersonalSkills, { tenantId: "aikonos-dev", userId: "alice@example.com" });
  const body = JSON.parse(res.body);
  assert.equal(body.skills.length, 1);
  assert.equal(body.granted.length, 1);
  assert.equal(body.grantedUnavailable, false);

  await app.close();
});

test("GET /skills — south failure yields granted:[] + grantedUnavailable:true, skills unaffected (fail-open)", async () => {
  const { app, token, setListUserAgentSkillsErr } = await makeAppWithAuth();
  await app.ready();
  setListUserAgentSkillsErr(new Error("south unavailable"));

  const res = await app.inject({ method: "GET", url: "/skills", headers: { authorization: `Bearer ${token}` } });

  assert.equal(res.statusCode, 200, "a south hiccup must never deny the owner's own skills list");
  const body = JSON.parse(res.body);
  assert.deepEqual(body.granted, []);
  assert.equal(body.grantedUnavailable, true);

  await app.close();
});

test("DELETE /skills/:name — forwards name, returns success", async () => {
  const { app, calls, token } = await makeAppWithAuth();
  await app.ready();

  const res = await app.inject({ method: "DELETE", url: "/skills/my-notes", headers: { authorization: `Bearer ${token}` } });

  assert.equal(res.statusCode, 200);
  assert.deepEqual(calls.deletePersonalSkill, { tenantId: "aikonos-dev", userId: "alice@example.com", name: "my-notes" });

  await app.close();
});

test("POST /skills/:name/share — direct user_id body builds a userId recipient", async () => {
  const { app, calls, token } = await makeAppWithAuth();
  await app.ready();

  const res = await app.inject({
    method: "POST", url: "/skills/my-notes/share",
    headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
    payload: { user_id: "bob@example.com" },
  });

  assert.equal(res.statusCode, 200);
  assert.deepEqual(calls.sendSkillTransfer, {
    tenantId: "aikonos-dev", userId: "alice@example.com", name: "my-notes",
    recipient: { userId: "bob@example.com" },
  });
  const body = JSON.parse(res.body);
  assert.deepEqual(body, { envelopeIds: ["env-1"], skippedUserIds: ["carol@example.com"] }, "skip-report must pass through");

  await app.close();
});

test("POST /skills/:name/share — group_id body builds a groupId recipient", async () => {
  const { app, calls, token } = await makeAppWithAuth();
  await app.ready();

  const res = await app.inject({
    method: "POST", url: "/skills/my-notes/share",
    headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
    payload: { group_id: "group-eng" },
  });

  assert.equal(res.statusCode, 200);
  assert.deepEqual(calls.sendSkillTransfer, {
    tenantId: "aikonos-dev", userId: "alice@example.com", name: "my-notes",
    recipient: { groupId: "group-eng" },
  });

  await app.close();
});

test("GET /skills/transfers/:envelopeId — proxies the preview", async () => {
  const { app, calls, token } = await makeAppWithAuth();
  await app.ready();

  const res = await app.inject({ method: "GET", url: "/skills/transfers/env-1", headers: { authorization: `Bearer ${token}` } });

  assert.equal(res.statusCode, 200);
  assert.deepEqual(calls.getSkillTransferPreview, { tenantId: "aikonos-dev", userId: "alice@example.com", envelopeId: "env-1" });
  const body = JSON.parse(res.body);
  assert.equal(body.skillName, "my-notes");
  assert.deepEqual(body.flags, ["suspicious-pattern"], "injection flags must be surfaced, never swallowed");

  await app.close();
});

test("POST /skills/transfers/:envelopeId/accept — forwards mode + name_override, defaults mode to rename", async () => {
  const { app, calls, token } = await makeAppWithAuth();
  await app.ready();

  const res = await app.inject({
    method: "POST", url: "/skills/transfers/env-1/accept",
    headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
    payload: {},
  });

  assert.equal(res.statusCode, 200);
  assert.deepEqual(calls.acceptSkillTransfer, {
    tenantId: "aikonos-dev", userId: "alice@example.com", envelopeId: "env-1",
    mode: "rename", nameOverride: "",
  });

  await app.close();
});

test("POST /skills/transfers/:envelopeId/accept — replace mode with name_override", async () => {
  const { app, calls, token } = await makeAppWithAuth();
  await app.ready();

  const res = await app.inject({
    method: "POST", url: "/skills/transfers/env-1/accept",
    headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
    payload: { mode: "replace", name_override: "my-notes-renamed" },
  });

  assert.equal(res.statusCode, 200);
  assert.deepEqual(calls.acceptSkillTransfer, {
    tenantId: "aikonos-dev", userId: "alice@example.com", envelopeId: "env-1",
    mode: "replace", nameOverride: "my-notes-renamed",
  });

  await app.close();
});

// ── grpc error → HTTP status mapping (existing grpcToHttp — PermissionDenied
// → 403, FailedPrecondition → 409; the brief's "412" is not what the shared
// mapper (src/http-errors.ts) implements) ────────────────────────────────────

test("DELETE /skills/:name — PermissionDenied maps to 403", async () => {
  const { app, token, setErr } = await makeAppWithAuth();
  await app.ready();
  setErr("deletePersonalSkill", Object.assign(new Error("not the owner"), { code: 7 })); // PERMISSION_DENIED

  const res = await app.inject({ method: "DELETE", url: "/skills/my-notes", headers: { authorization: `Bearer ${token}` } });

  assert.equal(res.statusCode, 403);

  await app.close();
});

test("POST /skills/:name/share — FailedPrecondition (recipient lacks the capability) maps to 409", async () => {
  const { app, token, setErr } = await makeAppWithAuth();
  await app.ready();
  setErr("sendSkillTransfer", Object.assign(new Error("recipient lacks skill:personal-skills"), { code: 9 })); // FAILED_PRECONDITION

  const res = await app.inject({
    method: "POST", url: "/skills/my-notes/share",
    headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
    payload: { user_id: "dave@example.com" },
  });

  assert.equal(res.statusCode, 409);

  await app.close();
});

test("GET /skills/transfers/:envelopeId — FailedPrecondition (expired envelope) maps to 409", async () => {
  const { app, token, setErr } = await makeAppWithAuth();
  await app.ready();
  setErr("getSkillTransferPreview", Object.assign(new Error("envelope expired"), { code: 9 })); // FAILED_PRECONDITION

  const res = await app.inject({ method: "GET", url: "/skills/transfers/env-1", headers: { authorization: `Bearer ${token}` } });

  assert.equal(res.statusCode, 409);

  await app.close();
});

test("POST /skills/transfers/:envelopeId/accept — PermissionDenied maps to 403", async () => {
  const { app, token, setErr } = await makeAppWithAuth();
  await app.ready();
  setErr("acceptSkillTransfer", Object.assign(new Error("not the recipient"), { code: 7 })); // PERMISSION_DENIED

  const res = await app.inject({
    method: "POST", url: "/skills/transfers/env-1/accept",
    headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
    payload: {},
  });

  assert.equal(res.statusCode, 403);

  await app.close();
});
