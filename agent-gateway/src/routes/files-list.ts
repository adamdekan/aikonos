// GET /files route: scoped directory listing (CP2 of scoped-file-listing).
// Extracted from server.ts so tests register this exact handler instead of a
// hand-copied stand-in — a mutation to the real route must fail the real test.
import type { FastifyInstance } from "fastify";
import { requireUser } from "../auth/require-user.js";
import type { JwksResolver, VerifyOptions } from "../auth/verify.js";
import { filterFiles, fileJson } from "../files-filter.js";
import type { NorthClient } from "../broker/north.js";
import { sendError } from "../http-errors.js";
import { log } from "../log.js";

export interface FilesListCtx {
  clients: { north: Pick<NorthClient, "listWorkspaceFiles"> };
  jwksResolver: JwksResolver;
  verifyOpts: VerifyOptions;
}

export function registerFilesListRoute(app: FastifyInstance, ctx: FilesListCtx): void {
  app.get<{ Querystring: { includeHidden?: string; dir?: string; recursive?: string } }>(
    "/files",
    async (req, reply) => {
      const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
      if (!principal) return;
      try {
        const rec = req.query.recursive;
        const resp = await ctx.clients.north.listWorkspaceFiles(
          {
            tenantId: principal.tenant,
            userId: principal.sub,
            path: req.query.dir ?? "",
            recursive: rec === "1" || rec === "true",
          },
          principal.token,
        );
        const ih = req.query.includeHidden;
        const includeHidden = ih === "1" || ih === "true";
        const mapped = (resp.files ?? []).map(fileJson);
        reply.send({ files: filterFiles(mapped, includeHidden) });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { files: [] } });
      }
    },
  );
}
