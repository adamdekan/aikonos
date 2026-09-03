// Regression test for the CP2 proto-fallout bug: after workflow-mode fields
// (workflowLineageId/workflowInputs) were added to CreateScheduledRunRequest/
// UpdateScheduledRunRequest, routes/schedules.ts's request literals omitted
// them. CreateScheduledRunRequest.encode() does an unguarded
// `Object.entries(message.workflowInputs)` — a missing (undefined) field
// throws a TypeError at encode time, not a TS compile error, because the
// route's other tests fake the north client and never touch the real proto
// encode path (schedules-error-mapping.test.ts).
//
// This test registers the real registerScheduleRoutes but gives it a north
// double whose createScheduledRun/updateScheduledRun actually round-trip the
// request through the real generated encode()/decode() — the same step a
// genuine gRPC client performs — so a future regression that drops a required
// field fails here instead of at the first prod POST /schedules.
import { test } from "node:test";
import assert from "node:assert/strict";
import Fastify from "fastify";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import { registerScheduleRoutes } from "../src/routes/schedules.js";
import {
  CreateScheduledRunRequest,
  UpdateScheduledRunRequest,
  ScheduleKind,
  ScheduledRunState,
  type ScheduledRun,
} from "../gen/ts/proto/broker.js";
import type { JwksResolver } from "../src/auth/verify.js";

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

// runOf builds a minimal ScheduledRun echoing the decoded request fields —
// enough for scheduleJson() to render without touching unrelated columns.
function runOf(id: string, decoded: { prompt: string; kind: ScheduleKind; cronExpr: string }): ScheduledRun {
  return {
    id,
    ownerUserId: "alice@example.com",
    prompt: decoded.prompt,
    kind: decoded.kind,
    cronExpr: decoded.cronExpr,
    nextFireAt: undefined,
    approvedTools: [],
    state: ScheduledRunState.SCHEDULED_RUN_ACTIVE,
    lastFireAt: undefined,
    lastStatus: "",
    lastSummary: "",
    runCount: 0,
    createdBy: "alice@example.com",
    createdAt: undefined,
    workflowLineageId: "",
    workflowInputs: {},
    workflowDisplayName: "",
  };
}

async function buildApp() {
  const { privateKey, jwk } = await makeKey();
  const jwksResolver = await localResolver(jwk);
  const token = await mintToken(privateKey);

  const app = Fastify({ logger: false });

  const north = {
    listScheduledRuns: (): never => {
      throw new Error("not used in this test");
    },
    // Round-trips through the REAL wire encode/decode — the step
    // schedules-error-mapping.test.ts's fake client skips entirely.
    async createScheduledRun(req: CreateScheduledRunRequest) {
      const bytes = CreateScheduledRunRequest.encode(req).finish();
      const decoded = CreateScheduledRunRequest.decode(bytes);
      return { run: runOf("sched-new", decoded) };
    },
    async updateScheduledRun(req: UpdateScheduledRunRequest) {
      const bytes = UpdateScheduledRunRequest.encode(req).finish();
      const decoded = UpdateScheduledRunRequest.decode(bytes);
      return { run: runOf(decoded.id, decoded) };
    },
    deleteScheduledRun: (): never => {
      throw new Error("not used in this test");
    },
  };

  registerScheduleRoutes(app, { clients: { north }, jwksResolver, verifyOpts: VERIFY_OPTS });

  return { app, token };
}

test("POST /schedules — real CreateScheduledRunRequest encode/decode round-trip does not throw", async () => {
  const { app, token } = await buildApp();
  await app.ready();

  const res = await app.inject({
    method: "POST",
    url: "/schedules",
    headers: { authorization: `Bearer ${token}` },
    payload: { prompt: "say hi every morning", kind: "CRON", cronExpr: "0 8 * * *" },
  });

  assert.equal(res.statusCode, 200, res.body);
  const body = JSON.parse(res.body) as { schedule: { prompt: string } };
  assert.equal(body.schedule.prompt, "say hi every morning");

  await app.close();
});

test("PATCH /schedules/:id — real UpdateScheduledRunRequest encode/decode round-trip does not throw", async () => {
  const { app, token } = await buildApp();
  await app.ready();

  const res = await app.inject({
    method: "PATCH",
    url: "/schedules/sched-1",
    headers: { authorization: `Bearer ${token}` },
    payload: { prompt: "updated prompt", kind: "ONCE", runAt: new Date(Date.now() + 60_000).toISOString() },
  });

  assert.equal(res.statusCode, 200, res.body);
  const body = JSON.parse(res.body) as { schedule: { prompt: string } };
  assert.equal(body.schedule.prompt, "updated prompt");

  await app.close();
});
