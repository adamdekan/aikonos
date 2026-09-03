// GET /files route handler
//
// WHY: CP2 (scoped-file-listing) adds ?dir=/?recursive= passthrough to the
// north listWorkspaceFiles call. Absent params must still produce the legacy
// request shape (path: "", recursive: false) byte-identical to today, and the
// includeHidden dot-segment filter must keep applying to whatever the north
// client returns, scoped or not.
//
// Registers the real registerFilesListRoute (src/routes/files-list.ts) against
// a fake north client that records the request it received, so a mutation to
// the production route fails these tests — no hand-copied handler.
import { test } from "node:test";
import assert from "node:assert/strict";
import Fastify from "fastify";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import { registerFilesListRoute } from "../src/routes/files-list.js";
import type { WorkspaceFile, ListWorkspaceFilesResponse } from "../gen/ts/proto/broker.js";
import type { JwksResolver } from "../src/auth/verify.js";

// ── Real bearer auth via a local JWKS resolver (mirrors admin-keys.test.ts) ──

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

async function mintToken(privateKey: Awaited<ReturnType<typeof generateKeyPair>>["privateKey"]) {
  return new SignJWT({
    sub: "alice@example.com",
    email: "alice@example.com",
    tenant_id: "aikonos-dev",
  })
    .setProtectedHeader({ alg: "RS256", kid: "k1" })
    .setIssuer(VERIFY_OPTS.issuer)
    .setAudience(VERIFY_OPTS.audience)
    .setIssuedAt()
    .setExpirationTime("1h")
    .sign(privateKey);
}

async function buildApp(files: WorkspaceFile[]) {
  const { privateKey, jwk } = await makeKey();
  const jwksResolver = await localResolver(jwk);
  const token = await mintToken(privateKey);

  const app = Fastify({ logger: false });
  const calls: { listWorkspaceFiles?: unknown } = {};

  const north = {
    async listWorkspaceFiles(req: unknown, _token?: string): Promise<ListWorkspaceFilesResponse> {
      calls.listWorkspaceFiles = req;
      return { files };
    },
  };

  registerFilesListRoute(app, { clients: { north }, jwksResolver, verifyOpts: VERIFY_OPTS });

  return { app, calls, token };
}

test("GET /files — no query params sends legacy request shape", async () => {
  const { app, calls, token } = await buildApp([]);
  await app.ready();

  const res = await app.inject({
    method: "GET",
    url: "/files",
    headers: { authorization: `Bearer ${token}` },
  });

  assert.equal(res.statusCode, 200);
  assert.deepEqual(calls.listWorkspaceFiles, {
    tenantId: "aikonos-dev",
    userId: "alice@example.com",
    path: "",
    recursive: false,
  });

  await app.close();
});

test("GET /files — forwards dir and recursive=true to the north request", async () => {
  const { app, calls, token } = await buildApp([]);
  await app.ready();

  const res = await app.inject({
    method: "GET",
    url: "/files?dir=sub/folder&recursive=true",
    headers: { authorization: `Bearer ${token}` },
  });

  assert.equal(res.statusCode, 200);
  assert.deepEqual(calls.listWorkspaceFiles, {
    tenantId: "aikonos-dev",
    userId: "alice@example.com",
    path: "sub/folder",
    recursive: true,
  });

  await app.close();
});

test("GET /files — recursive=1 also parses as true (existing includeHidden idiom)", async () => {
  const { app, calls, token } = await buildApp([]);
  await app.ready();

  await app.inject({
    method: "GET",
    url: "/files?dir=x&recursive=1",
    headers: { authorization: `Bearer ${token}` },
  });

  assert.deepEqual(calls.listWorkspaceFiles, {
    tenantId: "aikonos-dev",
    userId: "alice@example.com",
    path: "x",
    recursive: true,
  });

  await app.close();
});

test("GET /files — filterFiles still applied to scoped results (includeHidden=false drops dot segments)", async () => {
  const { app, token } = await buildApp([
    { path: "sub/visible.txt", sizeBytes: 1, isDir: false },
    { path: "sub/.hidden.txt", sizeBytes: 2, isDir: false },
  ]);
  await app.ready();

  const res = await app.inject({
    method: "GET",
    url: "/files?dir=sub",
    headers: { authorization: `Bearer ${token}` },
  });

  assert.equal(res.statusCode, 200);
  const body = JSON.parse(res.body) as { files: { path: string }[] };
  assert.deepEqual(
    body.files.map((f) => f.path),
    ["sub/visible.txt"],
  );

  await app.close();
});

test("GET /files — includeHidden=true keeps dot segments in scoped results", async () => {
  const { app, token } = await buildApp([
    { path: "sub/visible.txt", sizeBytes: 1, isDir: false },
    { path: "sub/.hidden.txt", sizeBytes: 2, isDir: false },
  ]);
  await app.ready();

  const res = await app.inject({
    method: "GET",
    url: "/files?dir=sub&includeHidden=true",
    headers: { authorization: `Bearer ${token}` },
  });

  const body = JSON.parse(res.body) as { files: { path: string }[] };
  assert.deepEqual(
    body.files.map((f) => f.path).sort(),
    ["sub/.hidden.txt", "sub/visible.txt"],
  );

  await app.close();
});
