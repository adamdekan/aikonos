// GET/PUT /workspace/backend + GET /workspace/onedrive/folders route handlers
//.
//
// Registers the real registerWorkspacePrefsRoutes (src/routes/workspace-prefs.ts)
// against a fake north client that records the request it received, so a
// mutation to the production route fails these tests — no hand-copied handler.
import { test } from "node:test";
import assert from "node:assert/strict";
import Fastify from "fastify";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import { registerWorkspacePrefsRoutes } from "../src/routes/workspace-prefs.js";
import type {
  GetWorkspaceBackendResponse,
  SetWorkspaceBackendResponse,
  ListOneDriveFoldersResponse,
} from "../gen/ts/proto/broker.js";
import type { JwksResolver } from "../src/auth/verify.js";

// ── Real bearer auth via a local JWKS resolver (mirrors files-list-route.test.ts) ──

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

async function buildApp() {
  const { privateKey, jwk } = await makeKey();
  const jwksResolver = await localResolver(jwk);
  const token = await mintToken(privateKey);

  const app = Fastify({ logger: false });
  const calls: {
    getWorkspaceBackend?: unknown;
    setWorkspaceBackend?: unknown;
    listOneDriveFolders?: unknown;
  } = {};
  let getWorkspaceBackendResponse: GetWorkspaceBackendResponse = {
    pref: { backend: "local", onedriveFolderPath: "" },
    onedriveAvailable: false,
    onedriveStatus: "",
  };
  let setWorkspaceBackendResponse: SetWorkspaceBackendResponse = {
    pref: { backend: "onedrive", onedriveFolderPath: "Apps/Aikonos" },
  };
  let listOneDriveFoldersResponse: ListOneDriveFoldersResponse = { folders: [] };
  let setWorkspaceBackendErr: Error | undefined;
  let listOneDriveFoldersErr: Error | undefined;

  const north = {
    async getWorkspaceBackend(req: unknown, _token?: string): Promise<GetWorkspaceBackendResponse> {
      calls.getWorkspaceBackend = req;
      return getWorkspaceBackendResponse;
    },
    async setWorkspaceBackend(req: unknown, _token?: string): Promise<SetWorkspaceBackendResponse> {
      calls.setWorkspaceBackend = req;
      if (setWorkspaceBackendErr) throw setWorkspaceBackendErr;
      return setWorkspaceBackendResponse;
    },
    async listOneDriveFolders(req: unknown, _token?: string): Promise<ListOneDriveFoldersResponse> {
      calls.listOneDriveFolders = req;
      if (listOneDriveFoldersErr) throw listOneDriveFoldersErr;
      return listOneDriveFoldersResponse;
    },
  };

  registerWorkspacePrefsRoutes(app, { clients: { north }, jwksResolver, verifyOpts: VERIFY_OPTS });

  return {
    app,
    calls,
    token,
    setGetWorkspaceBackendResponse: (r: GetWorkspaceBackendResponse) => {
      getWorkspaceBackendResponse = r;
    },
    setSetWorkspaceBackendResponse: (r: SetWorkspaceBackendResponse) => {
      setWorkspaceBackendResponse = r;
    },
    setListOneDriveFoldersResponse: (r: ListOneDriveFoldersResponse) => {
      listOneDriveFoldersResponse = r;
    },
    setSetWorkspaceBackendErr: (e: Error) => {
      setWorkspaceBackendErr = e;
    },
    setListOneDriveFoldersErr: (e: Error) => {
      listOneDriveFoldersErr = e;
    },
  };
}

test("GET /workspace/backend — forwards identity + bearer, returns broker response", async () => {
  const { app, calls, token, setGetWorkspaceBackendResponse } = await buildApp();
  await app.ready();
  setGetWorkspaceBackendResponse({
    pref: { backend: "onedrive", onedriveFolderPath: "Apps/Aikonos" },
    onedriveAvailable: true,
    onedriveStatus: "connected",
  });

  const res = await app.inject({
    method: "GET",
    url: "/workspace/backend",
    headers: { authorization: `Bearer ${token}` },
  });

  assert.equal(res.statusCode, 200);
  assert.deepEqual(calls.getWorkspaceBackend, { tenantId: "aikonos-dev", userId: "alice@example.com" });
  const body = JSON.parse(res.body);
  assert.deepEqual(body, {
    pref: { backend: "onedrive", onedriveFolderPath: "Apps/Aikonos" },
    onedriveAvailable: true,
    onedriveStatus: "connected",
  });

  await app.close();
});

test("PUT /workspace/backend — forwards backend + onedriveFolderPath from the body", async () => {
  const { app, calls, token } = await buildApp();
  await app.ready();

  const res = await app.inject({
    method: "PUT",
    url: "/workspace/backend",
    headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
    payload: { backend: "onedrive", onedriveFolderPath: "Reports/2026" },
  });

  assert.equal(res.statusCode, 200);
  assert.deepEqual(calls.setWorkspaceBackend, {
    tenantId: "aikonos-dev",
    userId: "alice@example.com",
    backend: "onedrive",
    onedriveFolderPath: "Reports/2026",
  });

  await app.close();
});

test("PUT /workspace/backend — error maps via the existing grpcToHttp path", async () => {
  const { app, token, setSetWorkspaceBackendErr } = await buildApp();
  await app.ready();
  const err = Object.assign(new Error("backend must be \"local\" or \"onedrive\""), { code: 3 }); // INVALID_ARGUMENT
  setSetWorkspaceBackendErr(err);

  const res = await app.inject({
    method: "PUT",
    url: "/workspace/backend",
    headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
    payload: { backend: "dropbox" },
  });

  assert.equal(res.statusCode, 400);

  await app.close();
});

test("GET /workspace/onedrive/folders — forwards dir as path, defaults to empty", async () => {
  const { app, calls, token } = await buildApp();
  await app.ready();

  const res = await app.inject({
    method: "GET",
    url: "/workspace/onedrive/folders",
    headers: { authorization: `Bearer ${token}` },
  });

  assert.equal(res.statusCode, 200);
  assert.deepEqual(calls.listOneDriveFolders, { tenantId: "aikonos-dev", userId: "alice@example.com", path: "" });

  await app.close();
});

test("GET /workspace/onedrive/folders?dir=Reports — forwards dir as path", async () => {
  const { app, calls, token, setListOneDriveFoldersResponse } = await buildApp();
  await app.ready();
  setListOneDriveFoldersResponse({ folders: [{ name: "2026", path: "Reports/2026" }] });

  const res = await app.inject({
    method: "GET",
    url: "/workspace/onedrive/folders?dir=Reports",
    headers: { authorization: `Bearer ${token}` },
  });

  assert.equal(res.statusCode, 200);
  assert.deepEqual(calls.listOneDriveFolders, { tenantId: "aikonos-dev", userId: "alice@example.com", path: "Reports" });
  const body = JSON.parse(res.body);
  assert.deepEqual(body, { folders: [{ name: "2026", path: "Reports/2026" }] });

  await app.close();
});

test("GET /workspace/onedrive/folders — error maps to empty folders body via sendError", async () => {
  const { app, token, setListOneDriveFoldersErr } = await buildApp();
  await app.ready();
  const err = Object.assign(new Error("OneDrive is not configured for your organization"), { code: 9 }); // FAILED_PRECONDITION
  setListOneDriveFoldersErr(err);

  const res = await app.inject({
    method: "GET",
    url: "/workspace/onedrive/folders",
    headers: { authorization: `Bearer ${token}` },
  });

  assert.equal(res.statusCode, 409);

  await app.close();
});
