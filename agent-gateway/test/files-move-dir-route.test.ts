// POST /files/move and POST /files/dir route handlers
//
// WHY: the directory-aware file explorer needs the gateway to forward
// move/mkdir requests with the verified OIDC identity (never trusting body
// fields for tenant/user) and to shape the move response through the same
// fileJson mapper as the other file routes, including the isDir field.
//
// Registers the real registerFilesRoutes (src/routes/files.ts) against a fake
// north client that records the request it received, so a mutation to the
// production route fails these tests — no hand-copied handler (closes the
// files-move-dir-route-test-illusory follow-up).
import { test } from "node:test";
import assert from "node:assert/strict";
import Fastify from "fastify";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import { registerFilesRoutes } from "../src/routes/files.js";
import type { MoveWorkspaceFileResponse, CreateWorkspaceDirResponse } from "../gen/ts/proto/broker.js";
import type { JwksResolver } from "../src/auth/verify.js";

const PERMISSION_DENIED_CODE = 7;

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

// Build the real Fastify app wiring only the files routes, with a north stub
// that records the request it received and is driven by `mode`.
async function buildApp(mode: "ok" | "denied" = "ok") {
  const { privateKey, jwk } = await makeKey();
  const jwksResolver = await localResolver(jwk);
  const token = await mintToken(privateKey);

  const app = Fastify({ logger: false });
  const calls: { moveWorkspaceFile?: unknown; createWorkspaceDir?: unknown } = {};

  const north = {
    async moveWorkspaceFile(req: unknown, _token?: string): Promise<MoveWorkspaceFileResponse> {
      calls.moveWorkspaceFile = req;
      if (mode === "denied") {
        throw Object.assign(new Error("PermissionDenied"), { code: PERMISSION_DENIED_CODE });
      }
      return {
        file: { path: "folder/moved.txt", sizeBytes: 42, modifiedAt: new Date("2026-01-01T00:00:00Z"), isDir: false },
      };
    },
    async createWorkspaceDir(req: unknown, _token?: string): Promise<CreateWorkspaceDirResponse> {
      calls.createWorkspaceDir = req;
      if (mode === "denied") {
        throw Object.assign(new Error("PermissionDenied"), { code: PERMISSION_DENIED_CODE });
      }
      return { success: true };
    },
    readWorkspaceFile: (): never => { throw new Error("not used in this test"); },
    uploadWorkspaceFile: (): never => { throw new Error("not used in this test"); },
    deleteWorkspaceFile: (): never => { throw new Error("not used in this test"); },
  };

  registerFilesRoutes(app, { clients: { north }, jwksResolver, verifyOpts: VERIFY_OPTS });

  return { app, calls, token };
}

test("POST /files/move — forwards identity + from/to, returns fileJson-shaped file", async () => {
  const { app, calls, token } = await buildApp("ok");
  await app.ready();

  const res = await app.inject({
    method: "POST",
    url: "/files/move",
    headers: { authorization: `Bearer ${token}` },
    payload: { from: "old/name.txt", to: "folder/moved.txt" },
  });

  assert.equal(res.statusCode, 200);
  assert.deepEqual(calls.moveWorkspaceFile, {
    tenantId: "aikonos-dev",
    userId: "alice@example.com",
    fromPath: "old/name.txt",
    toPath: "folder/moved.txt",
  });
  const body = JSON.parse(res.body) as { file: { path: string; size: number; isDir: boolean } };
  assert.equal(body.file.path, "folder/moved.txt");
  assert.equal(body.file.size, 42);
  assert.equal(body.file.isDir, false, "isDir must flow through fileJson");

  await app.close();
});

test("POST /files/move — broker denial surfaces as non-200 with error", async () => {
  const { app, token } = await buildApp("denied");
  await app.ready();

  const res = await app.inject({
    method: "POST",
    url: "/files/move",
    headers: { authorization: `Bearer ${token}` },
    payload: { from: "a", to: "b" },
  });

  assert.equal(res.statusCode, 403);
  const body = JSON.parse(res.body) as { error: string };
  assert.ok(body.error, "error message must be present");

  await app.close();
});

test("POST /files/dir — forwards identity + path, returns { success }", async () => {
  const { app, calls, token } = await buildApp("ok");
  await app.ready();

  const res = await app.inject({
    method: "POST",
    url: "/files/dir",
    headers: { authorization: `Bearer ${token}` },
    payload: { path: "newfolder" },
  });

  assert.equal(res.statusCode, 200);
  assert.deepEqual(calls.createWorkspaceDir, {
    tenantId: "aikonos-dev",
    userId: "alice@example.com",
    path: "newfolder",
  });
  const body = JSON.parse(res.body) as { success: boolean };
  assert.equal(body.success, true);

  await app.close();
});

test("POST /files/dir — broker denial surfaces as non-200 with error", async () => {
  const { app, token } = await buildApp("denied");
  await app.ready();

  const res = await app.inject({
    method: "POST",
    url: "/files/dir",
    headers: { authorization: `Bearer ${token}` },
    payload: { path: "newfolder" },
  });

  assert.equal(res.statusCode, 403);
  const body = JSON.parse(res.body) as { error: string };
  assert.ok(body.error, "error message must be present");

  await app.close();
});
