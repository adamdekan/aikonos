// GET /admin/agents/inventory — the AG-14 agent inventory export.
//
// Registers the real registerAdminRoutes against a fake north client.
import { test } from "node:test";
import assert from "node:assert/strict";
import Fastify from "fastify";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";
import * as grpc from "@grpc/grpc-js";

import { registerAdminRoutes, inventoryCsv, inventoryRow } from "../src/routes/admin.js";
import { BrokerClients } from "../src/broker/clients.js";
import { NorthClient } from "../src/broker/north.js";
import type { JwksResolver } from "../src/auth/verify.js";
import type { ExportAgentInventoryResponse } from "../gen/ts/proto/broker.js";

const VERIFY_OPTS = {
  issuer: "http://localhost:18080/realms/aikonos",
  audience: "aikonos-broker",
};

const ENTRY = {
  agent: {
    id: "a1",
    name: "=cmd|' /c calc'!A1",
    status: "active",
    owners: ["user:alice", "group:ops#member"],
    serviceAgent: true,
    gatewayEnabled: true,
    orphaned: false,
    isDefault: false,
    mcpServers: ["jira"],
    mcpTools: ["mcp:jira:search", "mcp:jira:get"],
    skills: ["web.fetch"],
    createdBy: "admin@example.com",
    createdAt: new Date("2026-01-02T03:04:05Z"),
  },
  apiKeys: 2,
  publicKeys: 1,
  nearestKeyExpiry: new Date("2026-11-30T00:00:00Z"),
  lastUsedAt: undefined,
} as unknown as ExportAgentInventoryResponse["agents"][number];

async function buildApp(response: () => Promise<ExportAgentInventoryResponse>) {
  const { publicKey, privateKey } = await generateKeyPair("RS256");
  const jwk: JWK = { ...(await exportJWK(publicKey)), kid: "k1", alg: "RS256", use: "sig" };
  const key = await importJWK(jwk, "RS256");
  const jwksResolver: JwksResolver = () => Promise.resolve(key);
  const token = await new SignJWT({ sub: "admin@example.com", email: "admin@example.com", tenant_id: "11111111-1111-1111-1111-111111111111" })
    .setProtectedHeader({ alg: "RS256", kid: "k1" })
    .setIssuer(VERIFY_OPTS.issuer)
    .setAudience(VERIFY_OPTS.audience)
    .setIssuedAt()
    .setExpirationTime("1h")
    .sign(privateKey);

  let calls = 0;
  const clients: BrokerClients = Object.create(BrokerClients.prototype);
  const north: NorthClient = Object.create(NorthClient.prototype);
  Object.assign(north, {
    async exportAgentInventory(): Promise<ExportAgentInventoryResponse> {
      calls++;
      return response();
    },
  });
  clients.north = north;

  const app = Fastify({ logger: false });
  registerAdminRoutes(app, { clients, jwksResolver, verifyOpts: VERIFY_OPTS });
  await app.ready();
  return { app, token, calls: () => calls };
}

test("inventory JSON download carries owners, connectors, tools, key counts and expiry", async () => {
  const { app, token } = await buildApp(async () => ({ agents: [ENTRY] }));
  const res = await app.inject({ method: "GET", url: "/admin/agents/inventory", headers: { authorization: `Bearer ${token}` } });
  assert.equal(res.statusCode, 200);
  assert.match(String(res.headers["content-type"]), /^application\/json/);
  assert.equal(res.headers["content-disposition"], 'attachment; filename="aikonos-agent-inventory.json"');
  assert.equal(res.headers["cache-control"], "no-store");
  const [row] = JSON.parse(res.body).agents;
  assert.deepEqual(row.owners, ["user:alice", "group:ops#member"]);
  assert.deepEqual(row.connectors, ["jira"]);
  assert.deepEqual(row.allowedTools, ["mcp:jira:search", "mcp:jira:get"]);
  assert.equal(row.serviceAgent, true);
  assert.equal(row.apiKeys, 2);
  assert.equal(row.publicKeys, 1);
  assert.equal(row.nearestKeyExpiry, "2026-11-30T00:00:00.000Z");
  assert.equal(row.lastUsedAt, null);
  assert.equal(row.soul, undefined);
});

test("inventory CSV download: header, ;-joined lists, formula cells neutralised", async () => {
  const { app, token } = await buildApp(async () => ({ agents: [ENTRY] }));
  const res = await app.inject({ method: "GET", url: "/admin/agents/inventory?format=csv", headers: { authorization: `Bearer ${token}` } });
  assert.equal(res.statusCode, 200);
  assert.match(String(res.headers["content-type"]), /^text\/csv/);
  assert.equal(res.headers["content-disposition"], 'attachment; filename="aikonos-agent-inventory.csv"');
  const [header, line] = res.body.split("\r\n");
  assert.equal(header.split(",")[3], "owners");
  assert.ok(line.startsWith("a1,'=cmd|' /c calc'!A1,active,user:alice;group:ops#member,true,"), line);
  assert.ok(line.includes(",jira,mcp:jira:search;mcp:jira:get,web.fetch,2,1,2026-11-30T00:00:00.000Z,,"), line);
});

test("inventoryCsv quotes cells with commas and quotes", () => {
  const row = inventoryRow({ ...ENTRY, agent: { ...ENTRY.agent!, name: 'a, "b"' } });
  assert.ok(inventoryCsv([row]).includes(',"a, ""b""",'));
});

test("inventory requires a bearer and passes broker denials through", async () => {
  const { app, token, calls } = await buildApp(async () => {
    throw Object.assign(new Error("must be tenant admin"), { code: grpc.status.PERMISSION_DENIED });
  });
  assert.equal((await app.inject({ method: "GET", url: "/admin/agents/inventory" })).statusCode, 401);
  assert.equal(calls(), 0);
  const res = await app.inject({ method: "GET", url: "/admin/agents/inventory", headers: { authorization: `Bearer ${token}` } });
  assert.equal(res.statusCode, 403);
});
