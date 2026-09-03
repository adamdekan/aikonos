// Workspace-backend preference: which storage backend (local disk vs the
// tenant's OneDrive OBO connection) the caller's Files explorer/composer/
// agent tools currently route to.
// Browser → /api/gw proxy → here → broker north RPCs.
import type { FastifyInstance } from "fastify";
import { requireUser } from "../auth/require-user.js";
import type { JwksResolver, VerifyOptions } from "../auth/verify.js";
import type { NorthClient } from "../broker/north.js";
import { sendError } from "../http-errors.js";
import { log } from "../log.js";

export interface WorkspacePrefsCtx {
  clients: {
    north: Pick<NorthClient, "getWorkspaceBackend" | "setWorkspaceBackend" | "listOneDriveFolders">;
  };
  jwksResolver: JwksResolver;
  verifyOpts: VerifyOptions;
}

export function registerWorkspacePrefsRoutes(app: FastifyInstance, ctx: WorkspacePrefsCtx): void {
  app.get("/workspace/backend", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    try {
      const resp = await ctx.clients.north.getWorkspaceBackend(
        { tenantId: principal.tenant, userId: principal.sub },
        principal.token,
      );
      reply.send({
        pref: resp.pref ?? { backend: "local", onedriveFolderPath: "" },
        onedriveAvailable: resp.onedriveAvailable,
        onedriveStatus: resp.onedriveStatus,
      });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.put<{ Body: { backend?: string; onedriveFolderPath?: string } }>(
    "/workspace/backend",
    async (req, reply) => {
      const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
      if (!principal) return;
      try {
        const resp = await ctx.clients.north.setWorkspaceBackend(
          {
            tenantId: principal.tenant,
            userId: principal.sub,
            backend: req.body?.backend ?? "",
            onedriveFolderPath: req.body?.onedriveFolderPath ?? "",
          },
          principal.token,
        );
        reply.send({ pref: resp.pref ?? { backend: "local", onedriveFolderPath: "" } });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  app.get<{ Querystring: { dir?: string } }>("/workspace/onedrive/folders", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    try {
      const resp = await ctx.clients.north.listOneDriveFolders(
        { tenantId: principal.tenant, userId: principal.sub, path: req.query.dir ?? "" },
        principal.token,
      );
      reply.send({ folders: resp.folders ?? [] });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { folders: [] } });
    }
  });
}
