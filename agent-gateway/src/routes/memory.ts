// Memory management proxy routes: the settings-modal Memory pane lists, reads, verifies,
// deprecates, and deletes OKF concept bundles through these forwards.
// Browser → /api/gw proxy → here → broker north RPCs.
//
// No local authz. The broker enforces the whole matrix (own user bundle: self;
// group: member to read, manager to manage; agent: tenant admin) and a denial
// arrives as PERMISSION_DENIED, which sendError maps to 403 — the gateway must
// not second-guess it with a client-visible admin check of its own.
import type { FastifyInstance } from "fastify";
import { requireUser } from "../auth/require-user.js";
import type { JwksResolver, VerifyOptions } from "../auth/verify.js";
import type { NorthClient } from "../broker/north.js";
import { sendError } from "../http-errors.js";
import { log } from "../log.js";

export interface MemoryCtx {
  clients: {
    north: Pick<
      NorthClient,
      | "listMemoryGroups"
      | "listMemoryConcepts"
      | "getMemoryConcept"
      | "verifyMemoryConcept"
      | "deprecateMemoryConcept"
      | "deleteMemoryConcept"
    >;
  };
  jwksResolver: JwksResolver;
  verifyOpts: VerifyOptions;
}

// The scope triple every memory RPC carries, in the snake_case the browser
// sends (query string on reads, JSON body on mutations).
interface ScopeArgs {
  scope?: string;
  group_id?: string;
  agent_id?: string;
}

// scopeRequest builds the identity + scope fields shared by all six RPCs. The
// tenant and user always come from the verified principal, never the request —
// the broker re-derives them from the bearer anyway, so a mismatch would only
// ever be a bug on this side.
function scopeRequest(principal: { tenant: string; sub: string }, args: ScopeArgs) {
  return {
    tenantId: principal.tenant,
    userId: principal.sub,
    scope: args.scope ?? "",
    groupId: args.group_id ?? "",
    agentId: args.agent_id ?? "",
  };
}

export function registerMemoryRoutes(app: FastifyInstance, ctx: MemoryCtx): void {
  app.get("/memory/groups", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    try {
      const resp = await ctx.clients.north.listMemoryGroups(
        { tenantId: principal.tenant, userId: principal.sub },
        principal.token,
      );
      reply.send({ groups: resp.groups ?? [] });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { groups: [] } });
    }
  });

  app.get<{ Querystring: ScopeArgs }>("/memory", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    try {
      const resp = await ctx.clients.north.listMemoryConcepts(
        scopeRequest(principal, req.query),
        principal.token,
      );
      reply.send({ concepts: resp.concepts ?? [] });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { concepts: [] } });
    }
  });

  app.get<{ Querystring: ScopeArgs & { id?: string } }>("/memory/concept", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    try {
      const resp = await ctx.clients.north.getMemoryConcept(
        { ...scopeRequest(principal, req.query), id: req.query.id ?? "" },
        principal.token,
      );
      reply.send({ meta: resp.meta, body: resp.body });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.post<{ Body: ScopeArgs & { id?: string } }>("/memory/verify", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    try {
      const resp = await ctx.clients.north.verifyMemoryConcept(
        { ...scopeRequest(principal, req.body ?? {}), id: req.body?.id ?? "" },
        principal.token,
      );
      reply.send({ meta: resp.meta });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.post<{ Body: ScopeArgs & { id?: string } }>("/memory/deprecate", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    try {
      const resp = await ctx.clients.north.deprecateMemoryConcept(
        { ...scopeRequest(principal, req.body ?? {}), id: req.body?.id ?? "" },
        principal.token,
      );
      reply.send({ meta: resp.meta });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.post<{ Body: ScopeArgs & { id?: string } }>("/memory/delete", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    try {
      await ctx.clients.north.deleteMemoryConcept(
        { ...scopeRequest(principal, req.body ?? {}), id: req.body?.id ?? "" },
        principal.token,
      );
      reply.send({ deleted: true });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });
}
