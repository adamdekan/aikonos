// Human-in-the-loop approvals for interactive runs: the approval dialog lists
// the caller's pending approvals and answers them. The /agui route raises them
// and ApprovalRegistry (src/agui/hitl.ts) holds them until they are answered.
import type { FastifyInstance } from "fastify";
import { requireUser } from "../auth/require-user.js";
import type { JwksResolver, VerifyOptions } from "../auth/verify.js";
import type { ApprovalRegistry } from "../agui/hitl.js";
import type { NorthClient } from "../broker/north.js";
import { isStepUpRequired, sendError, stepUpAcr } from "../http-errors.js";
import { log } from "../log.js";

export interface ApprovalRoutesCtx {
  approvals: ApprovalRegistry;
  clients: { north: Pick<NorthClient, "listPendingApprovals" | "approveTask" | "cancelTask"> };
  jwksResolver: JwksResolver;
  verifyOpts: VerifyOptions;
}

export function registerApprovalRoutes(app: FastifyInstance, ctx: ApprovalRoutesCtx): void {
  // Answer a pending approval (the dialog's Approve and Deny buttons). Only the
  // user whose run asked can answer it. The id is the model's tool-call id,
  // which shows in the run's stream and the saved session, so knowing it proves
  // nothing; another user's id gets the same reply as an unknown one.
  // An approval echoes the argsDigest the dialog was shown (AG-07); without
  // the right one it is refused with 409 and the approval keeps waiting.
  app.post<{ Params: { id: string }; Body: { approved?: boolean; argsDigest?: string } }>("/approve/:id", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    const result = ctx.approvals.resolve(req.params.id, principal.sub, req.body?.approved === true, req.body?.argsDigest);
    if (result === "mismatch") {
      reply.code(409).send({ resolved: false, error: "the arguments changed since this approval was shown" });
      return;
    }
    reply.send({ resolved: result === "resolved" });
  });

  // Pending approvals for a user (the CopilotKit frontend polls this to render
  // approval modals, since it can't easily surface AG-UI CUSTOM events).
  // ?threadId= narrows it to one chat's run.
  app.get<{ Querystring: { threadId?: string } }>("/approvals", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    const threadId = typeof req.query?.threadId === "string" ? req.query.threadId : undefined;
    reply.send({ approvals: ctx.approvals.listForUser(principal.sub, threadId || undefined) });
  });

  // Broker-held approvals the caller may decide (AG-07): survives a gateway
  // restart and reaches every approver of an n-of-m gate, not just the run's
  // owner. The broker applies approver_set + FGA can_approve.
  app.get("/pending-approvals", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    try {
      const resp = await ctx.clients.north.listPendingApprovals(
        { tenantId: principal.tenant, userId: principal.sub },
        principal.token,
      );
      reply.send({ approvals: resp.approvals ?? [] });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { approvals: [] } });
    }
  });

  // Decide one of them as the caller (broker ApproveTask). Any capability
  // tokens an approval mints stay here; the browser only learns the new status.
  // A run waiting on this approval wakes through its broker poll (AG-07 slice 2).
  app.post<{ Params: { taskId: string }; Body: { approved?: boolean; argsDigest?: string } }>(
    "/pending-approvals/:taskId",
    async (req, reply) => {
      const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
      if (!principal) return;
      try {
        const resp = await ctx.clients.north.approveTask(
          {
            taskId: req.params.taskId,
            tenantId: principal.tenant,
            approved: req.body?.approved === true,
            reason: "",
            approverUserId: principal.sub,
            argsDigest: req.body?.argsDigest ?? "",
          },
          principal.token,
        );
        reply.send({ newStatus: resp.newStatus });
      } catch (err) {
        // AG-13: 401 tells the browser to sign in again with MFA and retry.
        if (isStepUpRequired(err)) {
          reply.code(401).send({ error: "step_up_required", acr: stepUpAcr(err) });
          return;
        }
        sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: {} });
      }
    },
  );

  // Withdraw the caller's own pending request (the chat card's Withdraw): the
  // broker closes the gate and cancels its task (CancelTask: owner or admin
  // only). A run waiting on it wakes through its broker poll.
  app.post<{ Params: { taskId: string } }>("/pending-approvals/:taskId/withdraw", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    try {
      await ctx.clients.north.cancelTask(
        { taskId: req.params.taskId, tenantId: principal.tenant, reason: "approval withdrawn by requester" },
        principal.token,
      );
      reply.send({ withdrawn: true });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: {} });
    }
  });
}
