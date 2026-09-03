// F9 agent-bound workflows — gateway half.
//
// Covers the two pure decision helpers and the run-route wiring:
//   - boundAgentUuid: which session-agent-id forms may bind a workflow.
//   - runIdentityFor: bound → south/ownerGrant identity; unbound → token identity.
//   - POST /workflows/:lineageId/run: calls BeginWorkflowRun, then drives the run
//     under the identity it dictates; PermissionDenied surfaces as HTTP 403.
import { test } from "node:test";
import assert from "node:assert/strict";
import Fastify from "fastify";
import pino from "pino";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import { boundAgentUuid } from "../src/broker/governance.js";
import { registerWorkflowRoutes, runIdentityFor } from "../src/routes/workflows.js";
import { BrokerClients } from "../src/broker/clients.js";
import { NorthClient } from "../src/broker/north.js";
import { SouthClient } from "../src/broker/south.js";
import type { Config } from "../src/config.js";
import type { JwksResolver } from "../src/auth/verify.js";

// ── boundAgentUuid ─────────────────────────────────────────────────────────────

const UUID = "550e8400-e29b-41d4-a716-446655440000";

test('boundAgentUuid: "agent:<uuid>" → bare uuid', () => {
  assert.equal(boundAgentUuid(`agent:${UUID}`), UUID);
});

test("boundAgentUuid: bare uuid → same uuid", () => {
  assert.equal(boundAgentUuid(UUID), UUID);
});

test('boundAgentUuid: synthetic "<user>-agent" → "" (never binds)', () => {
  assert.equal(boundAgentUuid("alice-agent"), "");
});

test('boundAgentUuid: "agent:not-a-uuid" → ""', () => {
  assert.equal(boundAgentUuid("agent:not-a-uuid"), "");
});

test('boundAgentUuid: "" → ""', () => {
  assert.equal(boundAgentUuid(""), "");
});

// ── runIdentityFor ───────────────────────────────────────────────────────────

const principal = { token: "bearer-tok", tenant: "aikonos-dev", sub: "alice@example.com" };

test("runIdentityFor: bound → south identity (no token, ownerGrant + agentId set)", () => {
  const id = runIdentityFor(principal, { ownerGrant: "grant-x", boundAgentId: UUID });
  assert.equal(id.token, undefined, "bound run must carry NO token (forces the south/grant path)");
  assert.equal(id.ownerGrant, "grant-x");
  assert.equal(id.agentId, UUID);
  assert.equal(id.tenantId, "aikonos-dev");
  assert.equal(id.userId, "alice@example.com");
});

test("runIdentityFor: unbound → token identity with agentId = sub", () => {
  const id = runIdentityFor(principal, { ownerGrant: "", boundAgentId: "" });
  assert.equal(id.token, "bearer-tok");
  assert.equal(id.ownerGrant, undefined);
  assert.equal(id.agentId, "alice@example.com", "unbound personal run uses agentId=sub (unused on the token path)");
  assert.equal(id.tenantId, "aikonos-dev");
  assert.equal(id.userId, "alice@example.com");
});

// ── POST /workflows/:lineageId/run route wiring ──────────────────────────────

const PERMISSION_DENIED_CODE = 7;
const EMPTY_DEF = JSON.stringify({
  apiVersion: "aikonos.com/v1",
  kind: "Workflow",
  metadata: { name: "wf", visibility: { kind: "private" } },
  steps: [],
});

interface BeginCall {
  req: { tenantId: string; ownerUserId: string; lineageId: string };
}
interface GetCall {
  req: { ownerGrant: string };
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

function fakeConfig(): Config {
  return {
    openrouterApiKey: "", llmModel: "", brokerNorthAddr: "", brokerSouthAddr: "",
    brokerServerName: "", tlsCert: "", tlsKey: "", tlsCa: "", gatewaySpiffeId: "",
    port: 8080, defaultTenantId: "", oidcIssuer: "", oidcJwksUrl: "", oidcAudience: "",
    oidcSubjectClaim: "sub", oidcTenantClaim: "tenant_id", schedulerEnabled: false,
    schedulerTickMs: 30000, schedulerClaimLimit: 10, schedulerRunTimeoutMs: 180000,
    agentForUserOverrides: {}, externalPort: 8090, externalCorsOrigins: [],
    externalRateLimit: 60, threadTtlMs: 1800000, maxChildren: 32, childTtlMs: 1800000,
    natsUrl: "nats://nats:4222", auditSubject: "aikonos.audit.>", egressTimeoutMs: 120000,
    brokerTimeoutMs: 30000, rateLimitBreakerThreshold: 5, workflowReasonMaxTokens: 2048, maxLlmCallsPerRun: 100, approvalTimeoutMs: 900000, memorySemanticRecall: true, memoryEmbedTimeoutMs: 10000, subagentMaxWidth: 3, subagentBranchTimeoutMs: 180000,
  };
}

// Builds the real Fastify app + registerWorkflowRoutes over fake north/south
// clients that record the requests they saw. `mode` drives beginWorkflowRun.
async function buildApp(mode: "bound" | "unbound" | "denied") {
  const { privateKey, jwk } = await makeKey();
  const jwksResolver = await localResolver(jwk);
  const token = await mintToken(privateKey);

  const beginCalls: BeginCall[] = [];
  const northGetCalls: GetCall[] = [];
  const southGetCalls: GetCall[] = [];

  const app = Fastify({ logger: false });
  const clients: BrokerClients = Object.create(BrokerClients.prototype);

  const north: NorthClient = Object.create(NorthClient.prototype);
  Object.assign(north, {
    beginWorkflowRun(req: BeginCall["req"], _token?: string) {
      beginCalls.push({ req });
      if (mode === "denied") {
        throw Object.assign(new Error("you do not have access to agent " + UUID), {
          code: PERMISSION_DENIED_CODE,
        });
      }
      if (mode === "bound") return Promise.resolve({ ownerGrant: "grant-x", boundAgentId: UUID });
      return Promise.resolve({ ownerGrant: "", boundAgentId: "" });
    },
    getWorkflow(req: GetCall["req"], _token?: string) {
      northGetCalls.push({ req });
      return Promise.resolve({ definitionJson: EMPTY_DEF, version: 1, boundAgentId: "" });
    },
  });

  const south: SouthClient = Object.create(SouthClient.prototype);
  Object.assign(south, {
    getWorkflow(req: GetCall["req"]) {
      southGetCalls.push({ req });
      return Promise.resolve({ definitionJson: EMPTY_DEF, version: 1, boundAgentId: UUID });
    },
  });

  clients.north = north;
  clients.south = south;

  registerWorkflowRoutes(app, {
    clients,
    jwksResolver,
    verifyOpts: VERIFY_OPTS,
    cfg: fakeConfig(),
    log: pino({ level: "silent" }),
  });

  return { app, token, beginCalls, northGetCalls, southGetCalls };
}

test("POST /workflows/:lineageId/run — bound workflow runs under the south/ownerGrant identity", async () => {
  const { app, token, beginCalls, northGetCalls, southGetCalls } = await buildApp("bound");
  await app.ready();

  const res = await app.inject({
    method: "POST",
    url: "/workflows/lin-1/run",
    headers: { authorization: `Bearer ${token}` },
    payload: { inputs: {} },
  });

  assert.equal(res.statusCode, 200);
  assert.equal(beginCalls.length, 1, "beginWorkflowRun must be called");
  assert.deepEqual(
    beginCalls[0].req,
    { tenantId: "aikonos-dev", ownerUserId: "alice@example.com", lineageId: "lin-1" },
    "beginWorkflowRun must receive tenant/owner/lineage from the principal",
  );
  // Bound identity has NO token → the bridge takes the SOUTH path.
  assert.equal(southGetCalls.length, 1, "bound run must fetch the workflow via the south/grant path");
  assert.equal(southGetCalls[0].req.ownerGrant, "grant-x", "south getWorkflow must carry the broker owner grant");
  assert.equal(northGetCalls.length, 0, "bound run must NOT use the north/token path");

  await app.close();
});

test("POST /workflows/:lineageId/run — unbound workflow runs on the personal token path", async () => {
  const { app, token, beginCalls, northGetCalls, southGetCalls } = await buildApp("unbound");
  await app.ready();

  const res = await app.inject({
    method: "POST",
    url: "/workflows/lin-2/run",
    headers: { authorization: `Bearer ${token}` },
    payload: {},
  });

  assert.equal(res.statusCode, 200);
  assert.equal(beginCalls.length, 1, "beginWorkflowRun must be called even for personal workflows");
  assert.equal(northGetCalls.length, 1, "unbound run must fetch the workflow via the north/token path");
  assert.equal(southGetCalls.length, 0, "unbound run must NOT use the south path");

  await app.close();
});

test("POST /workflows/:lineageId/run — beginWorkflowRun PermissionDenied → 403 with message", async () => {
  // WHY: the caller may not operate the bound agent — the broker denies and the
  // gateway must surface it as 403 so the webui modal shows the reason, not a
  // false success.
  const { app, token, beginCalls } = await buildApp("denied");
  await app.ready();

  const res = await app.inject({
    method: "POST",
    url: "/workflows/lin-3/run",
    headers: { authorization: `Bearer ${token}` },
    payload: {},
  });

  assert.equal(res.statusCode, 403, "PermissionDenied must map to HTTP 403");
  const body = JSON.parse(res.body) as { ok: boolean; error: string };
  assert.equal(body.ok, false);
  assert.ok(body.error.includes("access to agent"), `error message must reach the client; got: ${body.error}`);
  assert.equal(beginCalls.length, 1);

  await app.close();
});
