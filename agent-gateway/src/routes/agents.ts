// Agents: user discovery (GET /agents) + per-agent soul read/write + the
// user's default agent (GET/PUT /agents/default, AG-01).
// GET /agents returns the agents the requesting user can use (ListMyAgents).
// Registered by registerAgentsRoutes(app, ctx) called from src/app.ts at the
// same position these routes previously occupied inline in server.ts.
import type { FastifyInstance } from "fastify";
import { requireUser } from "../auth/require-user.js";
import type { JwksResolver, VerifyOptions } from "../auth/verify.js";
import type { NorthClient } from "../broker/north.js";
import type { ChildSupervisor } from "../ipc/supervisor.js";
import { sendError } from "../http-errors.js";
import { log } from "../log.js";

export interface AgentsCtx {
  clients: {
    north: Pick<NorthClient, "listMyAgents" | "getAgentSoul" | "setAgentSoul" | "getMyDefaultAgent" | "setMyDefaultAgent">;
  };
  jwksResolver: JwksResolver;
  verifyOpts: VerifyOptions;
  // F28: push-invalidates idle children bound to the edited agent so a soul
  // edit takes effect immediately instead of waiting for the idle recheck.
  supervisor: Pick<ChildSupervisor, "evictIdleForAgent">;
}

export function registerAgentsRoutes(app: FastifyInstance, ctx: AgentsCtx): void {
  app.get("/agents", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    try {
      const resp = await ctx.clients.north.listMyAgents(
        { tenantId: principal.tenant, userId: principal.sub },
        principal.token,
      );
      reply.send({ agents: (resp.agents ?? []).map((a) => ({ id: a.id, name: a.name })) });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { agents: [] } });
    }
  });

  // AG-01: the agent a chat or schedule runs as when none is picked. "" = the
  // tenant default agent. The broker requires can_use on a set (403 otherwise).
  app.get("/agents/default", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    try {
      const resp = await ctx.clients.north.getMyDefaultAgent(
        { tenantId: principal.tenant, userId: principal.sub },
        principal.token,
      );
      reply.send({ agentId: resp.agentId ?? "" });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.put<{ Body: { agentId?: string } }>("/agents/default", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    const agentId = req.body?.agentId ?? "";
    if (typeof agentId !== "string") {
      reply.code(400).send({ error: "agentId must be a string" });
      return;
    }
    try {
      const resp = await ctx.clients.north.setMyDefaultAgent(
        { tenantId: principal.tenant, userId: principal.sub, agentId },
        principal.token,
      );
      reply.send({ agentId: resp.agentId ?? "" });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.get<{ Params: { id: string } }>("/agents/:id/soul", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    try {
      const resp = await ctx.clients.north.getAgentSoul(
        { tenantId: principal.tenant, userId: principal.sub, agentId: req.params.id },
        principal.token,
      );
      reply.send({ soul: resp.soul });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.put<{ Params: { id: string }; Body: { soul?: string } }>("/agents/:id/soul", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    const soul = req.body?.soul ?? "";
    try {
      const resp = await ctx.clients.north.setAgentSoul(
        { tenantId: principal.tenant, userId: principal.sub, agentId: req.params.id, soul },
        principal.token,
      );
      // Best-effort: an eviction failure must not fail the soul update itself —
      // the idle recheck (PLAN_RECHECK_MS) remains the backstop either way.
      try {
        ctx.supervisor.evictIdleForAgent(req.params.id, "soul updated");
      } catch (evictErr) {
        log.warn({ agentId: req.params.id, err: String(evictErr) }, "evictIdleForAgent failed — idle recheck remains the backstop");
      }
      reply.send({ soul: resp.soul });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });
}
