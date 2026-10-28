// AG-16: the /scim/v2 passthrough forwards method, path, query, raw body and
// sha256(token) to the broker, and relays its status and SCIM body.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import Fastify from "fastify";
import { registerScimRoutes } from "../../src/external/scim.js";
import type { Config } from "../../src/config.js";
import type { BrokerClients } from "../../src/broker/clients.js";
import type { ScimCallRequest } from "../../gen/ts/proto/broker.js";

const TENANT = "11111111-1111-1111-1111-111111111111";
const cfg = { defaultTenantId: TENANT } as Config;
const log = { warn: () => {} } as unknown as import("../../src/log.js").Logger;

function build(scimCall: (r: ScimCallRequest) => Promise<{ status: number; body: Buffer }>) {
  const app = Fastify();
  registerScimRoutes(app, cfg, { south: { scimCall } } as unknown as BrokerClients, log);
  return app;
}

test("forwards an Entra PATCH with scim+json body and relays the response", async () => {
  const seen: ScimCallRequest[] = [];
  const app = build(async (r) => {
    seen.push(r);
    return { status: 200, body: Buffer.from('{"id":"u1","active":false}') };
  });
  const body = '{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"Replace","path":"active","value":"False"}]}';
  const resp = await app.inject({
    method: "PATCH",
    url: "/scim/v2/Users/u1?attributes=active",
    headers: { authorization: "Bearer scim_secret", "content-type": "application/scim+json" },
    payload: body,
  });
  assert.equal(resp.statusCode, 200);
  assert.match(resp.headers["content-type"] as string, /application\/scim\+json/);
  assert.deepEqual(resp.json(), { id: "u1", active: false });
  assert.equal(seen.length, 1);
  assert.equal(seen[0].tenantId, TENANT);
  assert.equal(seen[0].method, "PATCH");
  assert.equal(seen[0].path, "/Users/u1");
  assert.equal(seen[0].query, "attributes=active");
  assert.equal(Buffer.from(seen[0].body).toString(), body);
  assert.equal(seen[0].tokenHashHex, createHash("sha256").update("scim_secret").digest("hex"));
  assert.ok(!JSON.stringify(seen[0]).includes("scim_secret"), "raw token must not reach the broker");
});

test("missing bearer is 401 without a broker call; 204 has no body; broker down is 503", async () => {
  let calls = 0;
  const app = build(async () => {
    calls++;
    if (calls === 2) throw new Error("unavailable");
    return { status: 204, body: Buffer.alloc(0) };
  });
  const noAuth = await app.inject({ method: "GET", url: "/scim/v2/Users" });
  assert.equal(noAuth.statusCode, 401);
  assert.equal(calls, 0);

  const del = await app.inject({ method: "DELETE", url: "/scim/v2/Users/u1", headers: { authorization: "Bearer t" } });
  assert.equal(del.statusCode, 204);
  assert.equal(del.body, "");

  const down = await app.inject({ method: "GET", url: "/scim/v2/Users", headers: { authorization: "Bearer t" } });
  assert.equal(down.statusCode, 503);
  assert.equal(down.json().status, "503");
});
