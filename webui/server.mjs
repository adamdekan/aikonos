// aikonOS unified webui backend.
// Serves the built Vue SPA from web/dist/ (SPA fallback to index.html).
// Proxies API + AG-UI + audit-stream paths to the agent-gateway / observability
// service, forwarding the Authorization header from the browser.
//
//   GET  /healthz
//   GET  /runtime-config.js → deployment settings for the SPA (from env)
//   GET  /desktop.json      → sign-in and release settings for the Windows app
//   ALL  /api/*             → gateway (strip /api prefix)  — JSON, buffered OK
//   POST /agui              → gateway /agui                — SSE, must NOT buffer
//   GET  /audit/stream      → observability /api/audit/stream — SSE, must NOT buffer
//   GET  /*                 → web/dist SPA (index.html fallback)
import { existsSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import Fastify from "fastify";

const here = dirname(fileURLToPath(import.meta.url));

// Skill-bundle uploads (POST /api/admin/skills/upload, PUT /api/admin/skill-bundles/:id)
// carry non-JSON bodies: SKILL.md text or a .skill/.zip archive. Without a parser
// for these media types Fastify rejects the request with 415 BEFORE the /api/*
// proxy handler runs. Register raw-buffer parsers so req.body is the raw bytes and
// the proxy can forward them to the gateway verbatim (original Content-Type + body).
export const UPLOAD_MEDIA_TYPES = ["text/markdown", "text/plain", "application/zip"];

// OIDC settings the SPA reads at startup from /runtime-config.js (see
// web/src/auth/oidc.js). They come from the container environment, so one
// published image serves any identity provider without a rebuild. An unset or
// empty variable is left out and the SPA falls back to the value baked in at
// build time (VITE_OIDC_*). Only these keys are ever exposed: they are public
// client settings, the same values a built bundle already carries.
export const RUNTIME_OIDC_ENV = {
  authority: "AIKONOS_WEBUI_OIDC_AUTHORITY",
  clientId: "AIKONOS_WEBUI_OIDC_CLIENT",
  redirectUri: "AIKONOS_WEBUI_OIDC_REDIRECT_URI",
  scope: "AIKONOS_WEBUI_OIDC_SCOPE",
  token: "AIKONOS_WEBUI_OIDC_TOKEN",
};

export function runtimeConfig(env = process.env) {
  const oidc = {};
  for (const [key, name] of Object.entries(RUNTIME_OIDC_ENV)) {
    const value = (env[name] ?? "").trim();
    if (value) oidc[key] = value;
  }
  return { oidc };
}

// Settings the Windows app (desktop/) reads when someone enters this server's
// address. It signs in as a native OIDC client with a loopback redirect
// (RFC 8252), so it has a client id of its own and never borrows the web
// console's; the identity provider, scope and token kind are the web
// console's, because both get the same tokens for the same gateway. The
// release fields name the desktop build this server expects: a newer one is
// offered to the user, and below the minimum the app refuses to continue.
// Like /runtime-config.js, only these public settings are ever exposed.
export const DESKTOP_ENV = {
  clientId: "AIKONOS_DESKTOP_OIDC_CLIENT",
  version: "AIKONOS_DESKTOP_VERSION",
  minimumVersion: "AIKONOS_DESKTOP_MINIMUM_VERSION",
  url: "AIKONOS_DESKTOP_URL",
  notes: "AIKONOS_DESKTOP_NOTES",
};

export const DEFAULT_DESKTOP_CLIENT = "aikonos-desktop";

export function desktopConfig(env = process.env) {
  const read = (name) => (env[name] ?? "").trim();
  const oidc = {};
  for (const key of ["authority", "scope", "token"]) {
    const value = read(RUNTIME_OIDC_ENV[key]);
    if (value) oidc[key] = value;
  }
  oidc.clientId = read(DESKTOP_ENV.clientId) || DEFAULT_DESKTOP_CLIENT;

  // A minimum alone is enough to turn old builds away.
  const minimumVersion = read(DESKTOP_ENV.minimumVersion);
  const version = read(DESKTOP_ENV.version) || minimumVersion;
  if (!version) return { oidc };
  const release = { version };
  if (minimumVersion) release.minimumVersion = minimumVersion;
  for (const key of ["url", "notes"]) {
    const value = read(DESKTOP_ENV[key]);
    if (value) release[key] = value;
  }
  return { oidc, release };
}

// buildApp creates and configures the Fastify instance without starting it.
// opts.distDir defaults to the real web/dist; pass a non-existent path in tests
// to skip static serving. opts.env defaults to process.env (runtime config).
export async function buildApp({
  gatewayUrl,
  observabilityUrl,
  distDir = join(here, "web", "dist"),
  env = process.env,
} = {}) {
  const GATEWAY_URL = (gatewayUrl ?? process.env.GATEWAY_URL ?? "http://agent-gateway.aikonos-platform.svc.cluster.local:8080").replace(/\/$/, "");
  const OBSERVABILITY_URL = (observabilityUrl ?? process.env.OBSERVABILITY_URL ?? "http://observability.aikonos-platform.svc.cluster.local:4000").replace(/\/$/, "");

  // bodyLimit headroom above the gateway's 14 MiB /files upload cap so the gateway
  // stays the authoritative limiter and returns the descriptive 413; without this
  // Fastify's 1 MiB default would reject large file/bundle uploads here first.
  const app = Fastify({ logger: false, bodyLimit: 15 * 1024 * 1024 });

  for (const ct of UPLOAD_MEDIA_TYPES) {
    app.addContentTypeParser(ct, { parseAs: "buffer" }, (_req, body, done) => done(null, body));
  }

  // Static SPA — register only when dist/ is present (allows running server.mjs in
  // dev before the first build, or in unit tests without a build artifact).
  const hasDist = existsSync(distDir);
  if (hasDist) {
    const fstatic = await import("@fastify/static");
    // No wildcard:false — let @fastify/static serve real files; unknown paths
    // fall through to the SPA catch-all registered after the API routes.
    app.register(fstatic.default, { root: distDir });
  }

  app.get("/healthz", async () => ({ ok: true, gateway: GATEWAY_URL, observability: OBSERVABILITY_URL || null }));

  // Loaded by a classic <script> in index.html, which runs before the deferred
  // app bundle. JSON.stringify output is a valid JS literal, so no value can
  // break out of the assignment. no-store: a changed setting applies on the next
  // page load, not after a cache expires.
  const runtimeConfigJs = `window.__AIKONOS_CONFIG__ = ${JSON.stringify(runtimeConfig(env))};\n`;
  app.get("/runtime-config.js", async (_req, reply) => {
    reply
      .header("content-type", "application/javascript; charset=utf-8")
      .header("cache-control", "no-store")
      .send(runtimeConfigJs);
  });

  const desktopJson = JSON.stringify(desktopConfig(env));
  app.get("/desktop.json", async (_req, reply) => {
    reply
      .header("content-type", "application/json; charset=utf-8")
      .header("cache-control", "no-store")
      .send(desktopJson);
  });

  // SSE passthrough helper — must write directly to the raw socket so Fastify
  // cannot buffer the response. Caller is responsible for calling this from a
  // route handler whose reply is never finalized by Fastify.
  function pipeSSE(upstream, raw) {
    raw.setHeader("content-type", "text/event-stream");
    raw.setHeader("cache-control", "no-cache");
    raw.setHeader("x-accel-buffering", "no");
    raw.flushHeaders();
    // Use the WHATWG ReadableStream from fetch() to avoid pulling in extra deps.
    const reader = upstream.body.getReader();
    function pump() {
      reader.read().then(({ done, value }) => {
        if (done) { raw.end(); return; }
        raw.write(value);
        pump();
      }).catch((err) => {
        console.error("SSE pump error:", err);
        // Cancel via the reader, not upstream.body: getReader() locked the stream,
        // so body.cancel() throws ERR_INVALID_STATE and crashes the process.
        reader.cancel().catch(() => {});
        try { raw.end(); } catch { /* socket already closed */ }
      });
    }
    pump();
  }

  // /agui — forward POST to gateway, stream SSE response back.
  // The Authorization header from the browser is forwarded verbatim.
  app.post("/agui", async (req, reply) => {
    const headers = { "content-type": "application/json" };
    if (req.headers["authorization"]) headers["authorization"] = req.headers["authorization"];
    // Copy accept header if present (AG-UI protocol).
    if (req.headers["accept"]) headers["accept"] = req.headers["accept"];
    try {
      const upstream = await fetch(`${GATEWAY_URL}/agui`, {
        method: "POST",
        headers,
        body: JSON.stringify(req.body),
      });
      reply.hijack();
      pipeSSE(upstream, reply.raw);
    } catch (err) {
      reply.code(502).send({ error: `gateway unreachable: ${String(err)}` });
    }
  });

  // /audit/stream — forward GET to observability service, stream SSE back.
  // Returns 501 when OBSERVABILITY_URL is not configured so the Audit view can
  // detect the not-configured state (it calls onerror → notConfigured = true).
  app.get("/audit/stream", async (req, reply) => {
    if (!OBSERVABILITY_URL) {
      reply.code(501).send({ error: "OBSERVABILITY_URL not configured" });
      return;
    }
    const headers = { accept: "text/event-stream" };
    if (req.headers["authorization"]) headers["authorization"] = req.headers["authorization"];
    // Forward the request query string verbatim (F44: ?tenant= scoping).
    const qs = req.url.includes("?") ? req.url.slice(req.url.indexOf("?")) : "";
    try {
      const upstream = await fetch(`${OBSERVABILITY_URL}/api/audit/stream${qs}`, {
        method: "GET",
        headers,
      });
      reply.hijack();
      pipeSSE(upstream, reply.raw);
    } catch (err) {
      reply.code(502).send({ error: `observability unreachable: ${String(err)}` });
    }
  });

  // /api/workflows/:lineageId/run — POST. With ?stream=1 the gateway responds
  // with SSE (live per-step progress), which the buffering /api/* handler below
  // would swallow — pipe it unbuffered like /agui. Without ?stream=1 it is an
  // ordinary buffered JSON proxy (the blocking run path), byte-identical to /api/*.
  app.post("/api/workflows/:lineageId/run", async (req, reply) => {
    const streaming = req.query?.stream != null && req.query.stream !== "";
    const path = req.url.replace(/^\/api/, "");
    const headers = { "content-type": "application/json" };
    if (req.headers["authorization"]) headers["authorization"] = req.headers["authorization"];
    if (streaming) headers["accept"] = "text/event-stream";
    try {
      const upstream = await fetch(GATEWAY_URL + path, {
        method: "POST",
        headers,
        body: JSON.stringify(req.body ?? {}),
      });
      if (streaming) {
        reply.hijack();
        pipeSSE(upstream, reply.raw);
        return;
      }
      const text = await upstream.text();
      const contentType = upstream.headers.get("content-type") ?? "application/json";
      reply.code(upstream.status).header("content-type", contentType).send(text);
    } catch (err) {
      reply.code(502).send({ error: `gateway unreachable: ${String(err)}` });
    }
  });

  // /api/* — forward to gateway, strip /api prefix, forward Authorization.
  app.all("/api/*", async (req, reply) => {
    const path = req.url.replace(/^\/api/, "");
    const target = GATEWAY_URL + path;
    const headers = {};
    if (req.headers["authorization"]) headers["authorization"] = req.headers["authorization"];
    const init = { method: req.method, headers };
    // Only tag the upstream request as JSON when a body is actually forwarded.
    // A body-less request (e.g. DELETE) sent with content-type: application/json
    // and an empty body makes the gateway's Fastify reject it with 400
    // (FST_ERR_CTP_EMPTY_JSON_BODY) before the route handler runs.
    if (req.method !== "GET" && req.method !== "HEAD" && req.body !== undefined) {
      const reqCt = (req.headers["content-type"] ?? "").split(";")[0].trim();
      if (UPLOAD_MEDIA_TYPES.includes(reqCt)) {
        // Non-JSON upload body (req.body is a raw Buffer) — forward verbatim so the
        // gateway's content-type discriminator sees the real media type, not JSON.
        headers["content-type"] = req.headers["content-type"];
        init.body = req.body;
      } else {
        headers["content-type"] = "application/json";
        init.body = JSON.stringify(req.body);
      }
    }
    try {
      const res = await fetch(target, init);
      const text = await res.text();
      // Forward the upstream content-type verbatim (e.g. text/csv exports) — only
      // hardcode application/json when upstream didn't send one.
      const contentType = res.headers.get("content-type") ?? "application/json";
      reply.code(res.status).header("content-type", contentType).send(text);
    } catch (err) {
      reply.code(502).send({ error: `gateway unreachable: ${String(err)}` });
    }
  });

  // SPA deep-link fallback — must be registered AFTER proxy routes so that
  // /api/*, /agui, /audit/stream are matched first. Only active when dist/ exists.
  if (hasDist) {
    const indexHtml = readFileSync(join(distDir, "index.html"), "utf8");
    app.setNotFoundHandler((_req, reply) => {
      reply.code(200).header("content-type", "text/html; charset=utf-8").send(indexHtml);
    });
  }

  return app;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const PORT = Number(process.env.PORT ?? 4200);
  const GATEWAY_URL = (process.env.GATEWAY_URL ?? "http://agent-gateway.aikonos-platform.svc.cluster.local:8080").replace(/\/$/, "");
  const app = await buildApp();
  await app.listen({ port: PORT, host: "0.0.0.0" });
  console.log(`webui backend on :${PORT} (gateway ${GATEWAY_URL})`);
}
