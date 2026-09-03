// Tests for POST /admin/skills/upload — SKILL.md bundle upload route.
//
// Strategy: import the real handleSkillUpload exported from admin.ts and drive
// it with a stub north client + fake req/reply. No diverged handler copy.
import { test } from "node:test";
import assert from "node:assert/strict";
import { deflateRawSync } from "node:zlib";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import { requireUser } from "../src/auth/require-user.js";
import type { JwksResolver } from "../src/auth/verify.js";
import { handleSkillUpload } from "../src/routes/admin.js";
import { parseSkillMd } from "../src/pi/skill-parser.js";
import type {
  UpsertAgentSkillRequest,
  UpsertAgentSkillResponse,
  AgentSkillBundle,
} from "../gen/ts/proto/broker.js";

// ── Key / token helpers ───────────────────────────────────────────────────────

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
  isAdmin = true,
) {
  return new SignJWT({
    sub: isAdmin ? "user-uuid-admin" : "user-uuid-nonadmin",
    email: isAdmin ? "admin@example.com" : "user@example.com",
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

// ── Fake req / reply ──────────────────────────────────────────────────────────

interface FakeReq {
  headers: Record<string, string>;
  body?: Buffer;
  query?: { keywords?: string };
}

function fakeReq(
  authHeader?: string,
  body?: Buffer,
  contentType?: string,
  query?: { keywords?: string },
): FakeReq {
  const headers: Record<string, string> = {};
  if (authHeader) headers.authorization = authHeader;
  if (contentType) headers["content-type"] = contentType;
  return { headers, body, query };
}

type ReplyBody = Record<string, unknown> | undefined;

interface FakeReply {
  statusCode: number;
  body: ReplyBody;
  code(n: number): FakeReply;
  send(b: unknown): void;
}

function fakeReply(): FakeReply {
  const r: FakeReply = {
    statusCode: 0,
    body: undefined,
    code(n) { r.statusCode = n; return r; },
    send(b) { r.body = b as ReplyBody; },
  };
  return r;
}

// ── Stub north client ─────────────────────────────────────────────────────────

type NorthCall =
  | { method: "upsertAgentSkill"; req: UpsertAgentSkillRequest; token: string | undefined };

function makeNorth(opts?: { rejectWith?: { code: number; message: string } }) {
  const calls: NorthCall[] = [];

  const fakeBundle: AgentSkillBundle = {
    id: "aaaaaaaa-0000-0000-0000-000000000001",
    name: "my-skill",
    description: "A test skill",
    body: "## Instructions\n\nDo something useful.",
    allowedTools: ["web.fetch"],
    contextFork: false,
    disableModelInvocation: false,
    createdBy: "user-uuid-admin",
    keywords: [],
    // Non-empty so the response-passthrough test below can prove
    // handleSkillUpload doesn't strip this field, independent of what the
    // request itself uploaded.
    filePaths: ["LICENSE.txt", "scripts/run.sh"],
  };

  return {
    calls,
    upsertAgentSkill(req: UpsertAgentSkillRequest, token?: string): Promise<UpsertAgentSkillResponse> {
      calls.push({ method: "upsertAgentSkill", req, token });
      if (opts?.rejectWith) return Promise.reject(opts.rejectWith);
      return Promise.resolve({ bundle: fakeBundle });
    },
  };
}

// ── ZIP builder helpers ───────────────────────────────────────────────────────
// Builds minimal STORED (compression=0) ZIP buffers for integration tests.
// No external dependency — hand-roll the local file header + central directory.

function writeUInt16LE(buf: Buffer, offset: number, val: number) {
  buf.writeUInt16LE(val, offset);
}
function writeUInt32LE(buf: Buffer, offset: number, val: number) {
  buf.writeUInt32LE(val, offset);
}

interface ZipEntry {
  name: string;
  data: Buffer;
}

function buildStoredZip(entries: ZipEntry[]): Buffer {
  const localHeaders: Buffer[] = [];
  const centralDirs: Buffer[] = [];
  let localOffset = 0;

  for (const entry of entries) {
    const nameBuf = Buffer.from(entry.name, "utf8");
    const data = entry.data;

    // Local file header (30 bytes) + name + data
    const local = Buffer.alloc(30 + nameBuf.length + data.length);
    writeUInt32LE(local, 0, 0x04034b50);  // signature PK\x03\x04
    writeUInt16LE(local, 4, 20);           // version needed (2.0)
    writeUInt16LE(local, 6, 0);            // flags
    writeUInt16LE(local, 8, 0);            // compression: STORED
    writeUInt16LE(local, 10, 0);           // mod time
    writeUInt16LE(local, 12, 0);           // mod date
    writeUInt32LE(local, 14, 0);           // crc-32 (omitted — not validated by parser)
    writeUInt32LE(local, 18, data.length); // compressed size
    writeUInt32LE(local, 22, data.length); // uncompressed size
    writeUInt16LE(local, 26, nameBuf.length);
    writeUInt16LE(local, 28, 0);           // extra field length
    nameBuf.copy(local, 30);
    data.copy(local, 30 + nameBuf.length);
    localHeaders.push(local);

    // Central directory entry (46 bytes) + name
    const cd = Buffer.alloc(46 + nameBuf.length);
    writeUInt32LE(cd, 0, 0x02014b50);     // central dir signature
    writeUInt16LE(cd, 4, 20);              // version made by
    writeUInt16LE(cd, 6, 20);              // version needed
    writeUInt16LE(cd, 8, 0);              // flags
    writeUInt16LE(cd, 10, 0);             // compression: STORED
    writeUInt16LE(cd, 12, 0);             // mod time
    writeUInt16LE(cd, 14, 0);             // mod date
    writeUInt32LE(cd, 16, 0);             // crc-32
    writeUInt32LE(cd, 20, data.length);   // compressed size
    writeUInt32LE(cd, 24, data.length);   // uncompressed size
    writeUInt16LE(cd, 28, nameBuf.length);
    writeUInt16LE(cd, 30, 0);             // extra field length
    writeUInt16LE(cd, 32, 0);             // file comment length
    writeUInt16LE(cd, 34, 0);             // disk number start
    writeUInt16LE(cd, 36, 0);             // internal file attributes
    writeUInt32LE(cd, 38, 0);             // external file attributes
    writeUInt32LE(cd, 42, localOffset);   // relative offset of local header
    nameBuf.copy(cd, 46);
    centralDirs.push(cd);

    localOffset += local.length;
  }

  const localBuf = Buffer.concat(localHeaders);
  const cdBuf = Buffer.concat(centralDirs);
  const cdSize = cdBuf.length;

  // End of central directory record (22 bytes)
  const eocd = Buffer.alloc(22);
  writeUInt32LE(eocd, 0, 0x06054b50);    // EOCD signature
  writeUInt16LE(eocd, 4, 0);             // disk number
  writeUInt16LE(eocd, 6, 0);             // disk with central dir
  writeUInt16LE(eocd, 8, entries.length);
  writeUInt16LE(eocd, 10, entries.length);
  writeUInt32LE(eocd, 12, cdSize);
  writeUInt32LE(eocd, 16, localOffset);  // offset of central dir from start
  writeUInt16LE(eocd, 20, 0);            // comment length

  return Buffer.concat([localBuf, cdBuf, eocd]);
}

// Builds a ZIP whose entries are DEFLATE-compressed (compression method 8), as
// produced by the real `zip` CLI and most archivers. Mirrors buildStoredZip but
// stores raw-deflated data and the compressed length in the size fields.
function buildDeflatedZip(entries: ZipEntry[]): Buffer {
  const localHeaders: Buffer[] = [];
  const centralDirs: Buffer[] = [];
  let localOffset = 0;

  for (const entry of entries) {
    const nameBuf = Buffer.from(entry.name, "utf8");
    const compressed = deflateRawSync(entry.data);

    const local = Buffer.alloc(30 + nameBuf.length + compressed.length);
    writeUInt32LE(local, 0, 0x04034b50);
    writeUInt16LE(local, 4, 20);
    writeUInt16LE(local, 6, 0);
    writeUInt16LE(local, 8, 8);                    // compression: DEFLATE
    writeUInt16LE(local, 10, 0);
    writeUInt16LE(local, 12, 0);
    writeUInt32LE(local, 14, 0);                   // crc-32 (not validated)
    writeUInt32LE(local, 18, compressed.length);   // compressed size
    writeUInt32LE(local, 22, entry.data.length);   // uncompressed size
    writeUInt16LE(local, 26, nameBuf.length);
    writeUInt16LE(local, 28, 0);
    nameBuf.copy(local, 30);
    compressed.copy(local, 30 + nameBuf.length);
    localHeaders.push(local);

    const cd = Buffer.alloc(46 + nameBuf.length);
    writeUInt32LE(cd, 0, 0x02014b50);
    writeUInt16LE(cd, 4, 20);
    writeUInt16LE(cd, 6, 20);
    writeUInt16LE(cd, 8, 0);
    writeUInt16LE(cd, 10, 8);                       // compression: DEFLATE
    writeUInt16LE(cd, 12, 0);
    writeUInt16LE(cd, 14, 0);
    writeUInt32LE(cd, 16, 0);
    writeUInt32LE(cd, 20, compressed.length);
    writeUInt32LE(cd, 24, entry.data.length);
    writeUInt16LE(cd, 28, nameBuf.length);
    writeUInt16LE(cd, 30, 0);
    writeUInt16LE(cd, 32, 0);
    writeUInt16LE(cd, 34, 0);
    writeUInt16LE(cd, 36, 0);
    writeUInt32LE(cd, 38, 0);
    writeUInt32LE(cd, 42, localOffset);
    nameBuf.copy(cd, 46);
    centralDirs.push(cd);

    localOffset += local.length;
  }

  const localBuf = Buffer.concat(localHeaders);
  const cdBuf = Buffer.concat(centralDirs);
  const eocd = Buffer.alloc(22);
  writeUInt32LE(eocd, 0, 0x06054b50);
  writeUInt16LE(eocd, 4, 0);
  writeUInt16LE(eocd, 6, 0);
  writeUInt16LE(eocd, 8, entries.length);
  writeUInt16LE(eocd, 10, entries.length);
  writeUInt32LE(eocd, 12, cdBuf.length);
  writeUInt32LE(eocd, 16, localOffset);
  writeUInt16LE(eocd, 20, 0);

  return Buffer.concat([localBuf, cdBuf, eocd]);
}

// ── Sample SKILL.md fixtures ──────────────────────────────────────────────────

const VALID_SKILL_MD = `---
name: my-skill
description: A test skill
allowed-tools:
  - web.fetch
context: inline
disable-model-invocation: false
---

## Instructions

Do something useful.
`;

const SKILL_MD_DISABLE_MODEL = `---
name: quiet-skill
description: No model
allowed-tools:
  - doc.write
disable-model-invocation: true
---

Body here.
`;

const SKILL_MD_CONTEXT_FORK = `---
name: fork-skill
description: Forks
allowed-tools:
  - web.fetch
context: fork
---

Body.
`;

// ── parser unit tests ─────────────────────────────────────────────────────────

test("parseSkillMd — valid SKILL.md parses all frontmatter fields", () => {
  const result = parseSkillMd(VALID_SKILL_MD, { zip: false });
  assert.equal(result.name, "my-skill");
  assert.equal(result.description, "A test skill");
  assert.deepEqual(result.allowedTools, ["web.fetch"]);
  assert.equal(result.contextFork, false);
  assert.equal(result.disableModelInvocation, false);
  assert.ok(result.body.includes("Do something useful"));
});

test("parseSkillMd — disable-model-invocation: true parsed correctly", () => {
  const result = parseSkillMd(SKILL_MD_DISABLE_MODEL, { zip: false });
  assert.equal(result.name, "quiet-skill");
  assert.equal(result.disableModelInvocation, true);
  assert.deepEqual(result.allowedTools, ["doc.write"]);
});

test("parseSkillMd — context: fork sets contextFork=true", () => {
  const result = parseSkillMd(SKILL_MD_CONTEXT_FORK, { zip: false });
  assert.equal(result.contextFork, true);
  assert.equal(result.name, "fork-skill");
});

test("parseSkillMd — missing name throws", () => {
  const md = `---\ndescription: no name\nallowed-tools:\n  - web.fetch\n---\nBody.\n`;
  assert.throws(() => parseSkillMd(md, { zip: false }), /name/i);
});

test("parseSkillMd — missing frontmatter delimiter throws", () => {
  assert.throws(() => parseSkillMd("No frontmatter here.", { zip: false }), /frontmatter/i);
});

// ── route handler tests ───────────────────────────────────────────────────────

test("POST /admin/skills/upload — no bearer → 401", async () => {
  const { jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const reply = fakeReply();

  await handleSkillUpload(
    fakeReq(undefined, Buffer.from(VALID_SKILL_MD), "text/markdown"),
    reply,
    resolver,
    VERIFY_OPTS,
    north,
  );

  assert.equal(reply.statusCode, 401);
  assert.equal(north.calls.length, 0);
});

test("POST /admin/skills/upload — valid SKILL.md (text/markdown) → 201, calls upsertAgentSkill", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleSkillUpload(
    fakeReq(`Bearer ${token}`, Buffer.from(VALID_SKILL_MD), "text/markdown"),
    reply,
    resolver,
    VERIFY_OPTS,
    north,
  );

  assert.equal(reply.statusCode, 201);
  assert.equal(north.calls.length, 1);
  const call = north.calls[0];
  assert.equal(call.method, "upsertAgentSkill");
  assert.equal(call.token, token);
  assert.equal(call.req.tenantId, "11111111-1111-1111-1111-111111111111");
  assert.equal(call.req.userId, "user-uuid-admin");
  assert.equal(call.req.name, "my-skill");
  assert.equal(call.req.description, "A test skill");
  assert.deepEqual(call.req.allowedTools, ["web.fetch"]);
  assert.equal(call.req.contextFork, false);
  assert.equal(call.req.disableModelInvocation, false);
  assert.ok(call.req.body.includes("Do something useful"));
  assert.equal(call.req.id, "");

  const body = reply.body as { bundle: AgentSkillBundle };
  assert.equal(body.bundle.name, "my-skill");
});

test("POST /admin/skills/upload — valid SKILL.md (text/plain) → 201", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleSkillUpload(
    fakeReq(`Bearer ${token}`, Buffer.from(VALID_SKILL_MD), "text/plain"),
    reply,
    resolver,
    VERIFY_OPTS,
    north,
  );

  assert.equal(reply.statusCode, 201);
  assert.equal(north.calls.length, 1);
  assert.equal(north.calls[0].req.name, "my-skill");
});

test("POST /admin/skills/upload — unsupported content-type → 415", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleSkillUpload(
    fakeReq(`Bearer ${token}`, Buffer.from(VALID_SKILL_MD), "application/json"),
    reply,
    resolver,
    VERIFY_OPTS,
    north,
  );

  assert.equal(reply.statusCode, 415);
  assert.equal(north.calls.length, 0);
});

test("POST /admin/skills/upload — non-admin caller → 403 (broker rejects PermissionDenied)", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth({ rejectWith: { code: 7, message: "PermissionDenied" } });
  const token = await mintToken(privateKey, false);
  const reply = fakeReply();

  await handleSkillUpload(
    fakeReq(`Bearer ${token}`, Buffer.from(VALID_SKILL_MD), "text/markdown"),
    reply,
    resolver,
    VERIFY_OPTS,
    north,
  );

  assert.equal(reply.statusCode, 403);
});

test("POST /admin/skills/upload — unknown allowed-tools → 400 (broker InvalidArgument)", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth({ rejectWith: { code: 3, message: "unknown tool: no.such.tool" } });
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  const md = `---\nname: bad-skill\ndescription: d\nallowed-tools:\n  - no.such.tool\n---\nBody.\n`;
  await handleSkillUpload(
    fakeReq(`Bearer ${token}`, Buffer.from(md), "text/markdown"),
    reply,
    resolver,
    VERIFY_OPTS,
    north,
  );

  assert.equal(reply.statusCode, 400);
  assert.ok((reply.body as { error: string }).error);
});

test("POST /admin/skills/upload — name collision → 400 (broker InvalidArgument)", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth({ rejectWith: { code: 3, message: "name already taken: my-skill" } });
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleSkillUpload(
    fakeReq(`Bearer ${token}`, Buffer.from(VALID_SKILL_MD), "text/markdown"),
    reply,
    resolver,
    VERIFY_OPTS,
    north,
  );

  assert.equal(reply.statusCode, 400);
});

// ── Finding 2: 5 MiB gateway cap ──────────────────────────────────────────────

test("POST /admin/skills/upload — body > 5 MiB → 413 before any parsing", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  const oversized = Buffer.alloc(5 * 1024 * 1024 + 1, 0x61); // 5 MiB + 1 byte
  await handleSkillUpload(
    fakeReq(`Bearer ${token}`, oversized, "text/markdown"),
    reply,
    resolver,
    VERIFY_OPTS,
    north,
  );

  assert.equal(reply.statusCode, 413);
  assert.equal(north.calls.length, 0, "broker must not be called when body too large");
});

// ── zip with a scripts/ entry → accepted (scripts/ is inert, not a rejection) ──
// A bundle is inert: the gateway forwards only name/description/body/allowed-tools
// to the broker and executes nothing from the archive. A scripts/ entry is
// therefore harmless and must not block the upload.

test("POST /admin/skills/upload — zip containing scripts/ entry → 201, broker called", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  const zipBuf = buildStoredZip([
    { name: "SKILL.md", data: Buffer.from(VALID_SKILL_MD) },
    { name: "scripts/run.sh", data: Buffer.from("#!/bin/sh\necho hi\n") },
  ]);

  await handleSkillUpload(
    fakeReq(`Bearer ${token}`, zipBuf, "application/zip"),
    reply,
    resolver,
    VERIFY_OPTS,
    north,
  );

  assert.equal(reply.statusCode, 201);
  assert.equal(north.calls.length, 1, "broker must be called — scripts/ is not a rejection");
  assert.equal(north.calls[0].req.name, "my-skill");
});

// ── DEFLATE (compression 8) zip support ──────────────────────────────────────
// Real .zip/.skill archives compress entries with DEFLATE; the parser must
// inflate them, not only read STORED entries.

test("parseSkillMd — DEFLATE-compressed zip entry is inflated and parsed", () => {
  const zipBuf = buildDeflatedZip([
    { name: "iso-27001/SKILL.md", data: Buffer.from(VALID_SKILL_MD) },
  ]);
  const parsed = parseSkillMd(zipBuf, { zip: true });
  assert.equal(parsed.name, "my-skill");
});

test("POST /admin/skills/upload — DEFLATE zip → 201, broker called", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  const zipBuf = buildDeflatedZip([
    { name: "iso-27001/SKILL.md", data: Buffer.from(VALID_SKILL_MD) },
    { name: "iso-27001/references/annex.md", data: Buffer.from("# Annex A\n") },
  ]);

  await handleSkillUpload(
    fakeReq(`Bearer ${token}`, zipBuf, "application/zip"),
    reply,
    resolver,
    VERIFY_OPTS,
    north,
  );

  assert.equal(reply.statusCode, 201);
  assert.equal(north.calls.length, 1);
  assert.equal(north.calls[0].req.name, "my-skill");
});

// ── full-tree forwarding ─────

test("POST /admin/skills/upload — zip with references/ + scripts/ entries forwards a files map with byte-identical content, response carries filePaths", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  const zipBuf = buildStoredZip([
    { name: "SKILL.md", data: Buffer.from(VALID_SKILL_MD) },
    { name: "references/annex.md", data: Buffer.from("# Annex\n") },
    { name: "scripts/run.sh", data: Buffer.from("#!/bin/sh\necho hi\n") },
  ]);

  await handleSkillUpload(
    fakeReq(`Bearer ${token}`, zipBuf, "application/zip"),
    reply,
    resolver,
    VERIFY_OPTS,
    north,
  );

  assert.equal(reply.statusCode, 201);
  assert.equal(north.calls.length, 1);
  const files = north.calls[0].req.files ?? {};
  assert.deepEqual(Object.keys(files).sort(), ["references/annex.md", "scripts/run.sh"]);
  assert.equal(Buffer.from(files["references/annex.md"]).toString("utf8"), "# Annex\n");
  assert.equal(Buffer.from(files["scripts/run.sh"]).toString("utf8"), "#!/bin/sh\necho hi\n");

  const body = reply.body as { bundle: AgentSkillBundle };
  assert.deepEqual(
    body.bundle.filePaths,
    ["LICENSE.txt", "scripts/run.sh"],
    "response must carry the broker's filePaths verbatim, not stripped",
  );
});

// ── Finding 4: multipart/form-data path → 201, broker called ─────────────────

function buildMultipart(boundary: string, fieldname: string, data: Buffer): Buffer {
  const parts: Buffer[] = [];
  parts.push(Buffer.from(`--${boundary}\r\nContent-Disposition: form-data; name="${fieldname}"; filename="SKILL.md"\r\nContent-Type: text/markdown\r\n\r\n`));
  parts.push(data);
  parts.push(Buffer.from(`\r\n--${boundary}--\r\n`));
  return Buffer.concat(parts);
}

test("POST /admin/skills/upload — multipart/form-data with skill part → 201, broker called", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  const boundary = "----TestBoundary12345";
  const multipartBody = buildMultipart(boundary, "skill", Buffer.from(VALID_SKILL_MD));

  await handleSkillUpload(
    fakeReq(`Bearer ${token}`, multipartBody, `multipart/form-data; boundary=${boundary}`),
    reply,
    resolver,
    VERIFY_OPTS,
    north,
  );

  assert.equal(reply.statusCode, 201);
  assert.equal(north.calls.length, 1);
  assert.equal(north.calls[0].req.name, "my-skill");
});

// ── Finding 6: truncated zip header → 400 (fail-loud, not silent accept) ──────

test("POST /admin/skills/upload — truncated zip (name extends past buffer) → 400", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  // Build a valid-looking local file header that claims nameLen=50 but
  // truncates the buffer before the name data — bypasses the old silent-break path.
  const truncated = Buffer.alloc(32);
  truncated.writeUInt32LE(0x04034b50, 0); // valid ZIP signature
  truncated.writeUInt16LE(20, 4);          // version needed
  truncated.writeUInt16LE(0, 6);           // flags
  truncated.writeUInt16LE(0, 8);           // compression: STORED
  truncated.writeUInt32LE(0, 14);          // crc-32
  truncated.writeUInt32LE(5, 18);          // compressed size
  truncated.writeUInt32LE(5, 22);          // uncompressed size
  truncated.writeUInt16LE(50, 26);         // nameLen=50, but buffer only has 2 more bytes
  truncated.writeUInt16LE(0, 28);          // extra field length

  await handleSkillUpload(
    fakeReq(`Bearer ${token}`, truncated, "application/zip"),
    reply,
    resolver,
    VERIFY_OPTS,
    north,
  );

  assert.equal(reply.statusCode, 400);
  assert.ok(
    (reply.body as { error: string }).error.includes("truncated"),
    "error message must mention truncated",
  );
  assert.equal(north.calls.length, 0, "broker must not be called on malformed zip");
});


// ── PUT /admin/skill-bundles/:id tests ───────────────────────────────────────

test("PUT /admin/skill-bundles/:id — valid markdown + bundleId → 201, upsertAgentSkill called with id", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();
  const bundleId = "bbbbbbbb-0000-0000-0000-000000000002";

  await handleSkillUpload(
    fakeReq(`Bearer ${token}`, Buffer.from(VALID_SKILL_MD), "text/markdown"),
    reply,
    resolver,
    VERIFY_OPTS,
    north,
    bundleId,
  );

  assert.equal(reply.statusCode, 201);
  assert.equal(north.calls.length, 1);
  const call = north.calls[0];
  assert.equal(call.method, "upsertAgentSkill");
  assert.equal(call.req.id, bundleId, "id must be the path-supplied bundleId");
  assert.equal(call.req.name, "my-skill");
});

test("PUT /admin/skill-bundles/:id — non-admin caller → 403 (broker rejects PermissionDenied)", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth({ rejectWith: { code: 7, message: "PermissionDenied" } });
  const token = await mintToken(privateKey, false);
  const reply = fakeReply();

  await handleSkillUpload(
    fakeReq(`Bearer ${token}`, Buffer.from(VALID_SKILL_MD), "text/markdown"),
    reply,
    resolver,
    VERIFY_OPTS,
    north,
    "any-id",
  );

  assert.equal(reply.statusCode, 403);
});

// PUT is upsert-by-id: broker ON CONFLICT … DO UPDATE handles both create and update.
// A non-existent id is not an error — it just inserts with that id.
// Assert the real upsert-by-id behavior: broker succeeds and returns the bundle.
test("PUT /admin/skill-bundles/:id — non-existent id → broker upserts (inserts) successfully → 201", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth(); // broker succeeds (no rejectWith)
  const token = await mintToken(privateKey);
  const reply = fakeReply();
  const newId = "cccccccc-0000-0000-0000-000000000003";

  await handleSkillUpload(
    fakeReq(`Bearer ${token}`, Buffer.from(VALID_SKILL_MD), "text/markdown"),
    reply,
    resolver,
    VERIFY_OPTS,
    north,
    newId,
  );

  assert.equal(reply.statusCode, 201);
  assert.equal(north.calls.length, 1);
  assert.equal(north.calls[0].req.id, newId, "id must be the path-supplied id even when the row is new");
  assert.equal(north.calls[0].req.name, "my-skill");
});

// A zip containing scripts/ is accepted on the edit path too — scripts/ is inert.
test("PUT /admin/skill-bundles/:id — zip with scripts/ entry → 201, broker called with id", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  const zipBuf = buildStoredZip([
    { name: "SKILL.md", data: Buffer.from(VALID_SKILL_MD) },
    { name: "scripts/run.sh", data: Buffer.from("#!/bin/sh\necho hi\n") },
  ]);

  await handleSkillUpload(
    fakeReq(`Bearer ${token}`, zipBuf, "application/zip"),
    reply,
    resolver,
    VERIFY_OPTS,
    north,
    "some-valid-id",
  );

  assert.equal(reply.statusCode, 201);
  assert.equal(north.calls.length, 1, "broker must be called — scripts/ is not a rejection");
  assert.equal(north.calls[0].req.id, "some-valid-id");
});

// ── keywords query-param round-trip ──────────────────────────────────────────

test("PUT /admin/skill-bundles/:id — ?keywords= round-trips as trimmed, deduped-of-empties array", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();
  const bundleId = "dddddddd-0000-0000-0000-000000000004";

  await handleSkillUpload(
    fakeReq(`Bearer ${token}`, Buffer.from(VALID_SKILL_MD), "text/markdown", {
      keywords: " billing , invoice ,, refunds ",
    }),
    reply,
    resolver,
    VERIFY_OPTS,
    north,
    bundleId,
  );

  assert.equal(reply.statusCode, 201);
  assert.equal(north.calls.length, 1);
  assert.deepEqual(north.calls[0].req.keywords, ["billing", "invoice", "refunds"]);
});

test("PUT /admin/skill-bundles/:id — no ?keywords= → empty array (full-overwrite semantics)", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  await handleSkillUpload(
    fakeReq(`Bearer ${token}`, Buffer.from(VALID_SKILL_MD), "text/markdown"),
    reply,
    resolver,
    VERIFY_OPTS,
    north,
    "eeeeeeee-0000-0000-0000-000000000005",
  );

  assert.equal(reply.statusCode, 201);
  assert.deepEqual(north.calls[0].req.keywords, []);
});

test("PUT /admin/skill-bundles/:id — too many keywords → 400, broker not called", async () => {
  const { privateKey, jwk } = await makeKey();
  const resolver = await localResolver(jwk);
  const north = makeNorth();
  const token = await mintToken(privateKey);
  const reply = fakeReply();

  const tooMany = Array.from({ length: 33 }, (_, i) => `kw${i}`).join(",");
  await handleSkillUpload(
    fakeReq(`Bearer ${token}`, Buffer.from(VALID_SKILL_MD), "text/markdown", { keywords: tooMany }),
    reply,
    resolver,
    VERIFY_OPTS,
    north,
    "ffffffff-0000-0000-0000-000000000006",
  );

  assert.equal(reply.statusCode, 400);
  assert.equal(north.calls.length, 0);
});

// Unused import kept for reference in the original file; requireUser is exercised
// indirectly via handleSkillUpload (the 401 test proves auth is wired correctly).
void requireUser;
