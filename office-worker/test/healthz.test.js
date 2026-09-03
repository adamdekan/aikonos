import { test } from 'node:test';
import assert from 'node:assert/strict';
import { startServer, get } from '../test-support/helpers.js';

test('GET /healthz returns 200 {ok:true}', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const res = await get(baseUrl, '/healthz');
    assert.equal(res.status, 200);
    assert.deepEqual(JSON.parse(res.body), { ok: true });
  } finally {
    await close();
  }
});
