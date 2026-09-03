// Workflows: agent-authored kind: Workflow YAML templates — save/run/rate/
// publish/fork/pin/propose/decide/versions/delete.
// Registered by registerWorkflowRoutes(app, ctx) called from src/app.ts at the
// same position these routes previously occupied inline in server.ts.
import type { FastifyInstance } from "fastify";
import { requireUser } from "../auth/require-user.js";
import type { JwksResolver, VerifyOptions } from "../auth/verify.js";
import type { BrokerClients } from "../broker/clients.js";
import { GovernanceBridge, type Approver, type Identity } from "../broker/governance.js";
import type { RateLimitChecker } from "../llm/egress-proxy.js";
import { WorkflowRating } from "../../gen/ts/proto/broker";
import type { Config } from "../config.js";
import type { Logger } from "pino";
import { sendError } from "../http-errors.js";
import { sessionIdOrEmpty } from "./agui.js";
import { log } from "../log.js";

export interface WorkflowsCtx {
  clients: BrokerClients;
  jwksResolver: JwksResolver;
  verifyOpts: VerifyOptions;
  cfg: Config;
  log: Logger;
  // Spend-caps CP3 finding 2: same breaker-wrapped rate-limit checker every
  // other GovernanceBridge construction site injects. Optional so route tests
  // that build a WorkflowsCtx by hand are unaffected — GovernanceBridge
  // defaults to a permissive no-op when omitted.
  rateLimitChecker?: RateLimitChecker;
}

interface SaveWorkflowBody {
  definitionJson?: string;
  name?: string;
  description?: string;
  // edit path: non-empty = new version under this lineage; empty = brand-new workflow
  lineageId?: string;
}

interface RunWorkflowBody {
  inputs?: Record<string, string>;
  // The chat session the caller will file this run's transcript under, so the
  // run's reason-step LLM cost is attributable to it. The webui mints the id
  // before starting the run for exactly this reason. Absent = unattributed,
  // which is what every non-webui caller gets.
  session_id?: string;
}

// Optional pagination query params on the list routes (F19 passthrough).
// Absent limit = 0 = legacy full listing; present limit must be a non-negative
// integer. cursor is opaque and forwarded verbatim.
interface ListQuery {
  limit?: string;
  cursor?: string;
}

function parseListQuery(q: ListQuery): { limit: number; cursor: string } | { error: string } {
  const cursor = typeof q.cursor === "string" ? q.cursor : "";
  if (q.limit === undefined) return { limit: 0, cursor };
  const n = Number(q.limit);
  if (!Number.isInteger(n) || n < 0) {
    return { error: `invalid limit "${q.limit}": expected a non-negative integer` };
  }
  return { limit: n, cursor };
}

interface RateWorkflowBody {
  version?: number;
  rating?: string;
  note?: string;
}

interface PublishWorkflowBody {
  version?: number;
  groupIds?: string[];
}

interface ForkWorkflowBody {
  newName?: string;
}

interface PinWorkflowBody {
  version?: number;
}

interface ProposeWorkflowBody {
  definitionJson?: string;
}

interface DecideWorkflowBody {
  version?: number;
  approved?: boolean;
  reason?: string;
}

// runIdentityFor decides the run identity from BeginWorkflowRun's result.
// A bound workflow returns a broker-minted owner grant + the bound agent UUID:
// run it agent-bound with NO token, so every bridge call takes the south/
// ownerGrant path — gate() uses CreateGatewayTask with the agent_id and
// InvokeTool passes the agent's MCP can_access check. An unbound (personal)
// workflow returns empty fields: run it on the legacy personal token path,
// gated by the user's own skills (agentId=sub is unused when the token is set).
export function runIdentityFor(
  principal: { token: string; tenant: string; sub: string },
  begin: { ownerGrant: string; boundAgentId: string },
): Identity {
  if (begin.boundAgentId) {
    return {
      tenantId: principal.tenant,
      userId: principal.sub,
      agentId: begin.boundAgentId,
      ownerGrant: begin.ownerGrant,
    };
  }
  return {
    token: principal.token,
    tenantId: principal.tenant,
    userId: principal.sub,
    agentId: principal.sub,
  };
}

export function registerWorkflowRoutes(app: FastifyInstance, ctx: WorkflowsCtx): void {
  app.get<{ Querystring: ListQuery }>("/workflows", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    const page = parseListQuery(req.query ?? {});
    if ("error" in page) {
      reply.code(400).send({ error: page.error, workflows: [] });
      return;
    }
    try {
      const resp = await ctx.clients.north.listWorkflows(
        { tenantId: principal.tenant, ownerGrant: "", userId: principal.sub, limit: page.limit, cursor: page.cursor },
        principal.token,
      );
      reply.send({
        workflows: resp.items ?? [],
        nextCursor: resp.nextCursor,
        sharedUnavailable: resp.sharedUnavailable,
      });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { workflows: [] } });
    }
  });

  app.get<{ Params: { lineageId: string } }>("/workflows/:lineageId", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    try {
      const resp = await ctx.clients.north.getWorkflow(
        {
          tenantId: principal.tenant,
          ownerGrant: "",
          userId: principal.sub,
          lineageId: req.params.lineageId,
        },
        principal.token,
      );
      reply.send({ definitionJson: resp.definitionJson, version: resp.version });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.post<{ Body: SaveWorkflowBody }>("/workflows", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    const b = req.body ?? {};
    try {
      const resp = await ctx.clients.north.saveWorkflow(
        {
          tenantId: principal.tenant,
          ownerUserId: principal.sub,
          ownerGrant: "",
          name: b.name ?? "",
          description: b.description ?? "",
          definitionJson: b.definitionJson ?? "",
          visibilityKind: "private",
          lineageId: b.lineageId ?? "",
          // F9: direct webui saves have no agent session — always personal
          // (unbound). Agent binding only comes from the Pi tool path via
          // GovernanceBridge.saveWorkflow.
          agentId: "",
        },
        principal.token,
      );
      reply.send({ workflowId: resp.workflowId, lineageId: resp.lineageId, version: resp.version });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.post<{ Params: { lineageId: string }; Body: RateWorkflowBody }>(
    "/workflows/:lineageId/rate",
    async (req, reply) => {
      const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
      if (!principal) return;
      const b = req.body ?? {};
      try {
        await ctx.clients.north.rateWorkflowRun(
          {
            tenantId: principal.tenant,
            ownerUserId: principal.sub,
            ownerGrant: "",
            lineageId: req.params.lineageId,
            version: b.version ?? 0,
            rating: b.rating === "RATING_SUCCESS"
              ? WorkflowRating.RATING_SUCCESS
              : b.rating === "RATING_BAD"
                ? WorkflowRating.RATING_BAD
                : WorkflowRating.WORKFLOW_RATING_UNSPECIFIED,
            note: b.note ?? "",
          },
          principal.token,
        );
        reply.send({ ok: true });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  app.post<{ Params: { lineageId: string }; Body: RunWorkflowBody; Querystring: { stream?: string } }>(
    "/workflows/:lineageId/run",
    async (req, reply) => {
      const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
      if (!principal) return;
      // Ask the broker whether this workflow is agent-bound. A bound workflow
      // returns an owner grant + the bound agent UUID → run agent-bound (south/
      // ownerGrant path, gated by the AGENT's skills). An unbound workflow returns
      // empty fields → run on the legacy personal token path (gated by the USER's
      // own skills). A PermissionDenied (caller may not operate the bound agent)
      // flows through the catch → sendError → HTTP 403. HITL is denied for
      // webui-initiated runs — a step needing step-up halts with a clear reason
      // rather than blocking the HTTP response indefinitely.
      const denyHitl: Approver = async () => false;

      // Attribution only — a reason step's usage row carries this so the webui's
      // per-session usage strip can find it. Never an authorization input.
      const usageSessionId = sessionIdOrEmpty(req.body?.session_id);

      // ?stream=1 opts into SSE: one `step` event per step as it settles, then a
      // terminal `result` event carrying the exact JSON the blocking path
      // returns. Without it, the blocking JSON response is unchanged.
      const wantStream = req.query?.stream === "1";

      if (!wantStream) {
        try {
          const begin = await ctx.clients.north.beginWorkflowRun(
            {
              tenantId: principal.tenant,
              ownerUserId: principal.sub,
              lineageId: req.params.lineageId,
            },
            principal.token,
          );
          const identity: Identity = runIdentityFor(principal, begin);
          const bridge = new GovernanceBridge(
            ctx.cfg, ctx.clients, identity, denyHitl, ctx.log, ctx.rateLimitChecker,
            undefined, "", usageSessionId,
          );
          const result = await bridge.runWorkflow(req.params.lineageId, req.body?.inputs ?? {});
          reply.send(result);
        } catch (err) {
          sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { ok: false } });
        }
        return;
      }

      // Streaming path. beginWorkflowRun can still 403 (caller may not operate
      // the bound agent) — do it BEFORE the SSE hijack so a non-2xx status is
      // still possible. Everything after reply.hijack() is SSE frames only.
      let begin;
      try {
        begin = await ctx.clients.north.beginWorkflowRun(
          {
            tenantId: principal.tenant,
            ownerUserId: principal.sub,
            lineageId: req.params.lineageId,
          },
          principal.token,
        );
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { ok: false } });
        return;
      }

      const identity: Identity = runIdentityFor(principal, begin);
      const bridge = new GovernanceBridge(
        ctx.cfg, ctx.clients, identity, denyHitl, ctx.log, ctx.rateLimitChecker,
        undefined, "", usageSessionId,
      );

      reply.raw.writeHead(200, {
        "content-type": "text/event-stream",
        "cache-control": "no-cache",
        connection: "keep-alive",
        "access-control-allow-origin": "*",
      });
      reply.hijack();
      reply.raw.write("retry: 3000\n\n");
      const writeEvent = (event: string, data: unknown): void => {
        reply.raw.write(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`);
      };

      try {
        const result = await bridge.runWorkflow(req.params.lineageId, req.body?.inputs ?? {}, (o) => {
          writeEvent("step", {
            index: o.stepIndex,
            skill: o.skill,
            ok: o.allowed && o.error === undefined,
            ...(o.denyReason !== undefined ? { denyReason: o.denyReason } : {}),
          });
        });
        writeEvent("result", result);
      } catch (err) {
        // Post-hijack: no status code left to send — surface as a result frame.
        log.warn({ err: String(err), route: `${req.method} ${req.url}` }, "workflow run stream failed");
        writeEvent("result", { ok: false, error: String(err) });
      } finally {
        reply.raw.end();
      }
    },
  );

  app.post<{ Params: { lineageId: string }; Body: PublishWorkflowBody }>(
    "/workflows/:lineageId/publish",
    async (req, reply) => {
      const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
      if (!principal) return;
      const b = req.body ?? {};
      try {
        const resp = await ctx.clients.north.publishWorkflow(
          {
            tenantId: principal.tenant,
            ownerUserId: principal.sub,
            ownerGrant: "",
            lineageId: req.params.lineageId,
            version: b.version ?? 0,
            groupIds: b.groupIds ?? [],
          },
          principal.token,
        );
        reply.send({ visibilityKind: resp.visibilityKind, groups: resp.groups ?? [] });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  app.post<{ Params: { lineageId: string }; Body: ForkWorkflowBody }>(
    "/workflows/:lineageId/fork",
    async (req, reply) => {
      const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
      if (!principal) return;
      const b = req.body ?? {};
      try {
        const resp = await ctx.clients.north.forkWorkflow(
          {
            tenantId: principal.tenant,
            ownerUserId: principal.sub,
            ownerGrant: "",
            sourceLineageId: req.params.lineageId,
            newName: b.newName ?? "",
          },
          principal.token,
        );
        reply.send({ lineageId: resp.lineageId });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  app.post<{ Params: { lineageId: string }; Body: PinWorkflowBody }>(
    "/workflows/:lineageId/pin",
    async (req, reply) => {
      const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
      if (!principal) return;
      const b = req.body ?? {};
      try {
        await ctx.clients.north.setWorkflowVersionPin(
          {
            tenantId: principal.tenant,
            ownerUserId: principal.sub,
            ownerGrant: "",
            lineageId: req.params.lineageId,
            version: b.version ?? 0,
          },
          principal.token,
        );
        reply.send({ ok: true });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  app.delete<{ Params: { lineageId: string } }>(
    "/workflows/:lineageId/pin",
    async (req, reply) => {
      const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
      if (!principal) return;
      try {
        await ctx.clients.north.clearWorkflowVersionPin(
          {
            tenantId: principal.tenant,
            ownerUserId: principal.sub,
            ownerGrant: "",
            lineageId: req.params.lineageId,
          },
          principal.token,
        );
        reply.send({ ok: true });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  app.post<{ Params: { lineageId: string }; Body: ProposeWorkflowBody }>(
    "/workflows/:lineageId/propose",
    async (req, reply) => {
      const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
      if (!principal) return;
      const b = req.body ?? {};
      try {
        const resp = await ctx.clients.north.proposeWorkflowVersion(
          {
            tenantId: principal.tenant,
            ownerUserId: principal.sub,
            ownerGrant: "",
            lineageId: req.params.lineageId,
            definitionJson: b.definitionJson ?? "",
          },
          principal.token,
        );
        reply.send({ version: resp.version });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  app.post<{ Params: { lineageId: string }; Body: DecideWorkflowBody }>(
    "/workflows/:lineageId/decide",
    async (req, reply) => {
      const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
      if (!principal) return;
      const b = req.body ?? {};
      try {
        const resp = await ctx.clients.north.decideWorkflowVersion(
          {
            tenantId: principal.tenant,
            ownerUserId: principal.sub,
            ownerGrant: "",
            lineageId: req.params.lineageId,
            version: b.version ?? 0,
            approved: b.approved ?? false,
            reason: b.reason ?? "",
          },
          principal.token,
        );
        reply.send({ approvalState: resp.approvalState });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  app.get<{ Params: { lineageId: string }; Querystring: ListQuery }>(
    "/workflows/:lineageId/versions",
    async (req, reply) => {
      const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
      if (!principal) return;
      const page = parseListQuery(req.query ?? {});
      if ("error" in page) {
        reply.code(400).send({ error: page.error, versions: [] });
        return;
      }
      try {
        const resp = await ctx.clients.north.listWorkflowVersions(
          {
            tenantId: principal.tenant,
            ownerUserId: principal.sub,
            ownerGrant: "",
            lineageId: req.params.lineageId,
            limit: page.limit,
            cursor: page.cursor,
          },
          principal.token,
        );
        reply.send({ versions: resp.items ?? [], nextCursor: resp.nextCursor });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { versions: [] } });
      }
    },
  );

  // Delete an entire workflow lineage (owner-only, enforced by the broker).
  // Deleting a published lineage withdraws it from every group it was shared with;
  // the webui confirms that consequence before calling.
  app.delete<{ Params: { lineageId: string } }>("/workflows/:lineageId", async (req, reply) => {
    const principal = await requireUser(req, reply, ctx.jwksResolver, ctx.verifyOpts);
    if (!principal) return;
    try {
      const resp = await ctx.clients.north.deleteWorkflow(
        {
          tenantId: principal.tenant,
          ownerUserId: principal.sub,
          ownerGrant: "",
          lineageId: req.params.lineageId,
        },
        principal.token,
      );
      reply.send({ ok: true, versionsDeleted: resp.versionsDeleted });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { ok: false } });
    }
  });
}
