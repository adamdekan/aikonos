// GET /workflows/:lineageId/versions handler
//
// WHY: the version-switcher UI calls this route to list all versions for a
// lineage with their approval_state so the user can pick one to pin.
// Covers: skill-deny → PermissionDenied response; allow → versions forwarded;
// store error → 400.
//
// Registers the real registerWorkflowRoutes (src/routes/workflows.ts) against a
// fake north client, so a mutation to the production route fails this test —
// no hand-copied handler.
import { test } from "node:test";
import assert from "node:assert/strict";
import Fastify from "fastify";
import pino from "pino";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import { registerWorkflowRoutes } from "../src/routes/workflows.js";
import { BrokerClients } from "../src/broker/clients.js";
import { NorthClient } from "../src/broker/north.js";
import type { Config } from "../src/config.js";
import type { JwksResolver } from "../src/auth/verify.js";

const PERMISSION_DENIED_CODE = 7;
const INTERNAL_CODE = 13;

interface VersionItem {
  version: number;
  approvalState: string;
  createdAt: string;
}

interface ListVersionsResponse {
  items: VersionItem[];
}

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

function fakeConfig(): Config {
  return {
    openrouterApiKey: "", llmModel: "", brokerNorthAddr: "", brokerSouthAddr: "",
    brokerServerName: "", tlsCert: "", tlsKey: "", tlsCa: "", gatewaySpiffeId: "",
    port: 8080, defaultTenantId: "", oidcIssuer: "", oidcJwksUrl: "", oidcAudience: "",
    oidcSubjectClaim: "sub", oidcTenantClaim: "tenant_id", schedulerEnabled: false,
    schedulerTickMs: 30000, schedulerClaimLimit: 10, schedulerRunTimeoutMs: 180000,
    agentForUserOverrides: {}, externalPort: 8090, externalCorsOrigins: [],
    externalRateLimit: 60, threadTtlMs: 1800000, maxChildren: 32, childTtlMs: 1800000, natsUrl: "nats://nats:4222", auditSubject: "aikonos.audit.>", egressTimeoutMs: 120000, brokerTimeoutMs: 30000, rateLimitBreakerThreshold: 5, workflowReasonMaxTokens: 2048, maxLlmCallsPerRun: 100, approvalTimeoutMs: 900000, memorySemanticRecall: true, memoryEmbedTimeoutMs: 10000, subagentMaxWidth: 3, subagentBranchTimeoutMs: 180000,
  };
}

// Object.create(NorthClient.prototype) + Object.assign avoids the real
// constructor (which dials a gRPC channel) while satisfying the nominal class
// type check without a cast.
async function buildApp(mode: "allowed" | "denied" | "error") {
  const { privateKey, jwk } = await makeKey();
  const jwksResolver = await localResolver(jwk);
  const token = await mintToken(privateKey);

  const app = Fastify({ logger: false });
  const clients: BrokerClients = Object.create(BrokerClients.prototype);
  const north: NorthClient = Object.create(NorthClient.prototype);
  Object.assign(north, {
    async listWorkflowVersions(_req: unknown, _token?: string): Promise<ListVersionsResponse> {
      if (mode === "denied") {
        throw Object.assign(new Error("PermissionDenied"), { code: PERMISSION_DENIED_CODE });
      }
      if (mode === "error") {
        throw Object.assign(new Error("db connection lost"), { code: INTERNAL_CODE });
      }
      return {
        items: [
          { version: 2, approvalState: "approved", createdAt: "2025-01-02T00:00:00Z" },
          { version: 1, approvalState: "approved", createdAt: "2025-01-01T00:00:00Z" },
        ],
      };
    },
  });
  clients.north = north;

  registerWorkflowRoutes(app, {
    clients,
    jwksResolver,
    verifyOpts: VERIFY_OPTS,
    cfg: fakeConfig(),
    log: pino({ level: "silent" }),
  });

  return { app, token };
}

test("GET /workflows/:lineageId/versions — skill denied → 403 + empty versions", async () => {
  // WHY: skill gate must produce a 403 response and an empty versions array so
  // the UI can surface an error rather than silently showing no versions.
  const { app, token } = await buildApp("denied");
  await app.ready();

  const res = await app.inject({
    method: "GET",
    url: "/workflows/lineage-1/versions",
    headers: { authorization: `Bearer ${token}` },
  });

  assert.equal(res.statusCode, 403, "expect 403 PermissionDenied");
  const body = JSON.parse(res.body) as { versions: unknown[]; error: string };
  assert.deepEqual(body.versions, [], "versions must be empty on error");
  assert.ok(body.error, "error message must be present");

  await app.close();
});

test("GET /workflows/:lineageId/versions — skill allowed → 200 + version items", async () => {
  // WHY: happy path — items returned from north are forwarded as-is under the
  // `versions` key; each item has version + approvalState + createdAt.
  const { app, token } = await buildApp("allowed");
  await app.ready();

  const res = await app.inject({
    method: "GET",
    url: "/workflows/lineage-1/versions",
    headers: { authorization: `Bearer ${token}` },
  });

  assert.equal(res.statusCode, 200);
  const body = JSON.parse(res.body) as {
    versions: Array<{ version: number; approvalState: string; createdAt: string }>;
  };
  assert.equal(body.versions.length, 2, "expect 2 version rows");
  assert.equal(body.versions[0]?.version, 2, "newest first (version 2)");
  assert.equal(body.versions[0]?.approvalState, "approved");
  assert.ok(body.versions[0]?.createdAt, "createdAt must be present");

  await app.close();
});

test("GET /workflows/:lineageId/versions — store error → 400 + empty versions", async () => {
  // WHY: a broker-side Internal error must not yield a 200 with empty list;
  // the gateway must surface a non-2xx status so the UI can show an error.
  const { app, token } = await buildApp("error");
  await app.ready();

  const res = await app.inject({
    method: "GET",
    url: "/workflows/lineage-1/versions",
    headers: { authorization: `Bearer ${token}` },
  });

  assert.notEqual(res.statusCode, 200, "store error must not produce 200");
  const body = JSON.parse(res.body) as { versions: unknown[]; error: string };
  assert.deepEqual(body.versions, [], "versions must be empty on error");
  assert.ok(body.error, "error message must be present");

  await app.close();
});
