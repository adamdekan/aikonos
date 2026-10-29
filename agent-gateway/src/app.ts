// Builds the internal Fastify app (all HTTP surfaces except the external
// :8090 API — see src/external/server.ts). Pure composition: registers every
// route group against the supplied ctx with zero module-scope side effects.
// src/server.ts is the only caller; it owns config load, client construction,
// and process bootstrap.
import Fastify, { type FastifyInstance } from "fastify";
import type { Logger } from "pino";
import type { JwksResolver, VerifyOptions } from "./auth/verify.js";
import type { BrokerClients } from "./broker/clients.js";
import type { RateLimitChecker } from "./llm/egress-proxy.js";
import type { Config } from "./config.js";
import { ApprovalRegistry } from "./agui/hitl.js";
import { ChildSupervisor } from "./ipc/supervisor.js";
import { registerAuditRoutes, type AuditConsumerHandle } from "./audit/stream.js";
import { evaluateReadyz } from "./readyz.js";
import { registerAdminRoutes } from "./routes/admin.js";
import { registerAgUiRoutes } from "./routes/agui.js";
import { registerApprovalRoutes } from "./routes/approvals.js";
import { registerFilesListRoute } from "./routes/files-list.js";
import { registerDelegationRoutes } from "./routes/delegation.js";
import { registerConnectorRoutes } from "./routes/connectors.js";
import { registerWorkspacePrefsRoutes } from "./routes/workspace-prefs.js";
import { registerSessionUsageRoutes } from "./routes/session-usage.js";
import { registerMemoryRoutes } from "./routes/memory.js";
import { registerScheduleRoutes } from "./routes/schedules.js";
import { registerWorkflowRoutes } from "./routes/workflows.js";
import { registerFilesRoutes } from "./routes/files.js";
import { registerAgentsRoutes } from "./routes/agents.js";
import { registerSkillsRoutes } from "./routes/skills.js";

export interface AppCtx {
  clients: BrokerClients;
  jwksResolver: JwksResolver;
  verifyOpts: VerifyOptions;
  approvals: ApprovalRegistry;
  supervisor: ChildSupervisor;
  cfg: Config;
  log: Logger;
  auditConsumer: AuditConsumerHandle;
  // Spend-caps CP3: the breaker-wrapped rate-limit checker every other
  // GovernanceBridge construction site injects (server.ts's ChildSupervisor
  // factory, scheduler/ticker.ts). Optional so existing tests that build an
  // AppCtx by hand are unaffected — GovernanceBridge defaults to a permissive
  // no-op when omitted.
  rateLimitChecker?: RateLimitChecker;
}

export function buildApp(ctx: AppCtx): FastifyInstance {
  // F57: bound how long one request may take to arrive (headers + body), so a
  // slow-sent 14 MiB upload cannot hold a connection open indefinitely —
  // Fastify's own default is no limit. Responses (the /agui SSE stream) are
  // not affected.
  const app = Fastify({ logger: false, requestTimeout: 120_000 });

  app.get("/healthz", async () => ({ ok: true }));

  // /readyz: 200 only when the broker channel is READY/IDLE-connectable and
  // the audit consumer is connected-or-disabled. Distinct from /healthz
  // (static liveness) — this is a dependency-aware readiness check meant for
  // orchestrator restart/routing decisions, not "is the process alive".
  app.get("/readyz", async (_req, reply) => {
    const result = evaluateReadyz({
      brokerState: () => ctx.clients.north.getConnectivityState(false),
      auditStatus: () => ctx.auditConsumer.status(),
    });
    if (result.ok) {
      reply.send({ ok: true, checks: result.checks });
    } else {
      reply.code(503).send({ ok: false, checks: result.checks });
    }
  });

  registerAuditRoutes(app, { jwksResolver: ctx.jwksResolver, verifyOpts: ctx.verifyOpts, north: ctx.clients.north });
  // F65: no GET / — the legacy unauthenticated demo UI is gone (the webui
  // supersedes it), so / is a plain 404.

  // POST /approve/:id + GET /approvals: the caller's own pending approvals.
  // GET /pending-approvals + POST /pending-approvals/:taskId: broker-held ones.
  registerApprovalRoutes(app, {
    approvals: ctx.approvals,
    clients: ctx.clients,
    jwksResolver: ctx.jwksResolver,
    verifyOpts: ctx.verifyOpts,
  });

  registerDelegationRoutes(app, { clients: ctx.clients, jwksResolver: ctx.jwksResolver, verifyOpts: ctx.verifyOpts });
  registerConnectorRoutes(app, { clients: ctx.clients, jwksResolver: ctx.jwksResolver, verifyOpts: ctx.verifyOpts });
  registerWorkspacePrefsRoutes(app, { clients: ctx.clients, jwksResolver: ctx.jwksResolver, verifyOpts: ctx.verifyOpts });
  registerSessionUsageRoutes(app, { clients: ctx.clients, jwksResolver: ctx.jwksResolver, verifyOpts: ctx.verifyOpts });
  registerMemoryRoutes(app, { clients: ctx.clients, jwksResolver: ctx.jwksResolver, verifyOpts: ctx.verifyOpts });
  registerScheduleRoutes(app, { clients: ctx.clients, jwksResolver: ctx.jwksResolver, verifyOpts: ctx.verifyOpts });
  registerWorkflowRoutes(app, {
    clients: ctx.clients,
    jwksResolver: ctx.jwksResolver,
    verifyOpts: ctx.verifyOpts,
    cfg: ctx.cfg,
    log: ctx.log,
    rateLimitChecker: ctx.rateLimitChecker,
  });

  registerFilesListRoute(app, { clients: ctx.clients, jwksResolver: ctx.jwksResolver, verifyOpts: ctx.verifyOpts });
  registerFilesRoutes(app, { clients: ctx.clients, jwksResolver: ctx.jwksResolver, verifyOpts: ctx.verifyOpts });
  registerAgentsRoutes(app, { clients: ctx.clients, jwksResolver: ctx.jwksResolver, verifyOpts: ctx.verifyOpts, supervisor: ctx.supervisor });
  registerSkillsRoutes(app, { clients: ctx.clients, jwksResolver: ctx.jwksResolver, verifyOpts: ctx.verifyOpts });

  // ── Admin routes ─────────────────────────────────────────────────────────────
  // All /admin/* endpoints + /agents/:id/mcp-servers.
  registerAdminRoutes(app, {
    clients: ctx.clients,
    jwksResolver: ctx.jwksResolver,
    verifyOpts: ctx.verifyOpts,
    supervisor: ctx.supervisor,
    scimBaseUrl: ctx.cfg.externalBaseUrl ? `${ctx.cfg.externalBaseUrl}/scim/v2` : "",
  });

  // ── AG-UI SSE run endpoint ────────────────────────────────────────────────────
  registerAgUiRoutes(app, {
    clients: ctx.clients,
    jwksResolver: ctx.jwksResolver,
    verifyOpts: ctx.verifyOpts,
    approvals: ctx.approvals,
    supervisor: ctx.supervisor,
    cfg: ctx.cfg,
    log: ctx.log,
    rateLimitChecker: ctx.rateLimitChecker,
  });

  return app;
}
