// Proxy tests for server.mjs.
// Uses Fastify's inject() so no real port is opened.
// fetch is stubbed globally per-test — no outbound network calls.
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createServer, request } from "node:http";
import { buildApp, missingOidcRuntime } from "../server.mjs";

// The real fetch, for the one test that talks to a live upstream.
const realFetch = globalThis.fetch;

// Every proxied route needs a bearer (L-28); the tests send this one.
const AUTH = { authorization: "Bearer test-token" };

// Build a test app: no static serving (distDir nonexistent), injected gateway URL.
async function makeApp(fetchStub) {
  globalThis.fetch = fetchStub;
  const app = await buildApp({ gatewayUrl: "http://mock-gateway", distDir: "/nonexistent" });
  await app.ready();
  return app;
}

// Returns a fetch stub that records what it received into `captured`.
// Responds with a 200 JSON body unless overridden.
function captureFetch(captured, { status = 200, body = '{"upstream":"ok"}', contentType = null } = {}) {
  return async (url, init = {}) => {
    captured.url = url;
    captured.method = init.method ?? "GET";
    captured.headers = init.headers ?? {};
    captured.body = init.body;
    return {
      status,
      headers: { get: (name) => (name.toLowerCase() === "content-type" ? contentType : null) },
      text: async () => body,
    };
  };
}

// L-28: /healthz is unauthenticated, so it names no internal service.
test("GET /healthz returns ok and no upstream URLs", async (t) => {
  const app = await makeApp(captureFetch({}));
  t.after(() => app.close());

  const res = await app.inject({ method: "GET", url: "/healthz" });
  assert.equal(res.statusCode, 200);
  assert.deepEqual(JSON.parse(res.payload), { ok: true });
  assert.ok(!res.payload.includes("mock-gateway"), res.payload);
});

test("GET /api/sessions strips /api prefix and proxies to gateway", async (t) => {
  const req = {};
  const app = await makeApp(captureFetch(req));
  t.after(() => app.close());

  await app.inject({ method: "GET", url: "/api/sessions", headers: AUTH });
  assert.equal(req.url, "http://mock-gateway/sessions");
  assert.equal(req.method, "GET");
});

test("POST /api/foo with JSON body forwards application/json and JSON-encoded body", async (t) => {
  const req = {};
  const app = await makeApp(captureFetch(req));
  t.after(() => app.close());

  await app.inject({
    method: "POST",
    url: "/api/sessions",
    headers: { ...AUTH, "content-type": "application/json" },
    payload: JSON.stringify({ prompt: "hello" }),
  });
  assert.equal(req.headers["content-type"], "application/json");
  assert.equal(req.body, JSON.stringify({ prompt: "hello" }));
});

test("POST /api/admin/skills/upload with text/markdown forwards body verbatim (no 415)", async (t) => {
  const req = {};
  const app = await makeApp(captureFetch(req));
  t.after(() => app.close());

  const mdBody = "# My Skill\nThis is a test skill.";
  const res = await app.inject({
    method: "POST",
    url: "/api/admin/skills/upload",
    headers: { ...AUTH, "content-type": "text/markdown" },
    payload: mdBody,
  });
  // 415 would mean the content-type parser wasn't registered
  assert.notEqual(res.statusCode, 415, "should not reject text/markdown with 415");
  assert.equal(req.headers["content-type"], "text/markdown");
  assert.equal(Buffer.from(req.body).toString(), mdBody);
});

test("PUT /api/admin/skill-bundles/abc with application/zip forwards binary body verbatim", async (t) => {
  const req = {};
  const app = await makeApp(captureFetch(req));
  t.after(() => app.close());

  const zipBytes = Buffer.from([0x50, 0x4b, 0x03, 0x04]); // PK header
  await app.inject({
    method: "PUT",
    url: "/api/admin/skill-bundles/abc",
    headers: { ...AUTH, "content-type": "application/zip" },
    payload: zipBytes,
  });
  assert.equal(req.headers["content-type"], "application/zip");
  assert.deepEqual(Buffer.from(req.body), zipBytes);
});

test("DELETE /api/agents/abc sends no body and no content-type upstream", async (t) => {
  const req = {};
  const app = await makeApp(captureFetch(req));
  t.after(() => app.close());

  await app.inject({ method: "DELETE", url: "/api/agents/abc", headers: AUTH });
  assert.equal(req.body, undefined, "DELETE should not forward a body");
  assert.equal(req.headers["content-type"], undefined, "DELETE should not set content-type");
});

// L-28: the cause (it names internal hosts) goes to the log, not the browser.
test("gateway fetch error returns a generic 502", async (t) => {
  const app = await makeApp(async () => { throw new Error("connect ECONNREFUSED agent-gateway.internal:8080"); });
  t.after(() => app.close());

  const res = await app.inject({ method: "GET", url: "/api/status", headers: AUTH });
  assert.equal(res.statusCode, 502);
  assert.deepEqual(JSON.parse(res.payload), { error: "gateway unreachable" });
});

test("a gateway that does not answer in time gets a 504", async (t) => {
  const app = await makeApp(async () => { throw new DOMException("timed out", "TimeoutError"); });
  t.after(() => app.close());

  const res = await app.inject({ method: "GET", url: "/api/status", headers: AUTH });
  assert.equal(res.statusCode, 504);
});

test("buffered /api/* fetches carry a timeout signal", async (t) => {
  let signal;
  const app = await makeApp(async (_url, init) => {
    signal = init.signal;
    return { status: 200, headers: { get: () => null }, text: async () => "{}" };
  });
  t.after(() => app.close());

  await app.inject({ method: "GET", url: "/api/status", headers: AUTH });
  assert.ok(signal instanceof AbortSignal);
});

// L-28: an anonymous client is refused before its body is read or forwarded.
test("proxied routes refuse a request without a bearer and never call upstream", async (t) => {
  let called = false;
  const app = await makeApp(async () => { called = true; throw new Error("must not be called"); });
  t.after(() => app.close());

  for (const [method, url] of [["POST", "/api/files"], ["GET", "/api/me"], ["POST", "/agui"], ["POST", "/api/workflows/l1/run?stream=1"]]) {
    const res = await app.inject({
      method, url,
      headers: { "content-type": "application/json", authorization: "Basic eDp5" },
      payload: method === "POST" ? JSON.stringify({ big: "x".repeat(1024) }) : undefined,
    });
    assert.equal(res.statusCode, 401, `${method} ${url}`);
  }
  assert.equal(called, false);
});

test("GET /audit/stream without a bearer: 501 when unconfigured (the Audit view's probe), else 401", async (t) => {
  const bare = await buildApp({ observabilityUrl: "", distDir: "/nonexistent" });
  await bare.ready();
  t.after(() => bare.close());
  assert.equal((await bare.inject({ method: "GET", url: "/audit/stream" })).statusCode, 501);

  const app = await buildApp({ observabilityUrl: "http://mock-observability", distDir: "/nonexistent" });
  await app.ready();
  t.after(() => app.close());
  assert.equal((await app.inject({ method: "GET", url: "/audit/stream" })).statusCode, 401);
});

test("GET /audit/stream?tenant=t1 forwards the query string to the observability upstream", async (t) => {
  const req = {};
  // pipeSSE reads upstream.body via a WHATWG ReadableStream reader — supply an
  // immediately-empty one so the SSE pump finishes and inject() can resolve.
  globalThis.fetch = async (url, init = {}) => {
    req.url = url;
    req.headers = init.headers ?? {};
    return { status: 200, body: new ReadableStream({ start(controller) { controller.close(); } }) };
  };
  const app = await buildApp({ observabilityUrl: "http://mock-observability", distDir: "/nonexistent" });
  await app.ready();
  t.after(() => app.close());

  await app.inject({ method: "GET", url: "/audit/stream?tenant=t1", headers: AUTH });
  assert.equal(req.url, "http://mock-observability/api/audit/stream?tenant=t1");
});

test("POST /api/workflows/:id/run?stream=1 pipes SSE (forwards stream param + accept)", async (t) => {
  const req = {};
  // SSE path: reply.hijack() + pipeSSE reads upstream.body via a WHATWG reader.
  globalThis.fetch = async (url, init = {}) => {
    req.url = url;
    req.method = init.method;
    req.headers = init.headers ?? {};
    req.body = init.body;
    return { status: 200, body: new ReadableStream({ start(c) {
      c.enqueue(new TextEncoder().encode("event: result\ndata: {\"ok\":true}\n\n"));
      c.close();
    } }) };
  };
  const app = await buildApp({ gatewayUrl: "http://mock-gateway", distDir: "/nonexistent" });
  await app.ready();
  t.after(() => app.close());

  const res = await app.inject({
    method: "POST",
    url: "/api/workflows/l1/run?stream=1",
    headers: { ...AUTH, "content-type": "application/json" },
    payload: JSON.stringify({ inputs: { q: "x" } }),
  });
  assert.equal(req.url, "http://mock-gateway/workflows/l1/run?stream=1");
  assert.equal(req.headers["accept"], "text/event-stream");
  assert.equal(res.headers["content-type"], "text/event-stream");
  assert.match(res.payload, /event: result/);
});

test("POST /api/workflows/:id/run without stream buffers JSON like /api/*", async (t) => {
  const req = {};
  const app = await makeApp(captureFetch(req, { body: '{"ok":true,"result":{}}' }));
  t.after(() => app.close());

  const res = await app.inject({
    method: "POST",
    url: "/api/workflows/l1/run",
    headers: { ...AUTH, "content-type": "application/json" },
    payload: JSON.stringify({ inputs: {} }),
  });
  assert.equal(req.url, "http://mock-gateway/workflows/l1/run");
  assert.notEqual(res.headers["content-type"], "text/event-stream");
  assert.equal(JSON.parse(res.payload).ok, true);
});

test("Authorization header is forwarded verbatim to gateway", async (t) => {
  const req = {};
  const app = await makeApp(captureFetch(req));
  t.after(() => app.close());

  await app.inject({
    method: "GET",
    url: "/api/me",
    headers: { authorization: "Bearer test-token" },
  });
  assert.equal(req.headers["authorization"], "Bearer test-token");
});

test("/api/* response forwards the upstream content-type verbatim (CP4)", async (t) => {
  globalThis.fetch = async () => ({
    status: 200,
    headers: { get: (name) => (name.toLowerCase() === "content-type" ? "text/csv" : null) },
    text: async () => "a,b\n1,2",
  });
  const app = await buildApp({ gatewayUrl: "http://mock-gateway", distDir: "/nonexistent" });
  await app.ready();
  t.after(() => app.close());

  const res = await app.inject({ method: "GET", url: "/api/access-control/export", headers: AUTH });
  assert.equal(res.headers["content-type"], "text/csv");
});

test("/api/* response falls back to application/json when upstream omits content-type (CP4)", async (t) => {
  globalThis.fetch = async () => ({
    status: 200,
    headers: { get: () => null },
    text: async () => '{"ok":true}',
  });
  const app = await buildApp({ gatewayUrl: "http://mock-gateway", distDir: "/nonexistent" });
  await app.ready();
  t.after(() => app.close());

  const res = await app.inject({ method: "GET", url: "/api/status", headers: AUTH });
  // Fastify appends "; charset=utf-8" to a bare application/json content-type header
  // on send() — pre-existing behavior, unrelated to the CP4 passthrough fix.
  assert.ok(res.headers["content-type"].startsWith("application/json"), res.headers["content-type"]);
});

// Runtime config: build an app with an explicit env and fetch /runtime-config.js.
async function getRuntimeConfig(t, env) {
  const app = await buildApp({ gatewayUrl: "http://mock-gateway", distDir: "/nonexistent", env });
  await app.ready();
  t.after(() => app.close());
  return app.inject({ method: "GET", url: "/runtime-config.js" });
}

// Runs a /runtime-config.js body the way a browser would and returns what it
// assigned to window.__AIKONOS_CONFIG__.
function evalRuntimeConfig(js) {
  const window = {};
  new Function("window", js)(window);
  return window.__AIKONOS_CONFIG__;
}

test("GET /runtime-config.js serves the AIKONOS_WEBUI_OIDC_* env as window.__AIKONOS_CONFIG__", async (t) => {
  const res = await getRuntimeConfig(t, {
    AIKONOS_WEBUI_OIDC_AUTHORITY: "https://login.example.com/tenant/v2.0",
    AIKONOS_WEBUI_OIDC_CLIENT: "client-123",
    AIKONOS_WEBUI_OIDC_REDIRECT_URI: "https://aikonos.example.com/auth/callback",
    AIKONOS_WEBUI_OIDC_SCOPE: "openid profile email",
    AIKONOS_WEBUI_OIDC_TOKEN: "id",
  });
  assert.equal(res.statusCode, 200);
  assert.ok(res.headers["content-type"].startsWith("application/javascript"), res.headers["content-type"]);
  assert.equal(res.headers["cache-control"], "no-store");
  assert.deepEqual(evalRuntimeConfig(res.payload), {
    oidc: {
      authority: "https://login.example.com/tenant/v2.0",
      clientId: "client-123",
      redirectUri: "https://aikonos.example.com/auth/callback",
      scope: "openid profile email",
      token: "id",
    },
  });
});

test("GET /runtime-config.js leaves out unset and blank settings so build-time values apply", async (t) => {
  const res = await getRuntimeConfig(t, {
    AIKONOS_WEBUI_OIDC_CLIENT: "   ",
    AIKONOS_WEBUI_OIDC_SCOPE: "",
    AIKONOS_WEBUI_OIDC_TOKEN: " access ",
  });
  assert.deepEqual(evalRuntimeConfig(res.payload), { oidc: { token: "access" } });

  const empty = await getRuntimeConfig(t, {});
  assert.deepEqual(evalRuntimeConfig(empty.payload), { oidc: {} });
});

test("GET /runtime-config.js exposes only the OIDC settings, never other env", async (t) => {
  const res = await getRuntimeConfig(t, {
    AIKONOS_WEBUI_OIDC_CLIENT: "client-123",
    AIKONOS_API_KEY_PEPPER: "pepper-must-not-leak",
    OPENROUTER_API_KEY: "key-must-not-leak",
  });
  assert.ok(!res.payload.includes("must-not-leak"), res.payload);
  assert.deepEqual(evalRuntimeConfig(res.payload), { oidc: { clientId: "client-123" } });
});

test("a runtime-config value cannot break out of the assignment", async (t) => {
  const hostile = '"};globalThis.pwned=1;//</script><script>';
  const res = await getRuntimeConfig(t, { AIKONOS_WEBUI_OIDC_SCOPE: hostile });
  assert.equal(evalRuntimeConfig(res.payload).oidc.scope, hostile);
  assert.equal(globalThis.pwned, undefined);
});

// Desktop settings: build an app with an explicit env and fetch /desktop.json.
async function getDesktopConfig(t, env) {
  const app = await buildApp({ gatewayUrl: "http://mock-gateway", distDir: "/nonexistent", env });
  await app.ready();
  t.after(() => app.close());
  return app.inject({ method: "GET", url: "/desktop.json" });
}

test("GET /desktop.json shares the web console's identity provider but never its client id", async (t) => {
  const res = await getDesktopConfig(t, {
    AIKONOS_WEBUI_OIDC_AUTHORITY: "https://login.example.com/tenant/v2.0",
    AIKONOS_WEBUI_OIDC_CLIENT: "web-client",
    AIKONOS_WEBUI_OIDC_REDIRECT_URI: "https://aikonos.example.com/auth/callback",
    AIKONOS_WEBUI_OIDC_SCOPE: "openid profile email",
    AIKONOS_WEBUI_OIDC_TOKEN: "id",
  });
  assert.equal(res.statusCode, 200);
  assert.ok(res.headers["content-type"].startsWith("application/json"), res.headers["content-type"]);
  assert.equal(res.headers["cache-control"], "no-store");
  assert.deepEqual(JSON.parse(res.payload), {
    oidc: {
      authority: "https://login.example.com/tenant/v2.0",
      scope: "openid profile email",
      token: "id",
      clientId: "aikonos-desktop",
    },
  });
});

test("GET /desktop.json serves the AIKONOS_DESKTOP_* client and release settings", async (t) => {
  const res = await getDesktopConfig(t, {
    AIKONOS_DESKTOP_OIDC_CLIENT: " desktop-client ",
    AIKONOS_DESKTOP_VERSION: "0.3.0",
    AIKONOS_DESKTOP_MINIMUM_VERSION: "0.2.0",
    AIKONOS_DESKTOP_URL: "https://downloads.example.com/aikonos-0.3.0.msi",
    AIKONOS_DESKTOP_NOTES: "Faster uploads.",
  });
  assert.deepEqual(JSON.parse(res.payload), {
    oidc: { clientId: "desktop-client" },
    release: {
      version: "0.3.0",
      minimumVersion: "0.2.0",
      url: "https://downloads.example.com/aikonos-0.3.0.msi",
      notes: "Faster uploads.",
    },
  });
});

test("GET /desktop.json: a minimum version alone still turns old builds away", async (t) => {
  const res = await getDesktopConfig(t, { AIKONOS_DESKTOP_MINIMUM_VERSION: "0.2.0", AIKONOS_DESKTOP_URL: "  " });
  assert.deepEqual(JSON.parse(res.payload).release, { version: "0.2.0", minimumVersion: "0.2.0" });

  // A download link with no version to offer is left out.
  const none = await getDesktopConfig(t, { AIKONOS_DESKTOP_URL: "https://downloads.example.com/x.msi" });
  assert.deepEqual(JSON.parse(none.payload), { oidc: { clientId: "aikonos-desktop" } });
});

test("GET /desktop.json exposes only its public settings, never other env", async (t) => {
  const res = await getDesktopConfig(t, {
    AIKONOS_DESKTOP_VERSION: "0.3.0",
    AIKONOS_API_KEY_PEPPER: "pepper-must-not-leak",
    OPENROUTER_API_KEY: "key-must-not-leak",
  });
  assert.ok(!res.payload.includes("must-not-leak"), res.payload);
});

test("GET /desktop.json answers JSON even where the SPA fallback answers unknown paths", async (t) => {
  const dist = mkdtempSync(join(tmpdir(), "aikonos-dist-"));
  t.after(() => rmSync(dist, { recursive: true, force: true }));
  writeFileSync(join(dist, "index.html"), "<!doctype html><title>aikonOS</title>");
  const app = await buildApp({ gatewayUrl: "http://mock-gateway", distDir: dist, env: {} });
  await app.ready();
  t.after(() => app.close());

  const page = await app.inject({ method: "GET", url: "/no-such-file.json" });
  assert.ok(page.headers["content-type"].startsWith("text/html"), page.headers["content-type"]);

  const res = await app.inject({ method: "GET", url: "/desktop.json" });
  assert.ok(res.headers["content-type"].startsWith("application/json"), res.headers["content-type"]);
  assert.equal(JSON.parse(res.payload).oidc.clientId, "aikonos-desktop");
});

test("POST /agui passes a gateway refusal through with its status and body, not as a 200 stream", async (t) => {
  const req = {};
  const app = await makeApp(captureFetch(req, {
    status: 403, body: '{"error":"agent is suspended (agent_suspended)"}', contentType: "application/json",
  }));
  t.after(() => app.close());

  const res = await app.inject({
    method: "POST",
    url: "/agui",
    headers: { ...AUTH, "content-type": "application/json", "x-aikonos-agent": "a1" },
    payload: JSON.stringify({ prompt: "hi" }),
  });
  assert.equal(res.statusCode, 403);
  assert.notEqual(res.headers["content-type"], "text/event-stream");
  assert.equal(JSON.parse(res.payload).error, "agent is suspended (agent_suspended)");
  assert.equal(req.headers["x-aikonos-agent"], "a1");
});

test("POST /api/workflows/:id/run?stream=1 passes a 401 through instead of streaming it", async (t) => {
  const app = await makeApp(captureFetch({}, { status: 401, body: '{"error":"session revoked; sign in again"}' }));
  t.after(() => app.close());

  const res = await app.inject({ method: "POST", url: "/api/workflows/l1/run?stream=1", headers: AUTH, payload: {} });
  assert.equal(res.statusCode, 401);
  assert.match(res.payload, /session revoked/);
});

test("/api/* forwards content-disposition and cache-control (AG-14 export)", async (t) => {
  globalThis.fetch = async () => ({
    status: 200,
    headers: { get: (n) => ({ "content-type": "text/csv", "content-disposition": 'attachment; filename="x.csv"', "cache-control": "no-store" })[n.toLowerCase()] ?? null },
    text: async () => "a,b",
  });
  const app = await buildApp({ gatewayUrl: "http://mock-gateway", distDir: "/nonexistent" });
  await app.ready();
  t.after(() => app.close());

  const res = await app.inject({ method: "GET", url: "/api/admin/agents/inventory?format=csv", headers: AUTH });
  assert.equal(res.headers["cache-control"], "no-store");
  assert.equal(res.headers["content-disposition"], 'attachment; filename="x.csv"');
});

// M-26: every response carries the browser-side backstop headers.
test("security headers on the SPA, /runtime-config.js and /api/*", async (t) => {
  const dist = mkdtempSync(join(tmpdir(), "aikonos-dist-"));
  t.after(() => rmSync(dist, { recursive: true, force: true }));
  writeFileSync(join(dist, "index.html"), "<!doctype html><title>aikonOS</title>");
  globalThis.fetch = captureFetch({});
  const app = await buildApp({
    gatewayUrl: "http://mock-gateway",
    distDir: dist,
    env: { AIKONOS_WEBUI_OIDC_AUTHORITY: "https://login.example.com/tenant/v2.0" },
  });
  await app.ready();
  t.after(() => app.close());

  for (const [url, headers] of [["/", {}], ["/chat", {}], ["/runtime-config.js", {}], ["/api/me", AUTH], ["/api/me", {}]]) {
    const res = await app.inject({ method: "GET", url, headers });
    const csp = res.headers["content-security-policy"] ?? "";
    assert.match(csp, /default-src 'self'/, url);
    assert.match(csp, /script-src 'self';/, url);
    assert.match(csp, /img-src 'self' data:;/, url);
    assert.match(csp, /connect-src 'self' https:\/\/login\.example\.com;/, url);
    assert.match(csp, /frame-ancestors 'none'/, url);
    assert.match(csp, /object-src 'none'/, url);
    assert.match(csp, /base-uri 'none'/, url);
    assert.equal(res.headers["x-frame-options"], "DENY", url);
    assert.equal(res.headers["x-content-type-options"], "nosniff", url);
    assert.equal(res.headers["referrer-policy"], "no-referrer", url);
  }
});

test("only /auth/callback may be framed, by this origin (silent renew)", async (t) => {
  const dist = mkdtempSync(join(tmpdir(), "aikonos-dist-"));
  t.after(() => rmSync(dist, { recursive: true, force: true }));
  writeFileSync(join(dist, "index.html"), "<!doctype html><title>aikonOS</title>");
  const app = await buildApp({ gatewayUrl: "http://mock-gateway", distDir: dist, env: {} });
  await app.ready();
  t.after(() => app.close());

  const res = await app.inject({ method: "GET", url: "/auth/callback?code=c&state=s" });
  assert.match(res.headers["content-security-policy"], /frame-ancestors 'self'/);
  assert.equal(res.headers["x-frame-options"], "SAMEORIGIN");
  // No runtime authority: the SPA's own default identity provider is allowed.
  assert.match(res.headers["content-security-policy"], /connect-src 'self' http:\/\/localhost:18080;/);
});

// L-27: Stop or a closed tab must reach the gateway, which aborts the run only
// when its own connection closes.
test("a browser that disconnects from /agui closes the upstream connection", async (t) => {
  let upstreamClosed;
  const closed = new Promise((r) => (upstreamClosed = r));
  const gateway = createServer((_req, res) => {
    res.writeHead(200, { "content-type": "text/event-stream" });
    res.write("data: {}\n\n");
    res.on("close", upstreamClosed);
  });
  await new Promise((r) => gateway.listen(0, "127.0.0.1", r));
  // closeAllConnections: a failing run must not hang on the held stream.
  t.after(() => { gateway.closeAllConnections(); gateway.close(); });

  globalThis.fetch = realFetch;
  const app = await buildApp({ gatewayUrl: `http://127.0.0.1:${gateway.address().port}`, distDir: "/nonexistent" });
  await app.listen({ port: 0, host: "127.0.0.1" });
  t.after(() => app.close());

  const req = request({
    host: "127.0.0.1", port: app.server.address().port, path: "/agui", method: "POST",
    headers: { ...AUTH, "content-type": "application/json" },
  });
  req.on("error", () => {});
  req.end("{}");
  const res = await new Promise((r) => req.on("response", r));
  await new Promise((r) => res.once("data", r));
  req.destroy(); // the browser goes away

  await Promise.race([closed, new Promise((_, rej) => setTimeout(() => rej(new Error("upstream stayed open")), 2000))]);
});

test("a configured redirect URI is the page silent renew may frame", async (t) => {
  const app = await buildApp({
    gatewayUrl: "http://mock-gateway",
    distDir: "/nonexistent",
    env: { AIKONOS_WEBUI_OIDC_REDIRECT_URI: "https://console.example.com/signin/cb" },
  });
  await app.ready();
  t.after(() => app.close());

  assert.equal((await app.inject({ method: "GET", url: "/signin/cb?code=c" })).headers["x-frame-options"], "SAMEORIGIN");
  assert.equal((await app.inject({ method: "GET", url: "/auth/callback" })).headers["x-frame-options"], "DENY");
});

// The CSP and frame policy cannot see a bundle-baked VITE_OIDC_*: startup
// warns about each runtime setting they would otherwise default.
test("the runtime OIDC settings the CSP needs are reported when unset", () => {
  assert.deepEqual(missingOidcRuntime({}), ["AIKONOS_WEBUI_OIDC_AUTHORITY", "AIKONOS_WEBUI_OIDC_REDIRECT_URI"]);
  assert.deepEqual(missingOidcRuntime({ AIKONOS_WEBUI_OIDC_AUTHORITY: "https://idp.example.com/realm", AIKONOS_WEBUI_OIDC_REDIRECT_URI: " " }), ["AIKONOS_WEBUI_OIDC_REDIRECT_URI"]);
  assert.deepEqual(missingOidcRuntime({
    AIKONOS_WEBUI_OIDC_AUTHORITY: "https://idp.example.com/realm",
    AIKONOS_WEBUI_OIDC_REDIRECT_URI: "https://console.example.com/auth/callback",
  }), []);
});
