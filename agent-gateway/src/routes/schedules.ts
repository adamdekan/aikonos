// Scheduler: user-scheduled agentic runs.
// Users manage their own schedules (verified bearer); the broker gates creation
// on the skill:scheduler capability. The admin list route is tenant-admin gated
// in the broker (403 on deny).
// Registered by registerScheduleRoutes(app, ctx) called from src/app.ts at the
// same position these routes previously occupied inline in server.ts.
import type { FastifyInstance } from "fastify";
import { requireUser } from "../auth/require-user.js";
import type { JwksResolver, VerifyOptions } from "../auth/verify.js";
import type { NorthClient } from "../broker/north.js";
import { ScheduleKind, ScheduledRunState } from "../../gen/ts/proto/broker";
import { scheduleJson } from "../schedule-json.js";
import { sendError } from "../http-errors.js";
import { log } from "../log.js";

export interface ScheduleCtx {
  clients: {
    north: Pick<NorthClient, "listScheduledRuns" | "createScheduledRun" | "updateScheduledRun" | "deleteScheduledRun">;
  };
  jwksResolver: JwksResolver;
  verifyOpts: VerifyOptions;
}

function kindFromBody(k?: string): ScheduleKind {
  return (k ?? "").toUpperCase() === "ONCE"
    ? ScheduleKind.SCHEDULE_KIND_ONCE
    : ScheduleKind.SCHEDULE_KIND_CRON;
}

interface ScheduleBody {
  user?: string;
  prompt?: string;
  kind?: string;
  cronExpr?: string;
  runAt?: string; // ISO datetime
  approvedTools?: string[];
  action?: "pause" | "resume";
}

export function registerScheduleRoutes(app: FastifyInstance, ctx: ScheduleCtx): void {
  app.get("/schedules", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    try {
      const resp = await ctx.clients.north.listScheduledRuns(
        { tenantId: principal.tenant, userId: principal.sub, ownerFilter: "" },
        principal.token,
      );
      reply.send({ schedules: (resp.runs ?? []).map(scheduleJson) });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { schedules: [] } });
    }
  });

  app.post<{ Body: ScheduleBody }>("/schedules", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    const b = req.body ?? {};
    try {
      const resp = await ctx.clients.north.createScheduledRun(
        {
          tenantId: principal.tenant,
          userId: principal.sub,
          prompt: b.prompt ?? "",
          kind: kindFromBody(b.kind),
          cronExpr: b.cronExpr ?? "",
          runAt: b.runAt ? new Date(b.runAt) : undefined,
          approvedTools: b.approvedTools ?? [],
          // Chat-created workflow schedules land via the CP3 Pi tool, not this
          // webui-facing route — always prompt-mode.
          workflowLineageId: "",
          workflowInputs: {},
        },
        principal.token,
      );
      reply.send({ schedule: resp.run ? scheduleJson(resp.run) : null });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.patch<{ Params: { id: string }; Body: ScheduleBody }>("/schedules/:id", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    const b = req.body ?? {};
    let desiredState = ScheduledRunState.SCHEDULED_RUN_STATE_UNSPECIFIED;
    if (b.action === "pause") desiredState = ScheduledRunState.SCHEDULED_RUN_PAUSED;
    else if (b.action === "resume") desiredState = ScheduledRunState.SCHEDULED_RUN_ACTIVE;
    try {
      const resp = await ctx.clients.north.updateScheduledRun(
        {
          tenantId: principal.tenant,
          userId: principal.sub,
          id: req.params.id,
          prompt: b.prompt ?? "",
          kind: kindFromBody(b.kind),
          cronExpr: b.cronExpr ?? "",
          runAt: b.runAt ? new Date(b.runAt) : undefined,
          approvedTools: b.approvedTools ?? [],
          desiredState,
          // Empty = "no change" server-side (scheduler.go's UpdateScheduledRun
          // only touches the workflow binding when this is non-empty) — this
          // route only ever edits timing/approvedTools/state, so always send
          // the no-op value rather than omitting fields the wire type requires.
          workflowLineageId: "",
          workflowInputs: {},
        },
        principal.token,
      );
      reply.send({ schedule: resp.run ? scheduleJson(resp.run) : null });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.delete<{ Params: { id: string } }>(
    "/schedules/:id",
    async (req, reply) => {
      const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
      if (!principal) return;
      try {
        const resp = await ctx.clients.north.deleteScheduledRun(
          { tenantId: principal.tenant, userId: principal.sub, id: req.params.id },
          principal.token,
        );
        reply.send({ success: resp.success });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { success: false } });
      }
    },
  );
}
