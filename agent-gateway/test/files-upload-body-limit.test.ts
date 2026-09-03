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
