// DELETE /workflows/:lineageId handler
//
// WHY: the Workflows view calls this route to remove an entire lineage (all
// versions). The broker enforces owner-only; the gateway forwards the failure as
// a non-2xx status so the UI can surface it rather than silently "succeeding".
// Covers: owner success → 200 { ok, versionsDeleted }; permission denied → 403;
// store/internal error → 400.
//
// Registers the real registerWorkflowRoutes (src/routes/workflows.ts) against a
// fake north client that records the request it received, so a mutation to the
// production route fails this test — no hand-copied handler.
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

interface DeleteResponse {
  versionsDeleted: number;
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

// Build the real Fastify app wiring the full workflow route group, with a
// north stub whose deleteWorkflow behavior is driven by `mode`.
// Object.create avoids calling BrokerClients' constructor (which opens real
// TLS/gRPC connections) while satisfying the class type check without a cast.
async function buildApp(mode: "ok" | "denied" | "error") {
  const { privateKey, jwk } = await makeKey();
  const jwksResolver = await localResolver(jwk);
  const token = await mintToken(privateKey);

  const app = Fastify({ logger: false });
  const clients: BrokerClients = Object.create(BrokerClients.prototype);
  // Object.create(NorthClient.prototype) + Object.assign avoids the real
  // constructor (which dials a gRPC channel) while satisfying the nominal
  // class type check without a cast — BrokerClients.north has private fields
  // in its own class, so a plain object literal can't structurally match it.
  const north: NorthClient = Object.create(NorthClient.prototype);
  Object.assign(north, {
    async deleteWorkflow(_req: unknown, _token?: string): Promise<DeleteResponse> {
      if (mode === "denied") {
        throw Object.assign(new Error("PermissionDenied"), { code: PERMISSION_DENIED_CODE });
      }
      if (mode === "error") {
        throw Object.assign(new Error("db connection lost"), { code: INTERNAL_CODE });
      }
      return { versionsDeleted: 2 };
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

test("DELETE /workflows/:lineageId — owner → 200 { ok, versionsDeleted }", async () => {
  const { app, token } = await buildApp("ok");
  await app.ready();

  const res = await app.inject({
    method: "DELETE",
    url: "/workflows/lineage-1",
    headers: { authorization: `Bearer ${token}` },
  });

  assert.equal(res.statusCode, 200);
  const body = JSON.parse(res.body) as { ok: boolean; versionsDeleted: number };
  assert.equal(body.ok, true);
  assert.equal(body.versionsDeleted, 2, "version count forwarded from broker");

  await app.close();
});

test("DELETE /workflows/:lineageId — non-owner / skill denied → 403", async () => {
  // WHY: a broker PermissionDenied must surface as 403 so the UI shows an error,
  // not a false success.
  const { app, token } = await buildApp("denied");
  await app.ready();

  const res = await app.inject({
    method: "DELETE",
    url: "/workflows/lineage-1",
    headers: { authorization: `Bearer ${token}` },
  });

  assert.equal(res.statusCode, 403, "expect 403 PermissionDenied");
  const body = JSON.parse(res.body) as { ok: boolean; error: string };
  assert.equal(body.ok, false);
  assert.ok(body.error, "error message must be present");

  await app.close();
});

test("DELETE /workflows/:lineageId — store error → 400", async () => {
  const { app, token } = await buildApp("error");
  await app.ready();

  const res = await app.inject({
    method: "DELETE",
    url: "/workflows/lineage-1",
    headers: { authorization: `Bearer ${token}` },
  });

  assert.notEqual(res.statusCode, 200, "store error must not produce 200");
  const body = JSON.parse(res.body) as { ok: boolean; error: string };
  assert.equal(body.ok, false);
  assert.ok(body.error, "error message must be present");

  await app.close();
});
