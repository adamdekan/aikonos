import { test } from 'node:test';
import assert from 'node:assert/strict';
import { startServer, post, buildMultipartRequest } from '../test-support/helpers.js';
import { KNOWN_OPS, registerHandler, unregisterHandler } from '../src/ops.js';

test('known op with no registered handler returns 501', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const { contentType, body } = buildMultipartRequest([], {});
    const res = await post(baseUrl, '/v1/docx/create', contentType, body);
    assert.equal(res.status, 501);
    assert.match(JSON.parse(res.body).error, /docx\/create.*not implemented/);
  } finally {
    await close();
  }
});

test('every known op is reachable and not-yet-implemented', async () => {
  const { baseUrl, close } = await startServer();
  try {
    for (const opKey of KNOWN_OPS) {
      const { contentType, body } = buildMultipartRequest([], {});
      const res = await post(baseUrl, `/v1/${opKey}`, contentType, body);
      assert.equal(res.status, 501, `${opKey} expected 501, got ${res.status}`);
    }
  } finally {
    await close();
  }
});

test('unknown format/op returns 404', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const { contentType, body } = buildMultipartRequest([], {});
    const res = await post(baseUrl, '/v1/docx/frobnicate', contentType, body);
    assert.equal(res.status, 404);
    assert.match(JSON.parse(res.body).error, /unknown operation/);
  } finally {
    await close();
  }
});

test('GET on a known op route is not a valid route (404)', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const http = await import('node:http');
    const res = await new Promise((resolve, reject) => {
      http
        .get(new URL('/v1/docx/create', baseUrl), (r) => {
          const chunks = [];
          r.on('data', (c) => chunks.push(c));
          r.on('end', () => resolve({ status: r.statusCode, body: Buffer.concat(chunks) }));
        })
        .on('error', reject);
    });
    assert.equal(res.status, 404);
  } finally {
    await close();
  }
});

test('a registered handler is invoked instead of returning 501', async () => {
  const { baseUrl, close } = await startServer();
  registerHandler('pdf/create', async () => ({ outputs: [], result: { ok: true } }));
  try {
    const { contentType, body } = buildMultipartRequest([], {});
    const res = await post(baseUrl, '/v1/pdf/create', contentType, body);
    assert.equal(res.status, 200);
  } finally {
    unregisterHandler('pdf/create');
    await close();
  }
});
