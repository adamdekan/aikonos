// Personal skills — management + transfer routes. The broker owns every gate
// (ownership, FGA capability, recipient checks) — these are pure forwards of
// the caller's own bearer, except POST /skills/import, which does gateway-
// local parsing (reusing pi/skill-parser.ts, same as the admin skill-bundle
// upload route) before ever calling a broker RPC.
// Registered by registerSkillsRoutes(app, ctx) from src/app.ts.
import { posix as posixPath } from "node:path";
import type { FastifyInstance } from "fastify";
import { requireUser } from "../auth/require-user.js";
import type { JwksResolver, VerifyOptions } from "../auth/verify.js";
import type { NorthClient } from "../broker/north.js";
import type { SouthClient } from "../broker/south.js";
import { extractMultipartFile } from "./admin.js";
import { FILE_UPLOAD_BODY_LIMIT } from "./files.js";
import { parseSkillMd, MAX_SKILL_FILES } from "../pi/skill-parser.js";
import { sendError } from "../http-errors.js";
import { log } from "../log.js";

export interface SkillsCtx {
  clients: {
    north: Pick<
      NorthClient,
      | "listPersonalSkills"
      | "deletePersonalSkill"
      | "sendSkillTransfer"
      | "getSkillTransferPreview"
      | "acceptSkillTransfer"
      | "uploadWorkspaceFile"
    >;
    south: Pick<SouthClient, "listUserAgentSkills">;
  };
  jwksResolver: JwksResolver;
  verifyOpts: VerifyOptions;
}

// ── name sanitization ───────────────────────────────────────────────────────

// sanitizeSkillDirName turns an arbitrary frontmatter `name` into a single,
// filesystem-safe Skills/ subdirectory segment: separators and whitespace
// collapse to a hyphen, any other disallowed character is dropped, and a
// dots-only result (".", "..", "...") — which would otherwise escape Skills/
// when joined into a path — is caught by the same leading-dot strip (a
// string made entirely of dots has nothing left after it) and falls back to
// the literal default "skill".
function sanitizeSkillDirName(raw: string): string {
  const withHyphens = raw.trim().replace(/[\\/\s]+/g, "-");
  const cleaned = withHyphens.replace(/[^a-zA-Z0-9._-]/g, "");
  const noLeadingDots = cleaned.replace(/^\.+/, "");
  return noLeadingDots || "skill";
}

// isSafeExtrasPath is the gateway-local trust-boundary check for zip-slip via
// extras keys: an extras dict key
// (references/*, assets/*) is the raw zip entry name — parseSkillMd never
// validates it against `..`. A crafted key like
// "references/../../../.agent/Sessions/evil.json" would otherwise clean to
// ".agent/Sessions/evil.json" and land outside Skills/<name>/. Reject any key
// that is itself absolute (parseSkillMd's own "references/"/"assets/" prefix
// filter already makes this unreachable today, but a future filter change
// must not silently reopen it — see the unit test below), or that, once
// joined onto Skills/<name>/ and normalized, escapes that directory.
export function isSafeExtrasPath(name: string, extraPath: string): boolean {
  if (posixPath.isAbsolute(extraPath)) return false;
  const base = `Skills/${name}/`;
  const joined = posixPath.normalize(posixPath.join(base, extraPath));
  return joined.startsWith(base);
}

// firstFreeName returns base, or the first base-2, base-3, … absent from
// existing — mirrors broker/internal/personalskill.FreeName's probe order,
// used here only to compute the 409 response's suggested_name hint (the
// gateway never installs under it itself — that is AcceptSkillTransfer's job
// on the transfer path; import always uses the sanitized name verbatim and
// 409s on conflict rather than auto-renaming).
function firstFreeName(existing: Set<string>, base: string): string {
  if (!existing.has(base)) return base;
  for (let n = 2; ; n++) {
    const candidate = `${base}-${n}`;
    if (!existing.has(candidate)) return candidate;
  }
}

// MAX_IMPORT_TOTAL_BYTES mirrors personalskill.MaxFolderBytes (broker) — the
// same whole-folder cap the broker's Snapshot enforces, checked here too so an
// oversize import 413s before any RPC rather than failing mid-write.
const MAX_IMPORT_TOTAL_BYTES = 20 * 1024 * 1024;

// ── POST /skills/import ──────────────────────────────────────────────────────

// Minimal req/reply shapes handleSkillImport needs — mirrors admin.ts's
// UploadReq/UploadReply so tests can drive the same code path without a live
// Fastify server (no diverged handler copy).
export interface ImportReq {
  headers: Record<string, string | string[] | undefined>;
  body?: Buffer;
}

export interface ImportReply {
  code(n: number): ImportReply;
  send(b: unknown): void;
}

/**
 * Core logic for POST /skills/import, extracted for testability (mirrors
 * admin.ts's handleSkillUpload). The route registers this with
 * bodyLimit: FILE_UPLOAD_BODY_LIMIT (mirrors POST /files, not the smaller
 * admin skill-bundle cap — an imported bundle's references/assets can be
 * larger than a single admin-authored SKILL.md).
 */
export async function handleSkillImport(
  req: ImportReq,
  reply: ImportReply,
  jwksResolver: JwksResolver,
  verifyOpts: VerifyOptions,
  north: Pick<NorthClient, "listPersonalSkills" | "uploadWorkspaceFile" | "deletePersonalSkill">,
): Promise<void> {
  const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
  if (!principal) return;

  const body = req.body ?? Buffer.alloc(0);

  // bodyLimit enforces this at the route level; the explicit check here
  // guards callers of the exported function (e.g. tests) that bypass Fastify.
  if (body.length > FILE_UPLOAD_BODY_LIMIT) {
    reply.code(413).send({ error: `payload too large: import must not exceed ${FILE_UPLOAD_BODY_LIMIT} bytes` });
    return;
  }

  // Content-type dispatch mirrors admin.ts's handleSkillUpload exactly (zip /
  // bare markdown / multipart fallback) — req.body is already a Buffer for
  // these content types because registerAdminRoutes registers the matching
  // addContentTypeParser entries once, globally, on the same Fastify app
  // instance this route shares (see app.ts). Registering them again here
  // would throw FST_ERR_CTP_ALREADY_PRESENT at startup.
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

  const name = sanitizeSkillDirName(parsed.name);

  // Zip-slip gate on extras keys — reject the whole import loud (400) before
  // any RPC, rather than skipping the bad entry silently.
  const unsafeExtraPath = Object.keys(parsed.extras).find((extraPath) => !isSafeExtrasPath(name, extraPath));
  if (unsafeExtraPath !== undefined) {
    reply.code(400).send({ error: `unsafe path in bundle extras: "${unsafeExtraPath}"` });
    return;
  }

  // Route-level caps, checked before any RPC. The file-count recheck is currently unreachable in
  // practice: parseSkillMd's own MAX_SKILL_FILES cap already throws (400)
  // before returning an over-count `parsed.extras` — kept here as a
  // ponytail: defense-in-depth backstop against a future divergence between
  // the parser's cap and this one, not exercised by today's call path.
  const extrasCount = Object.keys(parsed.extras).length;
  if (extrasCount > MAX_SKILL_FILES) {
    reply.code(413).send({ error: `too many files in bundle: max ${MAX_SKILL_FILES}` });
    return;
  }
  const totalBytes =
    Buffer.byteLength(parsed.rawSkillMd, "utf8") +
    Object.values(parsed.extras).reduce((sum, buf) => sum + buf.length, 0);
  if (totalBytes > MAX_IMPORT_TOTAL_BYTES) {
    reply.code(413).send({ error: `import too large: total decoded bytes exceed ${MAX_IMPORT_TOTAL_BYTES}` });
    return;
  }

  // "Existing dir" conflict check —
  // uploadWorkspaceFile has no exists-check of its own (a plain write, see
  // workspacefs.Store.Write/writeAtClean's unconditional os.Rename), so the
  // gateway is the sole "never overwrite" enforcement point. Best-effort: a
  // race between this check and the write below (two concurrent imports of
  // the same name) is an accepted residual — same class as the spec's other
  // staging-GC residuals, and the only harm is one lost import, not data loss
  // or an authority breach.
  let existingNames: Set<string>;
  try {
    const resp = await north.listPersonalSkills({ tenantId: principal.tenant, userId: principal.sub }, principal.token);
    existingNames = new Set((resp.skills ?? []).map((s) => s.name));
  } catch (err) {
    sendError(reply, log, err, { route: "POST /skills/import" });
    return;
  }

  if (existingNames.has(name)) {
    reply.code(409).send({
      error: `a skill named "${name}" already exists`,
      suggested_name: firstFreeName(existingNames, name),
    });
    return;
  }

  try {
    // rawSkillMd (not parsed.body) is written verbatim — it carries the
    // original frontmatter untouched, including fields this parser doesn't
    // model (e.g. keywords). Reconstructing frontmatter from the extracted
    // fields would silently drop those.
    await north.uploadWorkspaceFile(
      {
        tenantId: principal.tenant,
        userId: principal.sub,
        path: `Skills/${name}/SKILL.md`,
        content: Buffer.from(parsed.rawSkillMd, "utf8"),
      },
      principal.token,
    );
    for (const [extraPath, content] of Object.entries(parsed.extras)) {
      // content is already a Buffer (parseSkillMd's extras are binary-safe) —
      // no text coercion, so a binary asset (e.g. assets/img/logo.png) round-trips intact.
      await north.uploadWorkspaceFile(
        {
          tenantId: principal.tenant,
          userId: principal.sub,
          path: `Skills/${name}/${extraPath}`,
          content,
        },
        principal.token,
      );
    }
    reply.code(201).send({ name });
  } catch (err) {
    // Best-effort cleanup: a write failure mid-loop (e.g. SKILL.md landed,
    // an extra didn't) leaves a partial Skills/<name>/ orphan. Delete it via
    // the same path DELETE /skills/:name uses; swallow a cleanup failure —
    // the caller still gets the original error below.
    try {
      await north.deletePersonalSkill({ tenantId: principal.tenant, userId: principal.sub, name }, principal.token);
    } catch {
      // best-effort only
    }
    sendError(reply, log, err, { route: "POST /skills/import" });
  }
}

// ── route registration ────────────────────────────────────────────────────────

export function registerSkillsRoutes(app: FastifyInstance, ctx: SkillsCtx): void {
  const { clients, jwksResolver, verifyOpts } = ctx;

  app.get("/skills", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;

    let skills;
    try {
      const resp = await clients.north.listPersonalSkills(
        { tenantId: principal.tenant, userId: principal.sub },
        principal.token,
      );
      skills = resp.skills ?? [];
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      return;
    }

    // Granted admin bundles (read-only section) come from the same south RPC
    // the chat session build uses — fail-open, never denies the owner's own
    // skills above on a south hiccup.
    let granted: Awaited<ReturnType<typeof clients.south.listUserAgentSkills>>["bundles"] = [];
    let grantedUnavailable = false;
    try {
      const resp = await clients.south.listUserAgentSkills({ tenantId: principal.tenant, userId: principal.sub });
      granted = resp.bundles ?? [];
    } catch (err) {
      log.warn({ err: String(err), userId: principal.sub }, "listUserAgentSkills RPC failed — granted skills section unavailable");
      grantedUnavailable = true;
    }

    reply.send({ skills, granted, grantedUnavailable });
  });

  app.post<{ Body: Buffer }>(
    "/skills/import",
    { bodyLimit: FILE_UPLOAD_BODY_LIMIT },
    async (req, reply) => handleSkillImport(req, reply, jwksResolver, verifyOpts, clients.north),
  );

  app.delete<{ Params: { name: string } }>("/skills/:name", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    try {
      await clients.north.deletePersonalSkill(
        { tenantId: principal.tenant, userId: principal.sub, name: req.params.name },
        principal.token,
      );
      reply.send({ success: true });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}`, body: { success: false } });
    }
  });

  // Body keys are user_id / group_id — the recipient oneof, snake_case per the canonical
  // contract, not this file's ambient camelCase convention.
  app.post<{ Params: { name: string }; Body: { user_id?: string; group_id?: string } }>(
    "/skills/:name/share",
    async (req, reply) => {
      const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
      if (!principal) return;
      const b = req.body ?? {};
      const recipient = b.group_id ? { groupId: b.group_id } : { userId: b.user_id ?? "" };
      try {
        const resp = await clients.north.sendSkillTransfer(
          { tenantId: principal.tenant, userId: principal.sub, name: req.params.name, recipient },
          principal.token,
        );
        reply.send({ envelopeIds: resp.envelopeIds ?? [], skippedUserIds: resp.skippedUserIds ?? [] });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );

  app.get<{ Params: { envelopeId: string } }>("/skills/transfers/:envelopeId", async (req, reply) => {
    const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
    if (!principal) return;
    try {
      const resp = await clients.north.getSkillTransferPreview(
        { tenantId: principal.tenant, userId: principal.sub, envelopeId: req.params.envelopeId },
        principal.token,
      );
      reply.send({
        skillName: resp.skillName,
        fromUserId: resp.fromUserId,
        body: resp.body,
        manifest: resp.manifest ?? [],
        flags: resp.flags ?? [],
        contentHash: resp.contentHash,
        conflict: resp.conflict,
      });
    } catch (err) {
      sendError(reply, log, err, { route: `${req.method} ${req.url}` });
    }
  });

  // Body keys are mode / name_override — same snake_case-per-contract note as share above.
  app.post<{ Params: { envelopeId: string }; Body: { mode?: string; name_override?: string } }>(
    "/skills/transfers/:envelopeId/accept",
    async (req, reply) => {
      const principal = await requireUser(req, reply, jwksResolver, verifyOpts);
      if (!principal) return;
      const b = req.body ?? {};
      try {
        const resp = await clients.north.acceptSkillTransfer(
          {
            tenantId: principal.tenant,
            userId: principal.sub,
            envelopeId: req.params.envelopeId,
            mode: b.mode ?? "rename",
            nameOverride: b.name_override ?? "",
          },
          principal.token,
        );
        reply.send({ installedName: resp.installedName });
      } catch (err) {
        sendError(reply, log, err, { route: `${req.method} ${req.url}` });
      }
    },
  );
}
