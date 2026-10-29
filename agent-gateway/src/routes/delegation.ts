// Delegation route group (7.6): /delegate, /inbox*, /delegatable-users.
// alice delegates a scoped task to bob → broker SendEnvelope (Biscuit
// attenuation + OPA envelope_send + OpenFGA can_delegate_to_user).
// Registered by registerDelegationRoutes(app, ctx) called from src/app.ts at
// the same position these routes previously occupied inline in server.ts.
import type { FastifyInstance } from "fastify";
import { requireUser } from "../auth/require-user.js";
import type { JwksResolver, VerifyOptions } from "../auth/verify.js";
import type { NorthClient } from "../broker/north.js";
import { sendError } from "../http-errors.js";
import { log } from "../log.js";

export interface DelegationCtx {
  clients: {
    north: Pick<
      NorthClient,
      "sendEnvelope" | "listInboxEnvelopes" | "listDelegatableUsers" | "respondToEnvelope" | "dismissEnvelope"
    >;
  };
  jwksResolver: JwksResolver;
  verifyOpts: VerifyOptions;
}

export function registerDelegationRoutes(app: FastifyInstance, ctx: DelegationCtx): void {
  app.post<{ Body: { to?: string; group?: string; intent?: string; scopes?: string[]; maxCost?: number } }>(
    "/delegate",
    async (req, reply) => {
      const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
      if (!principal) return;
      // No guessed recipient and no scope the sender did not name: exactly one
      // of to/group, scopes default to none, and the cost cap to 50 units.
      const { to, group, intent = "", scopes = [], maxCost = 50 } = req.body ?? {};
      const hasTo = typeof to === "string" && to.trim() !== "";
      const hasGroup = typeof group === "string" && group.trim() !== "";
      if (hasTo === hasGroup) {
        reply.code(400).send({ ok: false, error: "name exactly one recipient: to or group" });
        return;
      }
      if (!Array.isArray(scopes) || !scopes.every((sc) => typeof sc === "string")) {
        reply.code(400).send({ ok: false, error: "scopes must be a list of strings" });
        return;
      }
      if (typeof maxCost !== "number" || !Number.isFinite(maxCost) || maxCost < 0) {
        reply.code(400).send({ ok: false, error: "maxCost must be a number of cost units, 0 or more" });
        return;
      }
      const recipient = hasGroup ? { groupId: group!.trim() } : { userId: to!.trim() };
      try {
        const handle = await ctx.clients.north.sendEnvelope(
          {
            fromUserId: principal.sub,
            tenantId: principal.tenant,
            recipient,
            task: { intent, payloadRef: "", requiredSkills: [], priority: "normal", kind: "" },
            delegation: { capabilityToken: "", attenuatedScopes: scopes, maxCostUnits: maxCost },
          },
          principal.token,
        );
        reply.send({ ok: true, envelopeId: handle.envelopeId ?? "" });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { ok: false } });
      }
    },
  );

  // bob's inbox of pending delegations.
  app.get<{ Querystring: { includeAccepted?: string } }>("/inbox", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    try {
      const resp = await ctx.clients.north.listInboxEnvelopes(
        {
          userId: principal.sub,
          tenantId: principal.tenant,
          includeAccepted: req.query.includeAccepted === "true",
          limit: 0,
          cursor: "",
        },
        principal.token,
      );
      reply.send({ envelopes: resp.envelopes ?? [] });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { envelopes: [] } });
    }
  });

  // Users the caller shares a delegatable group with. Discovery-only — authz is
  // re-checked by SendEnvelope when/if the user acts on a mention.
  app.get("/delegatable-users", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    try {
      const resp = await ctx.clients.north.listDelegatableUsers(
        { userId: principal.sub, tenantId: principal.tenant },
        principal.token,
      );
      reply.send({
        users: (resp.users ?? []).map((u) => ({ userId: u.userId, displayName: u.displayName })),
        groups: (resp.groups ?? []).map((g) => ({ groupId: g.groupId, displayName: g.displayName, memberCount: g.memberCount })),
      });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { users: [], groups: [] } });
    }
  });

  // bob accepts/rejects a delegation. Accept spawns a child task owned by bob.
  app.post<{ Params: { id: string }; Body: { accepted?: boolean } }>(
    "/inbox/:id/respond",
    async (req, reply) => {
      const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
      if (!principal) return;
      try {
        const resp = await ctx.clients.north.respondToEnvelope(
          {
            envelopeId: req.params.id,
            userId: principal.sub,
            tenantId: principal.tenant,
            accepted: req.body?.accepted === true,
            reason: "",
          },
          principal.token,
        );
        reply.send({ success: resp.success });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { success: false } });
      }
    },
  );

  // OK-dismiss: mark an inbox envelope DISMISSED (no task spawn, neutral audit).
  app.post<{ Params: { id: string } }>(
    "/inbox/:id/dismiss",
    async (req, reply) => {
      const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
      if (!principal) return;
      try {
        const resp = await ctx.clients.north.dismissEnvelope(
          {
            envelopeId: req.params.id,
            userId: principal.sub,
            tenantId: principal.tenant,
          },
          principal.token,
        );
        reply.send({ success: resp.success });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { success: false } });
      }
    },
  );
}
