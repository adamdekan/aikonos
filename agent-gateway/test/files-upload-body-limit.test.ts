// POST /files body-limit route config
//
// WHY: a 10 MiB workspace file becomes ~13.98 MiB base64 once wrapped in the
// JSON envelope. Fastify's app-level default (1 MiB) rejects that with a raw
// 413 before the broker's real 10 MiB workspacefs cap is ever reached. The
// /files upload route must carry its own higher bodyLimit so requests up to
// the broker's cap reach the handler; other /files* routes (tiny bodies)
// must not be widened.
import { test } from "node:test";
import assert from "node:assert/strict";
import Fastify from "fastify";
import { request as httpRequest } from "node:http";
import { registerFilesRoutes } from "../src/routes/files.js";
import { registerSkillsRoutes } from "../src/routes/skills.js";

const FILE_UPLOAD_BODY_LIMIT = 14 * 1024 * 1024;

function buildApp() {
  const app = Fastify({ logger: false });

  app.post<{ Body: { path?: string; contentBase64?: string } }>(
    "/files",
    { bodyLimit: FILE_UPLOAD_BODY_LIMIT },
    async (req, reply) => {
      const b = req.body ?? {};
      reply.send({ receivedBytes: (b.contentBase64 ?? "").length });
    },
  );

  return app;
}

test("POST /files — accepts a ~2 MiB body (above Fastify's 1 MiB default)", async () => {
  const app = buildApp();
  await app.ready();

  const contentBase64 = "a".repeat(2 * 1024 * 1024);
  const res = await app.inject({
    method: "POST",
    url: "/files",
    payload: { path: "big.bin", contentBase64 },
  });

  assert.equal(res.statusCode, 200, `expected 200, got ${res.statusCode}: ${res.body}`);
  const body = JSON.parse(res.body) as { receivedBytes: number };
  assert.equal(body.receivedBytes, contentBase64.length);

  await app.close();
});

test("POST /files — still rejects a body over the route's own limit", async () => {
  const app = buildApp();
  await app.ready();

  const contentBase64 = "a".repeat(FILE_UPLOAD_BODY_LIMIT);
  const res = await app.inject({
    method: "POST",
    url: "/files",
    payload: { path: "toobig.bin", contentBase64 },
  });

  assert.equal(res.statusCode, 413, `expected 413, got ${res.statusCode}`);

  await app.close();
});

// ── F57: authentication before the body is read ───────────────────────────────


// sendHeadersOnly declares a 1 MiB body but sends only a few bytes. A route
// that parses the body before authenticating would wait for the rest and never
// answer; one that authenticates first answers 401 right away.
function sendHeadersOnly(port: number, path: string, authorization?: string): Promise<number> {
  return new Promise((resolve) => {
    const safety = setTimeout(() => resolve(-1), 2000);
    const headers: Record<string, string> = { "content-type": "application/json", "content-length": String(1024 * 1024) };
    if (authorization) headers.authorization = authorization;
    const req = httpRequest({ host: "127.0.0.1", port, path, method: "POST", headers }, (res) => {
      clearTimeout(safety);
      res.resume();
      resolve(res.statusCode ?? 0);
      req.destroy();
    });
    req.on("error", () => {});
    req.write("{");
  });
}

test("F57 POST /files and /skills/import — an unauthenticated upload is refused before its body is read", async () => {
  const app = Fastify({ logger: false });
  const jwksResolver = (() => Promise.reject(new Error("no key"))) as never;
  const verifyOpts = { issuer: "i", audience: "a" };
  registerFilesRoutes(app, { clients: { north: {} as never }, jwksResolver, verifyOpts });
  registerSkillsRoutes(app, { clients: { north: {}, south: {} } as never, jwksResolver, verifyOpts } as never);
  await app.listen({ port: 0, host: "127.0.0.1" });
  const address = app.server.address();
  if (address === null || typeof address === "string") throw new Error("expected a bound TCP address");
  try {
    for (const path of ["/files", "/skills/import"]) {
      assert.equal(await sendHeadersOnly(address.port, path), 401, `${path}: no bearer`);
      assert.equal(await sendHeadersOnly(address.port, path, "Bearer not-a-jwt"), 401, `${path}: bad bearer`);
    }
  } finally {
    app.server.closeAllConnections();
    await app.close();
  }
});
