// Per-session LLM usage: model / tokens / cost for one chat session, backing
// the webui's usage strip above the composer. Browser → /api/gw proxy → here →
// broker north GetSessionUsage.
//
// A thin proxy on purpose. The broker scopes the read to the verified caller,
// so this layer adds no authorization of its own beyond requireUser — it must
// not, for instance, accept a userId from the query and forward it.
import type { FastifyInstance } from "fastify";
import { requireUser } from "../auth/require-user.js";
import type { JwksResolver, VerifyOptions } from "../auth/verify.js";
import type { NorthClient } from "../broker/north.js";
import { sendError } from "../http-errors.js";
import { log } from "../log.js";

export interface SessionUsageCtx {
  clients: {
    north: Pick<NorthClient, "getSessionUsage">;
  };
  jwksResolver: JwksResolver;
  verifyOpts: VerifyOptions;
}

// Shape the webui renders. Totals are summed here rather than in the browser so
// the strip has one number to show and every consumer sums identically; rows are
// still returned for the model breakdown (a session can span models via
// fallback, vision and reason calls).
interface SessionUsageResponse {
  models: string[];
  tokensIn: number;
  tokensOut: number;
  cacheRead: number;
  cacheWrite: number;
  costMicros: number;
  calls: number;
}

export function registerSessionUsageRoutes(app: FastifyInstance, ctx: SessionUsageCtx): void {
  app.get<{ Params: { id: string } }>("/sessions/:id/usage", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    try {
      const resp = await ctx.clients.north.getSessionUsage(
        { tenantId: principal.tenant, sessionId: req.params.id },
        principal.token,
      );
      const rows = resp.rows ?? [];
      const out: SessionUsageResponse = {
        // Broker orders rows most-expensive-first, so models[0] is the one worth
        // showing when there is only room for one.
        models: rows.map((r) => r.model).filter((m) => m !== ""),
        tokensIn: 0,
        tokensOut: 0,
        cacheRead: 0,
        cacheWrite: 0,
        costMicros: 0,
        calls: 0,
      };
      for (const r of rows) {
        out.tokensIn += Number(r.tokensIn ?? 0);
        out.tokensOut += Number(r.tokensOut ?? 0);
        out.cacheRead += Number(r.cacheRead ?? 0);
        out.cacheWrite += Number(r.cacheWrite ?? 0);
        out.costMicros += Number(r.costMicros ?? 0);
        out.calls += Number(r.calls ?? 0);
      }
      reply.send(out);
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });
}
