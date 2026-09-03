// CP4: GET /schedules must surface the workflow-mode display fields
// (workflowLineageId, workflowDisplayName) so the webui can render a
// distinguishable badge + workflow name.
// Registers the real registerScheduleRoutes against a fake north client so a
// scheduleJson() regression that drops a field fails here, not in the webui.
import { test } from "node:test";
import assert from "node:assert/strict";
import Fastify from "fastify";
import { generateKeyPair, exportJWK, SignJWT, importJWK } from "jose";
import type { JWK } from "jose";

import { registerScheduleRoutes } from "../src/routes/schedules.js";
import { ScheduleKind, ScheduledRunState, type ListScheduledRunsResponse } from "../gen/ts/proto/broker.js";
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

test("GET /schedules — workflow-mode row surfaces workflowLineageId + workflowDisplayName", async () => {
  const { privateKey, jwk } = await makeKey();
  const jwksResolver = await localResolver(jwk);
  const token = await mintToken(privateKey);

  const app = Fastify({ logger: false });
  const north = {
    async listScheduledRuns(): Promise<ListScheduledRunsResponse> {
      return {
        runs: [
          {
            id: "s1",
            ownerUserId: "alice@example.com",
            prompt: "",
            kind: ScheduleKind.SCHEDULE_KIND_CRON,
            cronExpr: "0 9 * * *",
            nextFireAt: undefined,
            approvedTools: [],
            state: ScheduledRunState.SCHEDULED_RUN_ACTIVE,
            lastFireAt: undefined,
            lastStatus: "",
            lastSummary: "",
            runCount: 0,
            createdBy: "alice@example.com",
            createdAt: undefined,
            workflowLineageId: "wf-lineage-1",
            workflowInputs: {},
            workflowDisplayName: "Weekly digest",
          },
        ],
        fgaEnabled: true,
        warnings: [],
      };
    },
    createScheduledRun: (): never => { throw new Error("not used in this test"); },
    updateScheduledRun: (): never => { throw new Error("not used in this test"); },
    deleteScheduledRun: (): never => { throw new Error("not used in this test"); },
  };

  registerScheduleRoutes(app, { clients: { north }, jwksResolver, verifyOpts: VERIFY_OPTS });
  await app.ready();

  const res = await app.inject({
    method: "GET",
    url: "/schedules",
    headers: { authorization: `Bearer ${token}` },
  });

  assert.equal(res.statusCode, 200);
  const body = JSON.parse(res.body) as {
    schedules: Array<{ workflowLineageId: string; workflowDisplayName: string }>;
  };
  assert.equal(body.schedules.length, 1);
  assert.equal(body.schedules[0].workflowLineageId, "wf-lineage-1");
  assert.equal(body.schedules[0].workflowDisplayName, "Weekly digest");

  await app.close();
});
