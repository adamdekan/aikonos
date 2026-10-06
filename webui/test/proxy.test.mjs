// Proxy tests for server.mjs.
// Uses Fastify's inject() so no real port is opened.
// fetch is stubbed globally per-test — no outbound network calls.
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { buildApp } from "../server.mjs";

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

test("GET /healthz returns ok with gateway URL", async (t) => {
  const app = await makeApp(captureFetch({}));
  t.after(() => app.close());

  const res = await app.inject({ method: "GET", url: "/healthz" });
  assert.equal(res.statusCode, 200);
  const b = JSON.parse(res.payload);
  assert.equal(b.ok, true);
  assert.equal(b.gateway, "http://mock-gateway");
});

test("GET /api/sessions strips /api prefix and proxies to gateway", async (t) => {
  const req = {};
  const app = await makeApp(captureFetch(req));
  t.after(() => app.close());

  await app.inject({ method: "GET", url: "/api/sessions" });
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
    headers: { "content-type": "application/json" },
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
    headers: { "content-type": "text/markdown" },
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
    headers: { "content-type": "application/zip" },
    payload: zipBytes,
  });
  assert.equal(req.headers["content-type"], "application/zip");
  assert.deepEqual(Buffer.from(req.body), zipBytes);
});

test("DELETE /api/agents/abc sends no body and no content-type upstream", async (t) => {
  const req = {};
  const app = await makeApp(captureFetch(req));
  t.after(() => app.close());

  await app.inject({ method: "DELETE", url: "/api/agents/abc" });
  assert.equal(req.body, undefined, "DELETE should not forward a body");
  assert.equal(req.headers["content-type"], undefined, "DELETE should not set content-type");
});

test("gateway fetch error returns 502 with error message", async (t) => {
  const app = await makeApp(async () => { throw new Error("ECONNREFUSED"); });
  t.after(() => app.close());

  const res = await app.inject({ method: "GET", url: "/api/status" });
  assert.equal(res.statusCode, 502);
  const b = JSON.parse(res.payload);
  assert.ok(b.error.startsWith("gateway unreachable:"), `unexpected error: ${b.error}`);
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

  await app.inject({ method: "GET", url: "/audit/stream?tenant=t1" });
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
    headers: { "content-type": "application/json" },
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
    headers: { "content-type": "application/json" },
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

  const res = await app.inject({ method: "GET", url: "/api/access-control/export" });
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

  const res = await app.inject({ method: "GET", url: "/api/status" });
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
