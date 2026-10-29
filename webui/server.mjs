// aikonOS unified webui backend.
// Serves the built Vue SPA from web/dist/ (SPA fallback to index.html).
// Proxies API + AG-UI + audit-stream paths to the agent-gateway / observability
// service, forwarding the Authorization header from the browser.
//
//   GET  /healthz           → liveness only, no upstream details
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

// Where the SPA signs in: the runtime authority, else the SPA's own default
// (auth/oidc.js). A deployment that bakes VITE_OIDC_AUTHORITY into the bundle
// instead must also set AIKONOS_WEBUI_OIDC_AUTHORITY, or the CSP below blocks
// its identity provider.
const DEFAULT_OIDC_ORIGIN = "http://localhost:18080";

function oidcOrigin(env) {
  const authority = runtimeConfig(env).oidc.authority;
  if (!authority) return DEFAULT_OIDC_ORIGIN;
  try {
    return new URL(authority).origin;
  } catch {
    return null; // not a URL: the SPA cannot sign in with it either
  }
}

// M-26: browser-side backstop for a markup or dependency gap in the SPA. No
// inline or third-party script, no remote images (a rendered markdown image
// would leak data with zero clicks), no network or frames except this origin
// and the identity provider (discovery, token, silent renew). Inline styles
// stay: index.html's @font-face block and the markdown renderer need them.
export function securityHeaders(env = process.env) {
  const idp = oidcOrigin(env);
  const withIdp = (src) => (idp ? `${src} ${idp}` : src);
  const csp = (frameAncestors) => [
    "default-src 'self'",
    "script-src 'self'",
    "style-src 'self' 'unsafe-inline'",
    "img-src 'self' data:",
    withIdp("connect-src 'self'"),
    withIdp("frame-src 'self'"),
    `frame-ancestors ${frameAncestors}`,
    "form-action 'self'",
    "base-uri 'none'",
    "object-src 'none'",
  ].join("; ");
  const common = { "x-content-type-options": "nosniff", "referrer-policy": "no-referrer" };
  return {
    page: { ...common, "content-security-policy": csp("'none'"), "x-frame-options": "DENY" },
    // oidc-client-ts silent renew (no refresh token) lands the IdP's answer
    // on the redirect URI inside a hidden iframe of this same origin.
    callback: { ...common, "content-security-policy": csp("'self'"), "x-frame-options": "SAMEORIGIN" },
    callbackPath: callbackPath(env),
  };
}

function callbackPath(env) {
  try {
    return new URL(runtimeConfig(env).oidc.redirectUri).pathname;
  } catch {
    return "/auth/callback"; // unset: the SPA's default (auth/oidc.js)
  }
}

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

  const headers = securityHeaders(env);
  app.addHook("onSend", async (req, reply) => {
    reply.headers(req.url.split("?")[0] === headers.callbackPath ? headers.callback : headers.page);
  });

  // L-28: unauthenticated, so it names no internal service.
  app.get("/healthz", async () => ({ ok: true }));

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
        if (!raw.destroyed) console.error("SSE pump error:", err);
        // Cancel via the reader, not upstream.body: getReader() locked the stream,
        // so body.cancel() throws ERR_INVALID_STATE and crashes the process.
        reader.cancel().catch(() => {});
        try { raw.end(); } catch { /* socket already closed */ }
      });
    }
    pump();
  }

  // An upstream refusal (401 session revoked, 403 agent suspended / no access,
  // 409 ...) is plain JSON, not a stream. Pass its status and body through
  // instead of piping it as a 200 event stream, so the client's !resp.ok path
  // shows the reason. Returns true when it replied.
  async function sendUpstreamError(upstream, reply) {
    if (upstream.status >= 200 && upstream.status < 300) return false;
    const contentType = upstream.headers.get("content-type") ?? "application/json";
    reply.code(upstream.status).header("content-type", contentType).send(await upstream.text());
    return true;
  }

  // L-27: an upstream request the browser's connection owns. Aborted when that
  // connection closes (Stop, tab closed), while the upstream is still sending
  // headers or mid-stream: the abort closes the gateway connection, which is
  // what aborts the run there.
  function clientSignal(reply) {
    const ac = new AbortController();
    reply.raw.on("close", () => ac.abort());
    return ac.signal;
  }

  // L-28: every proxied route needs a bearer, so an anonymous client is turned
  // away before its body (up to bodyLimit) is read and forwarded.
  // Same test and answer as the gateway's requireUser.
  function hasBearer(req) {
    return (req.headers["authorization"] ?? "").startsWith("Bearer ");
  }
  async function requireBearer(req, reply) {
    if (!hasBearer(req)) reply.code(401).send({ error: "Authorization: Bearer <token> required" });
  }

  // Upstream failure reply. The error itself is logged, never sent: it names
  // internal hosts. Nothing is sent when the browser already left.
  function sendUnreachable(reply, what, err) {
    if (reply.raw.destroyed) return;
    console.error(`${what} request failed:`, err);
    if (err?.name === "TimeoutError") reply.code(504).send({ error: `${what} timed out` });
    else reply.code(502).send({ error: `${what} unreachable` });
  }

  // Buffered /api/* calls are bounded so a hung upstream cannot hold sockets
  // forever. Generous: some are LLM-backed or carry a 14 MiB upload.
  const API_TIMEOUT_MS = 5 * 60 * 1000;

  // /agui — forward POST to gateway, stream SSE response back.
  // The Authorization header from the browser is forwarded verbatim.
  app.post("/agui", { onRequest: requireBearer }, async (req, reply) => {
    const headers = { "content-type": "application/json" };
    if (req.headers["authorization"]) headers["authorization"] = req.headers["authorization"];
    // Copy accept header if present (AG-UI protocol).
    if (req.headers["accept"]) headers["accept"] = req.headers["accept"];
    if (req.headers["x-aikonos-agent"]) headers["x-aikonos-agent"] = req.headers["x-aikonos-agent"];
    try {
      const upstream = await fetch(`${GATEWAY_URL}/agui`, {
        method: "POST",
        headers,
        body: JSON.stringify(req.body),
        signal: clientSignal(reply),
      });
      if (await sendUpstreamError(upstream, reply)) return;
      reply.hijack();
      pipeSSE(upstream, reply.raw);
    } catch (err) {
      sendUnreachable(reply, "gateway", err);
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
    // After the 501: the Audit view probes without a token to detect it.
    if (!hasBearer(req)) return requireBearer(req, reply);
    const headers = { accept: "text/event-stream" };
    if (req.headers["authorization"]) headers["authorization"] = req.headers["authorization"];
    // Forward the request query string verbatim (F44: ?tenant= scoping).
    const qs = req.url.includes("?") ? req.url.slice(req.url.indexOf("?")) : "";
    try {
      const upstream = await fetch(`${OBSERVABILITY_URL}/api/audit/stream${qs}`, {
        method: "GET",
        headers,
        signal: clientSignal(reply),
      });
      if (await sendUpstreamError(upstream, reply)) return;
      reply.hijack();
      pipeSSE(upstream, reply.raw);
    } catch (err) {
      sendUnreachable(reply, "observability", err);
    }
  });

  // /api/workflows/:lineageId/run — POST. With ?stream=1 the gateway responds
  // with SSE (live per-step progress), which the buffering /api/* handler below
  // would swallow — pipe it unbuffered like /agui. Without ?stream=1 it is an
  // ordinary buffered JSON proxy (the blocking run path), byte-identical to /api/*.
  app.post("/api/workflows/:lineageId/run", { onRequest: requireBearer }, async (req, reply) => {
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
        signal: clientSignal(reply),
      });
      if (streaming) {
        if (await sendUpstreamError(upstream, reply)) return;
        reply.hijack();
        pipeSSE(upstream, reply.raw);
        return;
      }
      const text = await upstream.text();
      const contentType = upstream.headers.get("content-type") ?? "application/json";
      reply.code(upstream.status).header("content-type", contentType).send(text);
    } catch (err) {
      sendUnreachable(reply, "gateway", err);
    }
  });

  // /api/* — forward to gateway, strip /api prefix, forward Authorization.
  app.all("/api/*", { onRequest: requireBearer }, async (req, reply) => {
    const path = req.url.replace(/^\/api/, "");
    const target = GATEWAY_URL + path;
    const headers = {};
    if (req.headers["authorization"]) headers["authorization"] = req.headers["authorization"];
    const init = { method: req.method, headers, signal: AbortSignal.timeout(API_TIMEOUT_MS) };
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
      reply.code(res.status).header("content-type", contentType);
      // Keep an export's filename and its no-store intent (AG-14).
      for (const h of ["content-disposition", "cache-control"]) {
        const v = res.headers.get(h);
        if (v) reply.header(h, v);
      }
      reply.send(text);
    } catch (err) {
      sendUnreachable(reply, "gateway", err);
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
