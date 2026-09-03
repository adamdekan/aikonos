// test/mcp-echo-server/server.js
//
// DEV ONLY — minimal MCP echo server for in-cluster e2e tests.
// Implements streamable-HTTP MCP JSON-RPC 2.0 on a single POST endpoint.
// No production dependencies — Node built-ins only.
//
// Supported methods:
//   initialize               → protocolVersion + capabilities + Mcp-Session-Id header
//   notifications/initialized → 202 no-op
//   tools/list               → one tool: echo(text)
//   tools/call               → echo: <text>
"use strict";

const http = require("http");
const crypto = require("crypto");

const PORT = Number(process.env.PORT ?? 8000);

// Per-connection session ids issued on initialize; echoed on subsequent requests.
// In-process only — this server is single-replica dev tooling.
const sessions = new Set();

function respond(res, id, result) {
  const body = JSON.stringify({ jsonrpc: "2.0", id, result });
  res.writeHead(200, { "content-type": "application/json" });
  res.end(body);
}

function respondError(res, id, code, message) {
  const body = JSON.stringify({
    jsonrpc: "2.0",
    id,
    error: { code, message },
  });
  res.writeHead(200, { "content-type": "application/json" });
  res.end(body);
}

const TOOLS = [
  {
    name: "echo",
    description: "Echoes the input text back with an 'echo: ' prefix.",
    inputSchema: {
      type: "object",
      properties: {
        text: { type: "string", description: "Text to echo" },
      },
      required: ["text"],
    },
    // echo has no side effects; advertise readOnlyHint so the broker classifies
    // it READ_ONLY (auto-approved) rather than the WRITE_EXTERNAL default that
    // forces human approval for un-annotated MCP tools.
    annotations: { readOnlyHint: true },
  },
];

function handle(req, res) {
  // Kubernetes liveness/readiness probe.
  if (req.method === "GET" && req.url === "/healthz") {
    res.writeHead(200, { "content-type": "application/json" });
    res.end('{"ok":true}');
    return;
  }

  if (req.method !== "POST") {
    res.writeHead(405);
    res.end();
    return;
  }

  let raw = "";
  req.on("data", (chunk) => { raw += chunk; });
  req.on("end", () => {
    let rpc;
    try {
      rpc = JSON.parse(raw);
    } catch {
      res.writeHead(400);
      res.end("bad json");
      return;
    }

    const { id, method, params } = rpc;

    if (method === "initialize") {
      const sessionId = crypto.randomUUID();
      sessions.add(sessionId);
      res.setHeader("Mcp-Session-Id", sessionId);
      respond(res, id, {
        protocolVersion: "2025-06-18",
        capabilities: { tools: {} },
        serverInfo: { name: "mcp-echo-server", version: "0.1.0" },
      });
      return;
    }

    if (method === "notifications/initialized") {
      // Notification: no response body needed.
      res.writeHead(202);
      res.end();
      return;
    }

    if (method === "tools/list") {
      respond(res, id, { tools: TOOLS });
      return;
    }

    if (method === "tools/call") {
      const name = params?.name;
      const args = params?.arguments ?? {};
      if (name !== "echo") {
        respondError(res, id, -32601, `unknown tool: ${name}`);
        return;
      }
      const text = typeof args.text === "string" ? args.text : String(args.text ?? "");
      respond(res, id, {
        content: [{ type: "text", text: `echo: ${text}` }],
        isError: false,
      });
      return;
    }

    respondError(res, id, -32601, `method not found: ${method}`);
  });
}

const server = http.createServer(handle);
server.listen(PORT, () => {
  console.log(`mcp-echo-server listening on :${PORT}`);
});
