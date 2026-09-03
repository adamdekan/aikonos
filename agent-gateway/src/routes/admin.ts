// Admin route group — all /admin/* endpoints plus /agents/:id/mcp-servers.
// Registered by registerAdminRoutes(app, ctx) called from server.ts at the
// same position these routes previously occupied inline.
import type { FastifyInstance } from "fastify";
import {
  AssignmentSection,
  type RoleTuple,
  type McpConnection,
  type Agent,
  type LlmProvider,
  type OrgSettings,
  type M365Connection,
  type WebSearchConfig,
} from "../../gen/ts/proto/broker";
import { PolicyDecision } from "../../gen/ts/proto/audit";
import { EffectClass } from "../../gen/ts/proto/plan";
import { requireUser } from "../auth/require-user.js";
import type { BrokerClients } from "../broker/clients";
import type { VerifyOptions, JwksResolver } from "../auth/verify.js";
import { parseSkillMd } from "../pi/skill-parser.js";
import { scheduleJson } from "../schedule-json.js";
import { sendError } from "../http-errors.js";
import { log } from "../log.js";

export interface AdminCtx {
  clients: BrokerClients;
  jwksResolver: JwksResolver;
  verifyOpts: VerifyOptions;
}

// ── helpers ──────────────────────────────────────────────────────────────────

// orgSettingsJson maps the proto OrgSettings message to the plain JSON the webui
// consumes. Optional scalar fields default to their documented empty value so the
// client never has to distinguish unset from empty.
function orgSettingsJson(settings: OrgSettings | undefined) {
  return {
    instructionPreamble: settings?.instructionPreamble ?? "",
    unattendedAllowed: settings?.unattendedAllowed ?? true,
    workflowSharingAllowed: settings?.workflowSharingAllowed ?? true,
    connectorAllowlistEnabled: settings?.connectorAllowlistEnabled ?? false,
    connectorAllowlist: settings?.connectorAllowlist ?? [],
    disabledEffectClasses: settings?.capabilities?.disabledEffectClasses ?? [],
    updatedBy: settings?.updatedBy ?? "",
    updatedAt: settings?.updatedAt ?? "",
  };
}

// m365Json maps the M365Connection proto message (or an absent/zero-value
// connection) to the plain JSON the webui panel consumes — the client-side
// contract mirrors orgSettingsJson's undefined-tolerant shape.
function m365Json(c: M365Connection | undefined) {
  return {
    entraTenantId: c?.entraTenantId ?? "",
    clientId: c?.clientId ?? "",
    hasSecret: c?.hasSecret ?? false,
    enabled: c?.enabled ?? false,
    updatedBy: c?.updatedBy ?? "",
    updatedAt: c?.updatedAt ?? "",
  };
}

// webSearchJson maps the WebSearchConfig proto message (or an absent/zero-value
// config) to the plain JSON the webui panel consumes — has_key only, never the
// key itself, mirroring m365Json's undefined-tolerant shape.
function webSearchJson(c: WebSearchConfig | undefined) {
  return {
    engine: c?.engine ?? "",
    maxResults: c?.maxResults ?? 0,
    hasKey: c?.hasKey ?? false,
    updatedBy: c?.updatedBy ?? "",
    updatedAt: c?.updatedAt ?? "",
  };
}

interface NetworkRuleJson {
  id: string;
  scopeKind: string;
  scopeValue: string;
  action: string;
  hostPattern: string;
  note: string;
  createdBy: string;
  createdAt?: Date;
}

function netRuleJson(r: NetworkRuleJson) {
  return {
    id: r.id,
    scopeKind: r.scopeKind,
    scopeValue: r.scopeValue,
    action: r.action,
    hostPattern: r.hostPattern,
    note: r.note,
    createdBy: r.createdBy,
    createdAt: r.createdAt ? r.createdAt.toISOString() : null,
  };
}

interface ProvisioningRuleJson {
  id: string;
  matcher: string;
  groups: string[];
  createdBy: string;
  // ts-proto emits Timestamp fields as string (ISO-8601); accept string | Date
  // so this interface is compatible with both the generated proto type and tests.
  createdAt?: string | Date;
}

function provRuleJson(r: ProvisioningRuleJson) {
  const createdAt = r.createdAt instanceof Date ? r.createdAt.toISOString() : (r.createdAt ?? null);
  return {
    id: r.id,
    matcher: r.matcher,
    groups: r.groups,
    createdBy: r.createdBy,
    createdAt,
  };
}

function mcpConnectionJson(c: McpConnection) {
  return {
    id: c.id,
    name: c.name,
    url: c.url,
    transport: c.transport,
    authType: c.authType,
    authSecretRef: c.authSecretRef,
    createdBy: c.createdBy,
    createdAt: c.createdAt ? c.createdAt.toISOString() : null,
  };
}

export function agentJson(a: Agent) {
  return {
    id: a.id,
    name: a.name,
    llmModel: a.llmModel,
    approvalMode: a.approvalMode,
    skills: a.skills ?? [],
    mcpServers: a.mcpServers ?? [],
    usableBy: a.usableBy ?? [],
    createdBy: a.createdBy,
    createdAt: a.createdAt ? a.createdAt.toISOString() : null,
    allowedProviders: a.allowedProviders ?? [],
    preferredProvider: a.preferredProvider ?? "",
    soul: a.soul ?? "",
    gatewayEnabled: a.gatewayEnabled ?? false,
  };
}

// normalizeRoleTuple builds a wire-safe RoleTuple from an untrusted request body.
// section is a curated UI hint the broker ignores (it derives the section from the
// object), but it is an enum (int32) on the gRPC wire — an omitted section would
// serialize as `undefined` and crash the north client. Default it to UNSPECIFIED.
export function normalizeRoleTuple(
  t: Partial<RoleTuple> & { user: string; relation: string; object: string },
): RoleTuple {
  return {
    user: t.user,
    relation: t.relation,
    object: t.object,
    section: t.section ?? AssignmentSection.ASSIGNMENT_SECTION_UNSPECIFIED,
  };
}

export function agentInputFromBody(id: string, b: AgentBody) {
  return {
    id,
    name: b.name ?? "",
    llmModel: b.llmModel ?? "",
    approvalMode: b.approvalMode ?? "needs_approval",
    skills: b.skills ?? [],
    mcpServers: b.mcpServers ?? [],
    allowedProviders: b.allowedProviders ?? [],
    preferredProvider: b.preferredProvider ?? "",
    // Soul is managed via the dedicated /agents/:id/soul endpoint; UpdateAgent does not write the soul column.
    soul: "",
    gatewayEnabled: b.gatewayEnabled ?? false,
    usableBy: [],
    createdBy: "",
    createdAt: undefined,
    updatedAt: undefined,
  };
}

const EFFECT_CLASS_BY_STR: Record<string, EffectClass> = {
  READ_ONLY:          EffectClass.READ_ONLY,
  WRITE_LOCAL:        EffectClass.WRITE_LOCAL,
  WRITE_EXTERNAL:     EffectClass.WRITE_EXTERNAL,
  NETWORK_EGRESS:     EffectClass.NETWORK_EGRESS,
  CREDENTIAL_ACCESS:  EffectClass.CREDENTIAL_ACCESS,
  DESTRUCTIVE:        EffectClass.DESTRUCTIVE,
  INFRASTRUCTURE:     EffectClass.INFRASTRUCTURE,
};

const DECISION_BY_STR: Record<string, PolicyDecision> = {
  "1": PolicyDecision.ALLOW,
  "2": PolicyDecision.DENY,
  "3": PolicyDecision.APPROVAL_REQUIRED,
  "4": PolicyDecision.STEP_UP_REQUIRED,
};

interface McpConnectionBody {
  user?: string;
  name?: string;
  url?: string;
  transport?: string;
  authType?: string;
  bearerToken?: string;
}

export interface AgentBody {
  user?: string;
  name?: string;
  llmModel?: string;
  approvalMode?: string;
  skills?: string[];
  mcpServers?: string[];
  allowedProviders?: string[];
  preferredProvider?: string;
  gatewayEnabled?: boolean;
}

interface SimulateBody {
  user?: string;
  subjectUserId?: string;
  toolId?: string;
  effectClass?: string;
  host?: string;
  readsSensitive?: boolean;
  fgaObject?: string;
}

// ── skill upload ──────────────────────────────────────────────────────────────

const SKILL_UPLOAD_MAX_BYTES = 5 * 1024 * 1024; // 5 MiB gateway cap (spec Risks row 3)

// ── multipart helper ──────────────────────────────────────────────────────────
// Minimal boundary-based extraction of a named file part from a multipart body.
// Used by the /admin/skills/upload route when Content-Type is multipart/form-data,
// and reused by routes/skills.ts's POST /skills/import for the same content-type dispatch — one boundary
// scanner, not a second copy.
export function extractMultipartFile(body: Buffer, boundary: string, fieldname: string): Buffer | null {
  const boundaryBuf = Buffer.from(`--${boundary}`);
  const crlf = Buffer.from("\r\n");
  const headerEnd = Buffer.from("\r\n\r\n");

  let pos = 0;
  while (pos < body.length) {
    const bStart = body.indexOf(boundaryBuf, pos);
    if (bStart === -1) break;
    pos = bStart + boundaryBuf.length;

    // Skip CRLF after boundary marker
    if (body[pos] === 0x0d && body[pos + 1] === 0x0a) pos += 2;
    else if (body[pos] === 0x2d && body[pos + 1] === 0x2d) break; // final boundary

    const hEnd = body.indexOf(headerEnd, pos);
    if (hEnd === -1) break;

    const headerSection = body.toString("utf8", pos, hEnd);
    const nameMatch = headerSection.match(/name="([^"]+)"/);
    const partName = nameMatch?.[1] ?? "";

    const dataStart = hEnd + 4; // skip \r\n\r\n
    const nextBoundary = body.indexOf(boundaryBuf, dataStart);
    const dataEnd = nextBoundary === -1 ? body.length : nextBoundary - crlf.length;

    if (partName === fieldname) {
      return body.subarray(dataStart, dataEnd);
    }
    pos = dataStart;
  }
  return null;
}

// Minimal interface for the reply object used by handleSkillUpload —
// compatible with both Fastify's real reply and the fake reply in tests.
interface UploadReply {
  statusCode: number;
  code(n: number): UploadReply;
  send(b: unknown): void;
}

// Minimal interface for the request object used by handleSkillUpload —
// compatible with both Fastify's real request and the fake request in tests.
interface UploadReq {
  headers: Record<string, string | string[] | undefined>;
  body?: Buffer;
  query?: { keywords?: string };
}

const MAX_KEYWORDS = 32;
const MAX_KEYWORD_LENGTH = 64;

// Keywords ride as a comma-separated `?keywords=` query param rather than the
// request body: the body on this route is the raw skill content itself (zip/
// markdown/plain/multipart) for every registered content type, so there is no
// JSON envelope to attach a field to. Absent → no auto-load match terms —
// consistent with the rest of the route, which always full-overwrites the
// bundle's fields rather than merging with the existing row.
function parseKeywordsParam(raw: string | undefined): { keywords: string[] } | { error: string } {
  if (raw === undefined) return { keywords: [] };
  const keywords = raw
    .split(",")
    .map((k) => k.trim())
    .filter((k) => k.length > 0);
  if (keywords.length > MAX_KEYWORDS) {
    return { error: `too many keywords: max ${MAX_KEYWORDS}` };
  }
  if (keywords.some((k) => k.length > MAX_KEYWORD_LENGTH)) {
    return { error: `keyword too long: max ${MAX_KEYWORD_LENGTH} characters` };
  }
  return { keywords };
}

/**
 * Core logic for POST /admin/skills/upload, extracted for testability.
 * The route registers this with bodyLimit: SKILL_UPLOAD_MAX_BYTES (5 MiB).
 * Exported so tests can drive the same code path without a live Fastify server.
 */
export async function handleSkillUpload(
  req: UploadReq,
  reply: UploadReply,
  jwksResolver: import("../auth/verify.js").JwksResolver,
  verifyOpts: VerifyOptions,
  north: Pick<BrokerClients["north"], "upsertAgentSkill">,
  bundleId?: string,
): Promise<void> {
  const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
  if (!principal) return;

  const body = req.body ?? Buffer.alloc(0);

  // 5 MiB cap enforced at the route level via bodyLimit; the explicit check
  // here guards callers of the exported function (e.g. tests) that bypass Fastify.
  if (body.length > SKILL_UPLOAD_MAX_BYTES) {
    reply.code(413).send({ error: "payload too large: skill bundle must not exceed 5 MiB" });
    return;
  }

  const rawContentType = (req.headers["content-type"] ?? "") as string;
  const contentType = rawContentType.split(";")[0].trim();

  let parsed: ReturnType<typeof parseSkillMd>;

  if (contentType === "application/zip") {
    try {
      parsed = parseSkillMd(body, { zip: true });
    } catch (err) {
      reply.code(400).send({ error: err instanceof Error ? err.message : String(err) });
      return;
    }
  } else if (contentType === "text/markdown" || contentType === "text/plain") {
    try {
      parsed = parseSkillMd(body, { zip: false });
    } catch (err) {
      reply.code(400).send({ error: err instanceof Error ? err.message : String(err) });
      return;
    }
  } else if (contentType === "multipart/form-data") {
    // Webui posts text/markdown; multipart is a fallback for curl/tooling.
    // @fastify/multipart is not installed — use the minimal boundary scan.
    const boundary = rawContentType.match(/boundary=([^\s;]+)/)?.[1];
    if (!boundary) {
      reply.code(400).send({ error: "multipart/form-data: missing boundary parameter" });
      return;
    }
    const skillBuf = extractMultipartFile(body, boundary, "skill");
    if (!skillBuf) {
      reply.code(400).send({ error: "multipart/form-data: missing file part 'skill'" });
      return;
    }
    try {
      parsed = parseSkillMd(skillBuf, { zip: false });
    } catch (err) {
      reply.code(400).send({ error: err instanceof Error ? err.message : String(err) });
      return;
    }
  } else {
    reply.code(415).send({ error: `Unsupported Media Type: ${contentType}` });
    return;
  }

  const keywordsResult = parseKeywordsParam(req.query?.keywords);
  if ("error" in keywordsResult) {
    reply.code(400).send({ error: keywordsResult.error });
    return;
  }

  try {
    const resp = await north.upsertAgentSkill(
      {
        tenantId: principal.tenant,
        userId: principal.sub,
        id: bundleId ?? "",
        name: parsed.name,
        description: parsed.description,
        body: parsed.body,
        allowedTools: parsed.allowedTools,
        contextFork: parsed.contextFork,
        disableModelInvocation: parsed.disableModelInvocation,
        keywords: keywordsResult.keywords,
        files: parsed.extras,
      },
      principal.token,
    );
    reply.code(201).send({ bundle: resp.bundle ?? null });
  } catch (err) {
    // req is the minimal UploadReq shape (no method/url) so the route is fixed —
    // this helper backs both /admin/skills/upload and PUT /admin/skill-bundles/:id.
    sendError(reply, log, err, { route: "skills/upload" });
  }
}

// ── route registration ────────────────────────────────────────────────────────

export function registerAdminRoutes(app: FastifyInstance, ctx: AdminCtx): void {
  const { clients, jwksResolver, verifyOpts } = ctx;

  // Register buffer body parsers for upload content types so that req.body is
  // a Buffer for the /admin/skills/upload route. Fastify's default JSON parser
  // does not handle these types; addContentTypeParser makes req.body the raw
  // buffer, which the route passes directly to parseSkillMd.
  for (const ct of ["application/zip", "text/markdown", "text/plain", "multipart/form-data"]) {
    app.addContentTypeParser(ct, { parseAs: "buffer" }, (_req, body, done) => {
      done(null, body);
    });
  }

  // ── Admin: role/assignment management ─────────────────────────────────────
  // The acting admin is the verified OIDC principal. The broker enforces the
  // tenant-admin gate; the gateway only forwards (mapping PERMISSION_DENIED → 403).
  app.get("/admin/assignments", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    try {
      const resp = await clients.north.listAssignments({ tenantId: principal.tenant, userId: principal.sub }, principal.token);
      reply.send({
        tuples: resp.tuples ?? [],
        principals: resp.principals ?? [],
        fgaEnabled: resp.fgaEnabled,
        warnings: resp.warnings ?? [],
      });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.post<{ Body: { tuple?: RoleTuple } }>("/admin/assignments", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    if (!req.body?.tuple) {
      reply.code(400).send({ error: "tuple required" });
      return;
    }
    try {
      const resp = await clients.north.assignRole(
        { tenantId: principal.tenant, userId: principal.sub, tuple: normalizeRoleTuple(req.body.tuple) },
        principal.token,
      );
      reply.send({ success: resp.success });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { success: false } });
    }
  });

  app.post<{ Body: { tuple?: RoleTuple } }>("/admin/assignments/revoke", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    if (!req.body?.tuple) {
      reply.code(400).send({ error: "tuple required" });
      return;
    }
    try {
      const resp = await clients.north.revokeRole(
        { tenantId: principal.tenant, userId: principal.sub, tuple: normalizeRoleTuple(req.body.tuple) },
        principal.token,
      );
      reply.send({ success: resp.success });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { success: false } });
    }
  });

  // Read-only tool vocabulary — used to populate skill dropdowns with tools that
  // have no grants yet. Tenant-admin gated in the broker (403 on PERMISSION_DENIED).
  app.get("/admin/skills", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    try {
      const resp = await clients.north.listSkills(
        { tenantId: principal.tenant, userId: principal.sub },
        principal.token,
      );
      reply.send({
        skills: (resp.skills ?? []).map((s) => ({
          toolId: s.toolId,
          scope: s.scope,
          enabled: s.enabled,
          effectClass: s.effectClass,
          displayName: s.displayName,
          description: s.description,
          executorKind: s.executorKind,
        })),
      });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.post<{ Body: { toolId?: string; effectClass?: string; displayName?: string; description?: string; enabled?: boolean; scope?: string } }>(
    "/admin/skills",
    async (req, reply) => {
      const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
      if (!principal) return;
      const b = req.body ?? {};
      try {
        const resp = await clients.north.upsertSkill(
          {
            tenantId: principal.tenant,
            userId: principal.sub,
            toolId: b.toolId ?? "",
            effectClass: b.effectClass ?? "",
            displayName: b.displayName ?? "",
            description: b.description ?? "",
            enabled: b.enabled !== undefined ? b.enabled : true,
            scope: b.scope ?? "",
          },
          principal.token,
        );
        const s = resp.skill;
        reply.send({
          skill: s
            ? {
                toolId: s.toolId,
                scope: s.scope,
                enabled: s.enabled,
                effectClass: s.effectClass,
                displayName: s.displayName,
                description: s.description,
                executorKind: s.executorKind,
              }
            : null,
        });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  app.delete<{ Params: { toolId: string } }>(
    "/admin/skills/:toolId",
    async (req, reply) => {
      const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
      if (!principal) return;
      try {
        await clients.north.deleteSkill(
          { tenantId: principal.tenant, userId: principal.sub, toolId: req.params.toolId },
          principal.token,
        );
        reply.send({ success: true });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  // ── Agent skill bundles (admin) ───────────────────────────────────────────

  // List all skill bundles for the tenant. Tenant-admin gated in the broker (403 on PERMISSION_DENIED).
  app.get("/admin/skill-bundles", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    try {
      const resp = await clients.north.listAgentSkills(
        { tenantId: principal.tenant, userId: principal.sub },
        principal.token,
      );
      reply.send({ bundles: resp.bundles ?? [] });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { bundles: [] } });
    }
  });

  // Delete a skill bundle by id. Tenant-admin gated in the broker (403 on PERMISSION_DENIED).
  app.delete<{ Params: { id: string } }>(
    "/admin/skill-bundles/:id",
    async (req, reply) => {
      const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
      if (!principal) return;
      try {
        await clients.north.deleteAgentSkill(
          { tenantId: principal.tenant, userId: principal.sub, id: req.params.id },
          principal.token,
        );
        reply.send({ success: true });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { success: false } });
      }
    },
  );

  // Upload a SKILL.md bundle (bare markdown or zip).
  // Wire-format discriminator (spec CP5):
  //   application/zip              → zip parser
  //   text/markdown | text/plain   → bare SKILL.md parser
  //   multipart/form-data          → extract file part named "skill" → bare parser
  //   anything else                → 415
  // On success calls broker UpsertAgentSkill; InvalidArgument → 400, PermissionDenied → 403.
  // req.body is a Buffer for the three registered content types (addContentTypeParser above).
  app.post<{ Body: Buffer; Querystring: { keywords?: string } }>(
    "/admin/skills/upload",
    { bodyLimit: SKILL_UPLOAD_MAX_BYTES },
    async (req, reply) => {
      await handleSkillUpload(req, reply, jwksResolver, verifyOpts, clients.north);
    },
  );

  // Edit-in-place: update an existing skill bundle by id.
  // Same wire-format rules as POST /admin/skills/upload; passes the path id to
  // UpsertAgentSkill so the broker updates the existing row rather than inserting.
  app.put<{ Params: { id: string }; Body: Buffer; Querystring: { keywords?: string } }>(
    "/admin/skill-bundles/:id",
    { bodyLimit: SKILL_UPLOAD_MAX_BYTES },
    async (req, reply) => {
      await handleSkillUpload(req, reply, jwksResolver, verifyOpts, clients.north, req.params.id);
    },
  );

  // Admin oversight: every schedule in the tenant, optionally filtered by owner.
  // Tenant-admin gated in the broker.
  app.get<{ Querystring: { owner?: string } }>(
    "/admin/scheduled-runs",
    async (req, reply) => {
      const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
      if (!principal) return;
      try {
        const resp = await clients.north.listScheduledRuns(
          { tenantId: principal.tenant, userId: principal.sub, ownerFilter: req.query.owner || "*" },
          principal.token,
        );
        reply.send({
          schedules: (resp.runs ?? []).map(scheduleJson),
          fgaEnabled: resp.fgaEnabled,
        });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { schedules: [] } });
      }
    },
  );

  // ── Network access-list (admin) ────────────────────────────────────────────
  app.get("/admin/network", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    try {
      const resp = await clients.north.listNetworkRules({ tenantId: principal.tenant, userId: principal.sub }, principal.token);
      reply.send({ rules: (resp.rules ?? []).map(netRuleJson) });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { rules: [] } });
    }
  });

  app.get<{ Querystring: { limit?: string } }>("/admin/alerts", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    try {
      const limit = req.query.limit ? Number(req.query.limit) : 0;
      const resp = await clients.north.listAlerts(
        { tenantId: principal.tenant, userId: principal.sub, limit: Number.isFinite(limit) ? limit : 0 },
        principal.token,
      );
      reply.send({
        alerts: (resp.alerts ?? []).map((a) => ({
          id: a.alertId,
          rule: a.rule,
          severity: a.severity,
          summary: a.summary ?? {},
          firedAt: a.firedAt ? a.firedAt.toISOString() : null,
        })),
      });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { alerts: [] } });
    }
  });

  app.post<{ Body: { scopeKind?: string; scopeValue?: string; action?: string; hostPattern?: string; note?: string } }>(
    "/admin/network",
    async (req, reply) => {
      const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
      if (!principal) return;
      const b = req.body ?? {};
      try {
        const resp = await clients.north.addNetworkRule(
          {
            tenantId: principal.tenant,
            userId: principal.sub,
            rule: {
              id: "",
              scopeKind: (b.scopeKind ?? "TENANT").toUpperCase(),
              scopeValue: b.scopeValue ?? "",
              action: (b.action ?? "ALLOW").toUpperCase(),
              hostPattern: b.hostPattern ?? "",
              note: b.note ?? "",
              createdBy: "",
              createdAt: undefined,
            },
          },
          principal.token,
        );
        reply.send({ rule: resp.rule ? netRuleJson(resp.rule) : null });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  app.post<{ Body: { id?: string } }>("/admin/network/delete", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    try {
      const resp = await clients.north.deleteNetworkRule(
        { tenantId: principal.tenant, userId: principal.sub, id: req.body?.id ?? "" },
        principal.token,
      );
      reply.send({ success: resp.success });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { success: false } });
    }
  });

  // ── Provisioning rules (admin) ─────────────────────────────────────────────
  app.get("/admin/provisioning", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    try {
      const resp = await clients.north.listProvisioningRules(
        { tenantId: principal.tenant, userId: principal.sub },
        principal.token,
      );
      reply.send({ rules: (resp.rules ?? []).map(provRuleJson) });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { rules: [] } });
    }
  });

  app.post<{ Body: { matcher?: string; groups?: string[] } }>(
    "/admin/provisioning",
    async (req, reply) => {
      const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
      if (!principal) return;
      const b = req.body ?? {};
      try {
        const resp = await clients.north.addProvisioningRule(
          {
            tenantId: principal.tenant,
            userId: principal.sub,
            matcher: b.matcher ?? "",
            groups: b.groups ?? [],
          },
          principal.token,
        );
        reply.send({
          rule: resp.rule ? provRuleJson(resp.rule) : null,
          appliedCount: resp.appliedCount,
        });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  app.post<{ Body: { id?: string } }>("/admin/provisioning/delete", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    try {
      await clients.north.deleteProvisioningRule(
        { tenantId: principal.tenant, userId: principal.sub, id: req.body?.id ?? "" },
        principal.token,
      );
      reply.send({});
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  // ── MCP connections (admin) ────────────────────────────────────────────────
  // Tenant-admins manage the registry of remote MCP servers. Bearer tokens go to
  // Vault; they are never echoed back by list or response mappers.
  app.get("/admin/mcp", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    try {
      const resp = await clients.north.listMcpConnections(
        { tenantId: principal.tenant, userId: principal.sub },
        principal.token,
      );
      reply.send({ connections: (resp.connections ?? []).map(mcpConnectionJson) });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { connections: [] } });
    }
  });

  app.post<{ Body: McpConnectionBody }>("/admin/mcp", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    const b = req.body ?? {};
    try {
      const resp = await clients.north.addMcpConnection(
        {
          tenantId: principal.tenant,
          userId: principal.sub,
          connection: {
            id: "",
            name: b.name ?? "",
            url: b.url ?? "",
            transport: b.transport ?? "streamable_http",
            authType: b.authType ?? "none",
            authSecretRef: "",
            createdBy: "",
            createdAt: undefined,
          },
          bearerToken: b.bearerToken ?? "",
        },
        principal.token,
      );
      reply.send({ connection: resp.connection ? mcpConnectionJson(resp.connection) : null });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.patch<{ Params: { id: string }; Body: McpConnectionBody }>("/admin/mcp/:id", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    const b = req.body ?? {};
    try {
      const resp = await clients.north.updateMcpConnection(
        {
          tenantId: principal.tenant,
          userId: principal.sub,
          connection: {
            id: req.params.id,
            name: b.name ?? "",
            url: b.url ?? "",
            transport: b.transport ?? "streamable_http",
            authType: b.authType ?? "none",
            authSecretRef: "",
            createdBy: "",
            createdAt: undefined,
          },
          bearerToken: b.bearerToken ?? "",
        },
        principal.token,
      );
      reply.send({ connection: resp.connection ? mcpConnectionJson(resp.connection) : null });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.delete<{ Params: { id: string } }>(
    "/admin/mcp/:id",
    async (req, reply) => {
      const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
      if (!principal) return;
      try {
        const resp = await clients.north.deleteMcpConnection(
          { tenantId: principal.tenant, userId: principal.sub, id: req.params.id },
          principal.token,
        );
        reply.send({ success: resp.success });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { success: false } });
      }
    },
  );

  app.get<{ Params: { id: string } }>(
    "/agents/:id/mcp-servers",
    async (req, reply) => {
      const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
      if (!principal) return;
      try {
        const resp = await clients.north.listAccessibleMcpServers(
          { tenantId: principal.tenant, agentId: req.params.id },
          principal.token,
        );
        reply.send({ connections: (resp.connections ?? []).map(mcpConnectionJson) });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { connections: [] } });
      }
    },
  );

  // ── Platform config (admin) ────────────────────────────────────────────────
  // Tenant-admin gated in the broker (403 on PERMISSION_DENIED).
  // InvalidArgument from the broker surfaces as 400 so validation errors reach
  // the UI (sendError/grpcToHttp maps INVALID_ARGUMENT to 400 specifically).
  app.get("/admin/config", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    try {
      const resp = await clients.north.getPlatformConfig({}, principal.token);
      reply.send({ entries: resp.entries ?? [] });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  // Read-only telemetry-export state. The OTLP endpoint is a broker deploy-time
  // env var (AIKONOS_OTEL_ENDPOINT) — surfaced for display only, never editable.
  app.get("/admin/observability", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    try {
      const resp = await clients.north.getObservabilityInfo({}, principal.token);
      reply.send({
        otelEndpoint: resp.otelEndpoint ?? "",
        exportEnabled: resp.exportEnabled ?? false,
        exportedJob: resp.exportedJob ?? "",
      });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.put<{ Body: { key?: string; value?: string } }>("/admin/config", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    const b = req.body ?? {};
    try {
      await clients.north.setPlatformConfig({ key: b.key ?? "", value: b.value ?? "" }, principal.token);
      reply.send({});
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  // ── LLM providers (admin) ──────────────────────────────────────────────────
  // Thin proxies to the broker north RPCs. The broker is the only authority:
  // tenant-admin gate, key→Vault, validation. The gateway forwards the bearer.
  app.get("/admin/providers", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    try {
      const resp = await clients.north.listLlmProviders({}, principal.token);
      // `defaults` (capability → provider id) passes through untouched — the
      // Defaults panel seeds its per-capability selects from it.
      reply.send({ providers: resp.providers ?? [], defaults: resp.defaults ?? {} });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.post<{ Body: { provider?: LlmProvider; apiKey?: string } }>("/admin/providers", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    const b = req.body ?? {};
    try {
      await clients.north.upsertLlmProvider({ provider: b.provider, apiKey: b.apiKey ?? "" }, principal.token);
      reply.send({});
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.delete<{ Params: { id: string } }>("/admin/providers/:id", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    try {
      await clients.north.deleteLlmProvider({ id: req.params.id }, principal.token);
      reply.send({});
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.post<{ Params: { id: string } }>("/admin/providers/:id/default", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    try {
      await clients.north.setDefaultProvider({ id: req.params.id }, principal.token);
      reply.send({});
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.post<{ Params: { id: string } }>("/admin/providers/:id/default-vision", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    try {
      await clients.north.setDefaultVisionProvider({ id: req.params.id }, principal.token);
      reply.send({});
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.post<{ Params: { id: string } }>("/admin/providers/:id/fallback", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    try {
      await clients.north.setFallbackProvider({ id: req.params.id }, principal.token);
      reply.send({});
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  // default-for supersedes the three single-capability routes above: one row per
  // capability in llm_provider_defaults. `clear: true` sends an empty provider id,
  // which the broker reads as "delete this capability's default".
  app.post<{ Params: { id: string }; Body: { capability?: string; clear?: boolean } }>(
    "/admin/providers/:id/default-for",
    async (req, reply) => {
      const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
      if (!principal) return;
      const b = req.body ?? {};
      try {
        await clients.north.setDefaultProviderFor(
          { capability: b.capability ?? "", providerId: b.clear === true ? "" : req.params.id },
          principal.token,
        );
        reply.send({});
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  app.post<{ Body: { provider?: LlmProvider; apiKey?: string; mode?: string } }>("/admin/providers/test", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    const b = req.body ?? {};
    try {
      const resp = await clients.north.testLlmProvider(
        { provider: b.provider, apiKey: b.apiKey ?? "", mode: b.mode ?? "" },
        principal.token,
      );
      reply.send({
        ok: resp.ok,
        statusCode: resp.statusCode,
        error: resp.error,
        latencyMs: Number(resp.latencyMs ?? 0),
      });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  // ── M365 tenant connection (admin) ─────────────────────────────────────────
  // Tenant-wide OneDrive OBO config: the
  // login app registration extended with delegated Graph Files.ReadWrite +
  // offline_access. Metadata in org_settings, secret in Vault — the gateway
  // never sees the stored secret, only has_secret; blank client_secret on
  // PUT/test preserves/reuses it (m365_admin.go resolveM365TestApp).
  app.get("/admin/m365", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    try {
      const resp = await clients.north.getM365Connection({}, principal.token);
      reply.send({ connection: m365Json(resp.connection) });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.put<{ Body: { entraTenantId?: string; clientId?: string; clientSecret?: string; enabled?: boolean } }>(
    "/admin/m365",
    async (req, reply) => {
      const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
      if (!principal) return;
      const b = req.body ?? {};
      try {
        const resp = await clients.north.upsertM365Connection(
          {
            connection: {
              entraTenantId: b.entraTenantId ?? "",
              clientId: b.clientId ?? "",
              hasSecret: false,
              enabled: b.enabled ?? false,
              updatedBy: "",
              updatedAt: "",
            },
            clientSecret: b.clientSecret ?? "",
          },
          principal.token,
        );
        reply.send({ connection: m365Json(resp.connection) });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  app.delete("/admin/m365", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    try {
      await clients.north.deleteM365Connection({}, principal.token);
      reply.send({});
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.post<{ Body: { entraTenantId?: string; clientId?: string; clientSecret?: string; enabled?: boolean } }>(
    "/admin/m365/test",
    async (req, reply) => {
      const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
      if (!principal) return;
      const b = req.body ?? {};
      try {
        const resp = await clients.north.testM365Connection(
          {
            connection: {
              entraTenantId: b.entraTenantId ?? "",
              clientId: b.clientId ?? "",
              hasSecret: false,
              enabled: b.enabled ?? false,
              updatedBy: "",
              updatedAt: "",
            },
            clientSecret: b.clientSecret ?? "",
          },
          principal.token,
        );
        reply.send({ ok: resp.ok, detail: resp.detail });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  // ── web.search engine config (admin) ───────────────────────────────────────
  // Org-wide search-engine config for the web.search tool.
  // api_key is write-only — the gateway never sees the stored key, only has_key;
  // blank api_key on PUT/test preserves/reuses the stored one (websearch_admin.go).
  app.get("/admin/websearch", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    try {
      const resp = await clients.north.getWebSearchConfig({}, principal.token);
      reply.send({ config: webSearchJson(resp.config) });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.put<{ Body: { engine?: string; maxResults?: number; apiKey?: string } }>(
    "/admin/websearch",
    async (req, reply) => {
      const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
      if (!principal) return;
      const b = req.body ?? {};
      try {
        const resp = await clients.north.upsertWebSearchConfig(
          {
            config: {
              engine: b.engine ?? "",
              maxResults: b.maxResults ?? 0,
              hasKey: false,
              updatedBy: "",
              updatedAt: "",
            },
            apiKey: b.apiKey ?? "",
          },
          principal.token,
        );
        reply.send({ config: webSearchJson(resp.config) });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  app.delete("/admin/websearch", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    try {
      await clients.north.deleteWebSearchConfig({}, principal.token);
      reply.send({});
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.post<{ Body: { engine?: string; maxResults?: number; apiKey?: string } }>(
    "/admin/websearch/test",
    async (req, reply) => {
      const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
      if (!principal) return;
      const b = req.body ?? {};
      try {
        const resp = await clients.north.testWebSearchConfig(
          {
            config: {
              engine: b.engine ?? "",
              maxResults: b.maxResults ?? 0,
              hasKey: false,
              updatedBy: "",
              updatedAt: "",
            },
            apiKey: b.apiKey ?? "",
          },
          principal.token,
        );
        reply.send({ ok: resp.ok, detail: resp.detail });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  // ── Decision trace (admin) ─────────────────────────────────────────────────
  // Tenant-admin gated in the broker (403 on PERMISSION_DENIED).
  app.get<{ Params: { taskId: string } }>(
    "/admin/decisions/:taskId",
    async (req, reply) => {
      const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
      if (!principal) return;
      try {
        const resp = await clients.north.getDecisionTrace({ taskId: req.params.taskId }, principal.token);
        reply.send({ taskId: resp.taskId ?? req.params.taskId, steps: resp.steps ?? [] });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  // ── Policy simulator (admin) ───────────────────────────────────────────────
  // Tenant-admin gated in the broker (403 on PERMISSION_DENIED). Lets admins
  // dry-run a hypothetical (subject, tool, effect-class) without any side-effects.
  app.post<{ Body: SimulateBody }>("/admin/policy/simulate", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    const b = req.body ?? {};

    // An absent/empty effectClass defaults to UNSPECIFIED (the broker treats it as
    // READ_ONLY in simulation). A non-empty unrecognised value is a caller error.
    const rawEffectClass = (b.effectClass ?? "").toUpperCase();
    if (rawEffectClass !== "" && !(rawEffectClass in EFFECT_CLASS_BY_STR)) {
      reply.code(400).send({ error: `unknown effect_class: ${b.effectClass}` });
      return;
    }
    const effectClassEnum =
      rawEffectClass !== "" ? EFFECT_CLASS_BY_STR[rawEffectClass] : EffectClass.EFFECT_CLASS_UNSPECIFIED;

    try {
      const resp = await clients.north.simulatePolicy(
        {
          subjectUserId:  b.subjectUserId  ?? "",
          toolId:         b.toolId         ?? "",
          effectClass:    effectClassEnum,
          host:           b.host           ?? "",
          readsSensitive: b.readsSensitive ?? false,
          fgaObject:      b.fgaObject      ?? "",
        },
        principal.token,
      );
      reply.send({ outcome: resp.outcome ?? "", gates: resp.gates ?? [] });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  // ── Audit read-side (admin) ────────────────────────────────────────────────
  // Both routes are tenant-admin gated in the broker (403 on PERMISSION_DENIED).
  // Timestamps are accepted as ISO strings and converted to Date for the proto
  // Timestamp fields (ts-proto encodes Date ↔ Timestamp automatically).
  app.get<{ Querystring: { start?: string; end?: string; actor?: string; event_type?: string; decision?: string; limit?: string; cursor?: string } }>(
    "/admin/audit/query",
    async (req, reply) => {
      const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
      if (!principal) return;
      const q = req.query;
      try {
        const resp = await clients.north.queryAudit(
          {
            startTime: q.start ? new Date(q.start) : undefined,
            endTime:   q.end   ? new Date(q.end)   : undefined,
            actorUserId: q.actor      ?? "",
            eventType:   q.event_type ?? "",
            decision:    DECISION_BY_STR[q.decision ?? ""] ?? PolicyDecision.POLICY_DECISION_UNSPECIFIED,
            limit:       q.limit ? parseInt(q.limit, 10) : 0,
            cursor:      q.cursor ?? "",
          },
          principal.token,
        );
        reply.send({
          events:          resp.events          ?? [],
          nextCursor:      resp.nextCursor       ?? "",
          storeConfigured: resp.storeConfigured  ?? false,
        });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  app.get(
    "/admin/audit/verify",
    async (req, reply) => {
      const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
      if (!principal) return;
      try {
        const resp = await clients.north.verifyAuditChain({}, principal.token);
        reply.send({
          total:             resp.total             ?? 0,
          ok:                resp.ok                ?? false,
          signed:            resp.signed             ?? false,
          storeConfigured:   resp.storeConfigured    ?? false,
          headEventId:       resp.headEventId        ?? "",
          tailEventId:       resp.tailEventId        ?? "",
          breaks:            resp.breaks             ?? [],
          signatureFailures: resp.signatureFailures  ?? [],
        });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  // ── Agents: admin CRUD ─────────────────────────────────────────────────────
  app.get("/admin/agents", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    try {
      const resp = await clients.north.listAgents({ tenantId: principal.tenant, userId: principal.sub }, principal.token);
      reply.send({ agents: (resp.agents ?? []).map(agentJson) });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { agents: [] } });
    }
  });

  app.post<{ Body: AgentBody }>("/admin/agents", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    const b = req.body ?? {};
    try {
      const resp = await clients.north.createAgent(
        {
          tenantId: principal.tenant,
          userId: principal.sub,
          agent: agentInputFromBody("", b),
        },
        principal.token,
      );
      reply.send({ agent: resp.agent ? agentJson(resp.agent) : null });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.patch<{ Params: { id: string }; Body: AgentBody }>("/admin/agents/:id", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    const b = req.body ?? {};
    try {
      const resp = await clients.north.updateAgent(
        {
          tenantId: principal.tenant,
          userId: principal.sub,
          agent: agentInputFromBody(req.params.id, b),
        },
        principal.token,
      );
      reply.send({ agent: resp.agent ? agentJson(resp.agent) : null });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.delete<{ Params: { id: string } }>(
    "/admin/agents/:id",
    async (req, reply) => {
      const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
      if (!principal) return;
      try {
        const resp = await clients.north.deleteAgent(
          { tenantId: principal.tenant, userId: principal.sub, id: req.params.id },
          principal.token,
        );
        reply.send({ success: resp.success });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { success: false } });
      }
    },
  );

  // ── Rate limit policies (admin) ────────────────────────────────────────────
  app.get("/admin/rate-limits", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    try {
      const resp = await clients.north.listRateLimitPolicies(
        { tenantId: principal.tenant },
        principal.token,
      );
      return reply.send({ policies: resp.policies ?? [] });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.post<{ Body: { agentId?: string; provider?: string; rpmLimit?: number; tpmLimit?: number } }>(
    "/admin/rate-limits",
    async (req, reply) => {
      const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
      if (!principal) return;
      const body = req.body ?? {};
      try {
        const resp = await clients.north.setRateLimitPolicy(
          {
            tenantId: principal.tenant,
            agentId: body.agentId ?? "",
            provider: body.provider ?? "",
            rpmLimit: body.rpmLimit ?? 0,
            tpmLimit: body.tpmLimit ?? 0,
          },
          principal.token,
        );
        reply.send({ id: resp.id });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  app.delete<{ Params: { id: string } }>(
    "/admin/rate-limits/:id",
    async (req, reply) => {
      const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
      if (!principal) return;
      try {
        await clients.north.deleteRateLimitPolicy(
          { tenantId: principal.tenant, id: req.params.id },
          principal.token,
        );
        reply.status(204).send();
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  // ── Spend caps ────────────────────────────
  app.get("/admin/spend-caps", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    try {
      const resp = await clients.north.listSpendCaps(
        { tenantId: principal.tenant },
        principal.token,
      );
      return reply.send({ caps: resp.caps ?? [] });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.get("/admin/spend-caps/summary", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    try {
      const resp = await clients.north.getSpendSummary(
        { tenantId: principal.tenant },
        principal.token,
      );
      return reply.send({
        orgSpendMicros: resp.orgSpendMicros ?? 0,
        orgCapMicros: resp.orgCapMicros ?? 0,
        users: resp.users ?? [],
        agents: resp.agents ?? [],
      });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.post<{ Body: { scope?: string; subjectId?: string; capMicros?: number } }>(
    "/admin/spend-caps",
    async (req, reply) => {
      const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
      if (!principal) return;
      const body = req.body ?? {};
      try {
        const resp = await clients.north.setSpendCap(
          {
            tenantId: principal.tenant,
            scope: body.scope ?? "",
            subjectId: body.subjectId ?? "",
            capMicros: body.capMicros ?? 0,
          },
          principal.token,
        );
        reply.send({ id: resp.id });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  app.delete<{ Params: { id: string } }>(
    "/admin/spend-caps/:id",
    async (req, reply) => {
      const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
      if (!principal) return;
      try {
        await clients.north.deleteSpendCap(
          { tenantId: principal.tenant, id: req.params.id },
          principal.token,
        );
        reply.status(204).send();
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  // ── Org governance settings (A-series, admin) ──────────────────────────────
  app.get("/admin/org-settings", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    try {
      const resp = await clients.north.getOrgSettings(
        { tenantId: principal.tenant },
        principal.token,
      );
      return reply.send({ settings: orgSettingsJson(resp.settings) });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.put<{ Body: { instructionPreamble?: string; unattendedAllowed?: boolean; workflowSharingAllowed?: boolean; connectorAllowlistEnabled?: boolean; connectorAllowlist?: string[]; disabledEffectClasses?: string[] } }>(
    "/admin/org-settings",
    async (req, reply) => {
      const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
      if (!principal) return;
      const body = req.body ?? {};
      // Only forward fields the client actually sent, so a page that owns one
      // setting never clobbers another page's field (the broker merges partial).
      // updatedBy/updatedAt are server-owned provenance — the broker ignores
      // them on update; they are set here only to satisfy the message type.
      const settings: OrgSettings = { updatedBy: "", updatedAt: "", connectorAllowlist: [] };
      if (typeof body.instructionPreamble === "string") {
        settings.instructionPreamble = body.instructionPreamble;
      }
      if (typeof body.unattendedAllowed === "boolean") {
        settings.unattendedAllowed = body.unattendedAllowed;
      }
      if (typeof body.workflowSharingAllowed === "boolean") {
        settings.workflowSharingAllowed = body.workflowSharingAllowed;
      }
      // A7: enabled flag + list travel together (the broker applies both when
      // the flag is present). Only forward when the client sent the flag.
      if (typeof body.connectorAllowlistEnabled === "boolean") {
        settings.connectorAllowlistEnabled = body.connectorAllowlistEnabled;
        settings.connectorAllowlist = Array.isArray(body.connectorAllowlist)
          ? body.connectorAllowlist.filter((p): p is string => typeof p === "string")
          : [];
      }
      // A2: presence of the array signals the capabilities page owns this update.
      if (Array.isArray(body.disabledEffectClasses)) {
        settings.capabilities = {
          disabledEffectClasses: body.disabledEffectClasses.filter((c): c is string => typeof c === "string"),
        };
      }
      try {
        const resp = await clients.north.updateOrgSettings(
          { tenantId: principal.tenant, settings },
          principal.token,
        );
        return reply.send({ settings: orgSettingsJson(resp.settings) });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  // ── Members roster (A3, admin) ─────────────────────────────────────────────
  app.get("/admin/members", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    try {
      const resp = await clients.north.listMembers({ tenantId: principal.tenant }, principal.token);
      return reply.send({ members: resp.members ?? [] });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  app.post<{ Params: { id: string }; Body: { label?: string } }>(
    "/admin/agents/:id/keys",
    async (req, reply) => {
      const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
      if (!principal) return;
      try {
        const resp = await clients.north.mintAgentApiKey(
          {
            tenantId: principal.tenant,
            userId: principal.sub,
            agentId: req.params.id,
            label: req.body?.label ?? "",
          },
          principal.token,
        );
        reply.send({
          rawKey: resp.rawKey,
          key: resp.key
            ? { id: resp.key.id, keyPrefix: resp.key.keyPrefix, label: resp.key.label }
            : null,
        });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  app.get<{ Params: { id: string } }>(
    "/admin/agents/:id/keys",
    async (req, reply) => {
      const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
      if (!principal) return;
      try {
        const resp = await clients.north.listAgentApiKeys(
          { tenantId: principal.tenant, userId: principal.sub, agentId: req.params.id },
          principal.token,
        );
        reply.send({
          keys: (resp.keys ?? []).map((k) => ({
            id: k.id,
            keyPrefix: k.keyPrefix,
            label: k.label,
          })),
        });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  app.delete<{ Params: { id: string; keyId: string } }>(
    "/admin/agents/:id/keys/:keyId",
    async (req, reply) => {
      const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
      if (!principal) return;
      try {
        const resp = await clients.north.revokeAgentApiKey(
          {
            tenantId: principal.tenant,
            userId: principal.sub,
            agentId: req.params.id,
            keyId: req.params.keyId,
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
