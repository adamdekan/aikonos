// Connectors: per-user Google Drive / OneDrive OAuth.
// Browser → /api/gw proxy → here → broker north RPCs. The broker runs the OAuth
// exchange + Vault storage; the gateway just forwards with the user's token.
// Registered by registerConnectorRoutes(app, ctx) called from src/app.ts at the
// same position these routes previously occupied inline in server.ts.
import type { FastifyInstance } from "fastify";
import { requireUser } from "../auth/require-user.js";
import type { JwksResolver, VerifyOptions } from "../auth/verify.js";
import type { NorthClient } from "../broker/north.js";
import { ConnectorProvider } from "../../gen/ts/proto/broker";
import { sendError } from "../http-errors.js";
import { log } from "../log.js";

export interface ConnectorsCtx {
  clients: {
    north: Pick<
      NorthClient,
      "beginConnectorAuth" | "completeConnectorAuth" | "listConnectors" | "listConnectorProviders" | "revokeConnector"
    >;
  };
  jwksResolver: JwksResolver;
  verifyOpts: VerifyOptions;
}

const PROVIDER_BY_KEY: Record<string, ConnectorProvider> = {
  google_drive: ConnectorProvider.GOOGLE_DRIVE,
  gdrive: ConnectorProvider.GOOGLE_DRIVE,
  onedrive: ConnectorProvider.ONEDRIVE,
};

export function registerConnectorRoutes(app: FastifyInstance, ctx: ConnectorsCtx): void {
  app.post<{ Body: { provider?: string } }>("/connectors/begin", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    const provider = PROVIDER_BY_KEY[(req.body?.provider ?? "").toLowerCase()];
    if (provider === undefined) {
      reply.code(400).send({ error: `unknown provider: ${req.body?.provider}` });
      return;
    }
    try {
      const resp = await ctx.clients.north.beginConnectorAuth(
        { tenantId: principal.tenant, userId: principal.sub, provider, scopes: [] },
        principal.token,
      );
      reply.send({ authorizeUrl: resp.authorizeUrl, state: resp.state });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.post<{ Body: { code?: string; state?: string } }>("/connectors/complete", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    try {
      const resp = await ctx.clients.north.completeConnectorAuth(
        { tenantId: principal.tenant, userId: principal.sub, code: req.body?.code ?? "", state: req.body?.state ?? "" },
        principal.token,
      );
      reply.send({ connectorId: resp.connectorId, status: resp.status, scopes: resp.scopes });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.get("/connectors", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    try {
      const resp = await ctx.clients.north.listConnectors({ tenantId: principal.tenant, userId: principal.sub }, principal.token);
      reply.send({ connectors: resp.connectors ?? [] });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { connectors: [] } });
    }
  });

  app.get("/connectors/providers", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    try {
      const resp = await ctx.clients.north.listConnectorProviders({}, principal.token);
      reply.send({ providers: resp.providers ?? [] });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { providers: [] } });
    }
  });

  app.post<{ Params: { id: string } }>("/connectors/:id/revoke", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    try {
      const resp = await ctx.clients.north.revokeConnector(
        { tenantId: principal.tenant, userId: principal.sub, connectorId: req.params.id },
        principal.token,
      );
      reply.send({ success: resp.success });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { success: false } });
    }
  });
}
