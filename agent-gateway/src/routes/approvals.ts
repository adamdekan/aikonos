// Human-in-the-loop approvals for interactive runs: the approval dialog lists
// the caller's pending approvals and answers them. The /agui route raises them
// and ApprovalRegistry (src/agui/hitl.ts) holds them until they are answered.
import type { FastifyInstance } from "fastify";
import { requireUser } from "../auth/require-user.js";
import type { JwksResolver, VerifyOptions } from "../auth/verify.js";
import type { ApprovalRegistry } from "../agui/hitl.js";

export interface ApprovalRoutesCtx {
  approvals: ApprovalRegistry;
  jwksResolver: JwksResolver;
  verifyOpts: VerifyOptions;
}

export function registerApprovalRoutes(app: FastifyInstance, ctx: ApprovalRoutesCtx): void {
  // Answer a pending approval (the dialog's Approve and Deny buttons). Only the
  // user whose run asked can answer it. The id is the model's tool-call id,
  // which shows in the run's stream and the saved session, so knowing it proves
  // nothing; another user's id gets the same reply as an unknown one.
  app.post<{ Params: { id: string }; Body: { approved?: boolean } }>("/approve/:id", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    const resolved = ctx.approvals.resolve(req.params.id, principal.sub, req.body?.approved === true);
    reply.send({ resolved });
  });

  // Pending approvals for a user (the CopilotKit frontend polls this to render
  // approval modals, since it can't easily surface AG-UI CUSTOM events).
  app.get("/approvals", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    reply.send({ approvals: ctx.approvals.listForUser(principal.sub) });
  });
}
