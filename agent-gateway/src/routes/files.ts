// Workspace files: per-user browser file explorer (remaining routes; GET
// /files itself lives in routes/files-list.ts).
// The acting user (the verified OIDC principal) operates on their own workspace; the broker
// binds it to the verified OIDC subject. File bytes travel as base64 in JSON so
// the existing JSON-only proxy chain needs no multipart handling.
// Registered by registerFilesRoutes(app, ctx) called from src/app.ts at the
// same position these routes previously occupied inline in server.ts.
import type { FastifyInstance } from "fastify";
import { requireUser } from "../auth/require-user.js";
import type { JwksResolver, VerifyOptions } from "../auth/verify.js";
import type { NorthClient } from "../broker/north.js";
import { fileJson } from "../files-filter.js";
import { sendError } from "../http-errors.js";
import { log } from "../log.js";

export interface FilesCtx {
  clients: {
    north: Pick<
      NorthClient,
      "readWorkspaceFile" | "uploadWorkspaceFile" | "deleteWorkspaceFile" | "moveWorkspaceFile" | "createWorkspaceDir"
    >;
  };
  jwksResolver: JwksResolver;
  verifyOpts: VerifyOptions;
}

// 10 MiB file → ~13.98 MiB base64 + JSON envelope; keep the broker's 10 MiB
// workspacefs cap the real limit. Exported so routes/skills.ts's POST
// /skills/import mirrors the same
// cap instead of picking its own number.
export const FILE_UPLOAD_BODY_LIMIT = 14 * 1024 * 1024;

export function registerFilesRoutes(app: FastifyInstance, ctx: FilesCtx): void {
  app.get<{ Querystring: { path?: string } }>("/files/content", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    try {
      const resp = await ctx.clients.north.readWorkspaceFile(
        { tenantId: principal.tenant, userId: principal.sub, path: req.query.path ?? "" },
        principal.token,
      );
      reply.send({
        path: resp.path,
        mime: resp.mimeType,
        contentBase64: Buffer.from(resp.content ?? new Uint8Array()).toString("base64"),
      });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.post<{ Body: { path?: string; contentBase64?: string } }>(
    "/files",
    { bodyLimit: FILE_UPLOAD_BODY_LIMIT },
    async (req, reply) => {
      const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
      if (!principal) return;
      const b = req.body ?? {};
      try {
        const resp = await ctx.clients.north.uploadWorkspaceFile(
          {
            tenantId: principal.tenant,
            userId: principal.sub,
            path: b.path ?? "",
            content: Buffer.from(b.contentBase64 ?? "", "base64"),
          },
          principal.token,
        );
        reply.send({ file: resp.file ? fileJson(resp.file) : null });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  app.delete<{ Querystring: { path?: string } }>("/files", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    try {
      const resp = await ctx.clients.north.deleteWorkspaceFile(
        { tenantId: principal.tenant, userId: principal.sub, path: req.query.path ?? "" },
        principal.token,
      );
      reply.send({ success: resp.success });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { success: false } });
    }
  });

  app.post<{ Body: { from?: string; to?: string } }>("/files/move", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    const b = req.body ?? {};
    try {
      const resp = await ctx.clients.north.moveWorkspaceFile(
        {
          tenantId: principal.tenant,
          userId: principal.sub,
          fromPath: b.from ?? "",
          toPath: b.to ?? "",
        },
        principal.token,
      );
      reply.send({ file: resp.file ? fileJson(resp.file) : null });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.post<{ Body: { path?: string } }>("/files/dir", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    const b = req.body ?? {};
    try {
      const resp = await ctx.clients.north.createWorkspaceDir(
        { tenantId: principal.tenant, userId: principal.sub, path: b.path ?? "" },
        principal.token,
      );
      reply.send({ success: resp.success });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });
}
