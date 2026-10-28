// AG-17: POST and PATCH /schedules forward the "Acts as" agent (bare UUID) to the
// broker and rejects a malformed one before any RPC — under identity.obo=enforce
// an actorless schedule is refused, so the webui picker must reach the broker.
import { test } from "node:test";
import assert from "node:assert/strict";
import Fastify from "fastify";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import { registerScheduleRoutes } from "../src/routes/schedules.js";
import type { CreateScheduledRunRequest, UpdateScheduledRunRequest } from "../gen/ts/proto/broker.js";

const VERIFY_OPTS = { issuer: "http://localhost:18080/realms/aikonos", audience: "aikonos-broker" };
const AGENT = "11111111-2222-3333-4444-555555555555";

async function setup() {
  const { publicKey, privateKey } = await generateKeyPair("RS256");
  const jwk: JWK = { ...(await exportJWK(publicKey)), kid: "k1", alg: "RS256", use: "sig" };
  const key = await importJWK(jwk, "RS256");
  const token = await new SignJWT({ sub: "alice@example.com", email: "alice@example.com", tenant_id: "aikonos-dev" })
    .setProtectedHeader({ alg: "RS256", kid: "k1" })
    .setIssuer(VERIFY_OPTS.issuer).setAudience(VERIFY_OPTS.audience)
    .setIssuedAt().setExpirationTime("1h").sign(privateKey);
  const calls: CreateScheduledRunRequest[] = [];
  const updates: UpdateScheduledRunRequest[] = [];
  const north = {
    listScheduledRuns: (): never => { throw new Error("unused"); },
    async createScheduledRun(req: CreateScheduledRunRequest) { calls.push(req); return { run: undefined }; },
    async updateScheduledRun(req: UpdateScheduledRunRequest) { updates.push(req); return { run: undefined }; },
    deleteScheduledRun: (): never => { throw new Error("unused"); },
  };
  const app = Fastify({ logger: false });
  registerScheduleRoutes(app, { clients: { north }, jwksResolver: () => Promise.resolve(key), verifyOpts: VERIFY_OPTS });
  await app.ready();
  const post = (body: Record<string, unknown>) =>
    app.inject({ method: "POST", url: "/schedules", headers: { authorization: `Bearer ${token}` }, payload: body });
  const patch = (body: Record<string, unknown>) =>
    app.inject({ method: "PATCH", url: "/schedules/s1", headers: { authorization: `Bearer ${token}` }, payload: body });
  return { app, calls, updates, post, patch };
}

test("POST /schedules forwards actorAgentId and an offset runAt as the same instant", async () => {
  const { app, calls, post } = await setup();
  const res = await post({ prompt: "p", kind: "ONCE", runAt: "2099-03-01T09:30:00+01:00", actorAgentId: AGENT });
  assert.equal(res.statusCode, 200);
  assert.equal(calls[0].actorAgentId, AGENT);
  assert.equal(calls[0].runAt?.toISOString(), "2099-03-01T08:30:00.000Z");

  await post({ prompt: "p" });
  assert.equal(calls[1].actorAgentId, "", "omitted actor stays empty (broker decides under identity.obo)");
  await app.close();
});

test("POST /schedules rejects a non-UUID actorAgentId without calling the broker", async () => {
  const { app, calls, post } = await setup();
  for (const bad of ["agent:" + AGENT, "alice-agent", "not-a-uuid"]) {
    const res = await post({ prompt: "p", actorAgentId: bad });
    assert.equal(res.statusCode, 400, bad);
  }
  assert.equal(calls.length, 0);
  await app.close();
});

test("PATCH /schedules/:id forwards actorAgentId only when present, rejects a malformed one", async () => {
  const { app, updates, patch } = await setup();
  assert.equal((await patch({ prompt: "p", actorAgentId: AGENT })).statusCode, 200);
  assert.equal(updates[0].actorAgentId, AGENT);
  await patch({ prompt: "p" });
  assert.equal(updates[1].actorAgentId, undefined, "absent actor = keep the schedule's actor");
  await patch({ prompt: "p", actorAgentId: "" });
  assert.equal(updates[2].actorAgentId, "", "explicit empty reaches the broker (clear, refused under enforce)");

  assert.equal((await patch({ prompt: "p", actorAgentId: "alice-agent" })).statusCode, 400);
  assert.equal(updates.length, 3);
  await app.close();
});
