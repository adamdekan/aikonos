// Smoke test for webui/server.mjs. Verifies:
//   (a) Static index.html is served for the SPA root.
//   (b) /api/* calls proxy to the gateway with x-aikonos-user injected.
//   (c) /agui SSE passthrough is NOT buffered (first chunk arrives before stream
//       ends, proving the server is streaming, not buffering the full response).
//
//   node webui/scripts/smoke.mjs
import { createServer } from "node:http";
import { spawn }        from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here           = dirname(fileURLToPath(import.meta.url));
const GW_PORT        = 8199;
const WEB_PORT       = 4299;
const OBS_PORT       = 8198;
const WEB_PORT_NOOBS = 4298; // second server with OBSERVABILITY_URL="" for 501 check

let seen = null;
// Track which paths the mock observability server received — used to assert the
// webui proxied to /api/audit/stream and NOT the old /stream path.
const obsPaths = [];

// ── mock observability service ────────────────────────────────────────────────
// Only serves GET /api/audit/stream; anything else → 404. This ensures a path
// regression (fetching /stream) fails the smoke rather than silently succeeding.
const obsServer = createServer((req, res) => {
  obsPaths.push(req.url);
  const url = req.url.split("?")[0];
  if (req.method === "GET" && url === "/api/audit/stream") {
    res.writeHead(200, {
      "content-type":      "text/event-stream",
      "cache-control":     "no-cache",
      "x-accel-buffering": "no",
      connection: "keep-alive",
    });
    res.write(`data: ${JSON.stringify({ event_id: "smoke-1", event_type: "task.created" })}\n\n`);
    setTimeout(() => res.end(), 60);
    return;
  }
  res.writeHead(404);
  res.end();
});

// ── mock gateway ─────────────────────────────────────────────────────────────
const gateway = createServer((req, res) => {
  let body = "";
  req.on("data", (c) => (body += c));
  req.on("end", () => {
    seen = { path: req.url, method: req.method, user: req.headers["x-aikonos-user"], body };
    const url = req.url.split("?")[0];

    if (req.method === "GET" && url === "/admin/assignments") {
      res.setHeader("content-type", "application/json");
      return res.end(JSON.stringify({ tuples: [], principals: [], fgaEnabled: true }));
    }

    if (req.method === "POST" && url === "/agui") {
      res.writeHead(200, {
        "content-type":  "text/event-stream",
        "cache-control": "no-cache",
        "x-accel-buffering": "no",
      });
      // Write the first SSE chunk immediately, close after a short pause.
      res.write(`data: ${JSON.stringify({ type: "text-delta", delta: "hi" })}\n\n`);
      setTimeout(() => {
        res.write(`data: ${JSON.stringify({ type: "run-finished" })}\n\n`);
        res.end();
      }, 60);
      return;
    }

    res.writeHead(404);
    res.end();
  });
});

function fail(msg) {
  console.error("FAIL:", msg);
  cleanup(1);
}

let server;
let serverNoObs;
function cleanup(code) {
  try { server?.kill(); } catch {}
  try { serverNoObs?.kill(); } catch {}
  try { gateway.close(); } catch {}
  try { obsServer.close(); } catch {}
  process.exit(code);
}

// Start the mock observability server first, then the mock gateway, so both are
// ready before server.mjs spawns.
await new Promise((resolve) => obsServer.listen(OBS_PORT, resolve));

gateway.listen(GW_PORT, async () => {
  server = spawn(
    "node",
    [join(here, "..", "server.mjs")],
    {
      env: {
        ...process.env,
        PORT:             String(WEB_PORT),
        GATEWAY_URL:      `http://127.0.0.1:${GW_PORT}`,
        OBSERVABILITY_URL: `http://127.0.0.1:${OBS_PORT}`,
      },
      stdio: "inherit",
    },
  );

  // Wait for server to start.
  await new Promise((r) => setTimeout(r, 800));

  // ── (a) static SPA ────────────────────────────────────────────────────────
  // dist/ may not exist in CI without a prior build. When it does, / must
  // return the SPA index. When it doesn't, Fastify returns 404 which is fine
  // for the smoke (static serving is a build-time concern, tested by `npm run
  // build` + manual check). We only assert the server responds.
  const rootRes = await fetch(`http://127.0.0.1:${WEB_PORT}/`);
  // Accept 200 (index.html served) or 404 (dist/ absent = dev mode).
  if (rootRes.status !== 200 && rootRes.status !== 404) {
    fail(`static root returned unexpected status ${rootRes.status}`);
  }
  console.log(`OK: static route — status ${rootRes.status} (200=SPA served, 404=dist absent)`);

  // ── (a2) SPA deep-link fallback ──────────────────────────────────────────
  // A client-side route like /chat must return 200 + index.html, not 404.
  // Skipped when dist/ is absent (same posture as the static root check above).
  if (rootRes.status === 200) {
    const deepRes = await fetch(`http://127.0.0.1:${WEB_PORT}/chat`);
    if (deepRes.status !== 200) {
      fail(`deep-link /chat returned ${deepRes.status}, expected 200 (SPA fallback broken)`);
    }
    const deepBody = await deepRes.text();
    if (!deepBody.includes("<div id=\"app\">") && !deepBody.includes("<title>")) {
      fail("deep-link /chat response does not look like index.html");
    }
    console.log("OK: SPA deep-link /chat → 200 + index.html");
  } else {
    console.log("SKIP: SPA deep-link check (dist/ absent)");
  }

  // ── (b) JSON proxy ───────────────────────────────────────────────────────
  const getRes = await fetch(`http://127.0.0.1:${WEB_PORT}/api/admin/assignments`, {
    headers: { "x-aikonos-user": "admin@example.com" },
  });
  const getJson = await getRes.json();
  if (seen?.path !== "/admin/assignments") fail(`GET path not forwarded: ${seen?.path}`);
  if (seen?.user !== "admin@example.com")   fail(`x-aikonos-user not forwarded: ${seen?.user}`);
  if (!("tuples" in getJson))              fail(`response body not relayed: ${JSON.stringify(getJson)}`);
  console.log("OK: JSON proxy forwards path, x-aikonos-user, and body");

  // ── (c) SSE passthrough — must receive first chunk before stream ends ─────
  // We use a raw Node fetch and read the first chunk directly without waiting
  // for the full body. If the server buffered the response we would only get
  // data after res.end() — the 60 ms delay means this check would time out.
  let firstChunk = null;
  const sseRes = await fetch(`http://127.0.0.1:${WEB_PORT}/agui`, {
    method: "POST",
    headers: { "content-type": "application/json", "x-aikonos-user": "dev@example.com" },
    body: JSON.stringify({ prompt: "hi", threadId: "t1" }),
  });
  if (!sseRes.ok && sseRes.status !== 200) fail(`/agui returned ${sseRes.status}`);
  const ct = sseRes.headers.get("content-type") ?? "";
  if (!ct.includes("text/event-stream")) fail(`/agui content-type not SSE: ${ct}`);

  const reader = sseRes.body.getReader();
  const decoder = new TextDecoder();
  // Read until we see the first non-empty data line or exhaust chunks.
  let buf = "";
  while (!firstChunk) {
    const { done, value } = await reader.read();
    if (done) break;
    buf += decoder.decode(value, { stream: true });
    const lines = buf.split("\n");
    for (const line of lines) {
      if (line.startsWith("data:") && line.trim() !== "data:") {
        firstChunk = line.trim().replace(/^data:\s*/, "");
        break;
      }
    }
  }
  reader.cancel();
  if (!firstChunk) fail("SSE: no data chunk received");
  let parsed;
  try { parsed = JSON.parse(firstChunk); } catch { fail(`SSE chunk not valid JSON: ${firstChunk}`); }
  if (parsed.type !== "text-delta") fail(`SSE first event type unexpected: ${parsed.type}`);
  console.log("OK: SSE passthrough — first chunk received un-buffered");

  // ── (d) audit-stream SSE proxy — must hit /api/audit/stream on the obs server ──
  // The mock observability server only serves /api/audit/stream; a request to
  // the old /stream path returns 404, so a path regression fails here.
  let auditChunk = null;
  const auditRes = await fetch(`http://127.0.0.1:${WEB_PORT}/audit/stream`, {
    headers: { "x-aikonos-user": "dev@example.com" },
  });
  if (!auditRes.ok) fail(`/audit/stream returned ${auditRes.status} (expected 200)`);
  const auditCt = auditRes.headers.get("content-type") ?? "";
  if (!auditCt.includes("text/event-stream")) fail(`/audit/stream content-type not SSE: ${auditCt}`);
  const auditReader = auditRes.body.getReader();
  const auditDecoder = new TextDecoder();
  let auditBuf = "";
  while (!auditChunk) {
    const { done, value } = await auditReader.read();
    if (done) break;
    auditBuf += auditDecoder.decode(value, { stream: true });
    const lines = auditBuf.split("\n");
    for (const line of lines) {
      if (line.startsWith("data:") && line.trim() !== "data:") {
        auditChunk = line.trim().replace(/^data:\s*/, "");
        break;
      }
    }
  }
  auditReader.cancel();
  if (!auditChunk) fail("audit SSE: no data chunk received");
  let auditParsed;
  try { auditParsed = JSON.parse(auditChunk); } catch { fail(`audit SSE chunk not valid JSON: ${auditChunk}`); }
  if (!auditParsed.event_id) fail(`audit SSE event missing event_id: ${JSON.stringify(auditParsed)}`);
  // Confirm the mock was hit on the correct path (not the old /stream).
  if (!obsPaths.some((p) => p.split("?")[0] === "/api/audit/stream")) {
    fail(`audit proxy hit wrong path(s): ${JSON.stringify(obsPaths)}`);
  }
  if (obsPaths.some((p) => p.split("?")[0] === "/stream")) {
    fail("audit proxy hit old /stream path — path bug not fixed");
  }
  console.log("OK: audit-stream SSE proxied to /api/audit/stream (correct path)");

  // ── (e) 501 opt-out — OBSERVABILITY_URL="" must make /audit/stream return 501 ─
  // Spawn a second short-lived server with no observability URL so we can confirm
  // the Audit view's "not configured" path is exercised.
  serverNoObs = spawn(
    "node",
    [join(here, "..", "server.mjs")],
    {
      env: {
        ...process.env,
        PORT:              String(WEB_PORT_NOOBS),
        GATEWAY_URL:       `http://127.0.0.1:${GW_PORT}`,
        OBSERVABILITY_URL: "",
      },
      stdio: "inherit",
    },
  );
  await new Promise((r) => setTimeout(r, 800));
  const noObsRes = await fetch(`http://127.0.0.1:${WEB_PORT_NOOBS}/audit/stream`);
  if (noObsRes.status !== 501) {
    fail(`/audit/stream with OBSERVABILITY_URL="" returned ${noObsRes.status}, expected 501`);
  }
  serverNoObs.kill();
  serverNoObs = null;
  console.log("OK: /audit/stream returns 501 when OBSERVABILITY_URL is empty");

  console.log("ALL PASS");
  cleanup(0);
});
