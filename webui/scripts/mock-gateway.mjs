// Dev-only mock of the agent-gateway + observability service, so the webui can
// be driven in a browser or smoke tests without a live cluster. Handles:
//   GET  /admin/assignments        — returns a stub tuple list
//   POST /agui                     — emits a minimal AG-UI SSE stream
//   GET  /stream                   — emits a minimal audit SSE stream (observability)
//
//   node webui/scripts/mock-gateway.mjs   # listens on :8090
import { createServer } from "node:http";

const PORT = Number(process.env.PORT ?? 8090);

function sseResponse(res, events) {
  res.writeHead(200, {
    "content-type":  "text/event-stream",
    "cache-control": "no-cache",
    "x-accel-buffering": "no",
    connection: "keep-alive",
  });
  for (const ev of events) {
    res.write(`data: ${JSON.stringify(ev)}\n\n`);
  }
  // Keep open briefly so the client can read, then close.
  setTimeout(() => res.end(), 50);
}

function send(res, code, body) {
  res.writeHead(code, { "content-type": "application/json" });
  res.end(JSON.stringify(body));
}

const server = createServer((req, res) => {
  let body = "";
  req.on("data", (c) => (body += c));
  req.on("end", () => {
    const url = req.url.split("?")[0];

    if (req.method === "GET" && url === "/admin/assignments") {
      return send(res, 200, { tuples: [], principals: [], fgaEnabled: true, warnings: [] });
    }

    if (req.method === "POST" && url === "/agui") {
      // Emit a single AG-UI text-delta + finished event.
      return sseResponse(res, [
        { type: "text-delta",  delta: "Hello from mock gateway" },
        { type: "run-finished", status: "complete" },
      ]);
    }

    if (req.method === "GET" && url === "/stream") {
      // Emit a single audit event.
      return sseResponse(res, [
        {
          event_id:    "mock-event-1",
          event_type:  "task.created",
          decision:    1,
          resource_ref: "task/mock-1",
          actor_user_id: "dev@example.com",
          occurred_at: { seconds: Math.floor(Date.now() / 1000), nanos: 0 },
          context: { tool_id: "web.fetch" },
        },
      ]);
    }

    send(res, 404, { error: `no route ${req.method} ${url}` });
  });
});

server.listen(PORT, () => console.log(`mock gateway on :${PORT}`));
