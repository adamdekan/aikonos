// Ingress bounds. The broker's 10 MiB
// cap applies to the compressed input only, so without these the decompression
// work a job does is bounded by nothing but the container's mem_limit — and
// every concurrent job shares that one container. Busboy truncates by default,
// so each cap must fail the request loud: a silently truncated document that
// produces a plausible-looking wrong result is worse than an error.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { startServer, post, buildMultipartRequest } from '../test-support/helpers.js';
import { registerHandler, unregisterHandler } from '../src/ops.js';
import { MAX_FIELD_BYTES, MAX_FIELDS } from '../src/multipart.js';
import { runNodeScript, NODE_MAX_OLD_SPACE_MB } from '../src/handlers/run-script.js';

// Registers a spy handler on an op that has no real handler, so a request
// reaching it proves parseMultipart let the body through.
async function withSpyHandler(serverOpts, fn) {
  const seen = { calls: 0, files: null };
  registerHandler('pdf/create', async ({ files }) => {
    seen.calls++;
    seen.files = files;
    return { outputs: [], result: { ok: true } };
  });
  const { baseUrl, close } = await startServer(serverOpts);
  try {
    return await fn({ baseUrl, seen });
  } finally {
    unregisterHandler('pdf/create');
    await close();
  }
}

test('a file part over the upload cap fails with 413, not a truncated success', async () => {
  await withSpyHandler({ maxUploadBytes: 64 }, async ({ baseUrl, seen }) => {
    const { contentType, body } = buildMultipartRequest(
      [{ name: 'file', filename: 'bomb.docx', buffer: Buffer.alloc(4096, 0x41) }],
      {}
    );
    const res = await post(baseUrl, '/v1/pdf/create', contentType, body);

    assert.equal(res.status, 413);
    assert.match(JSON.parse(res.body).error, /bomb\.docx.*64/);
    assert.equal(seen.calls, 0, 'the handler must never see a truncated file');
  });
});

test('more file parts than the files cap fails with 413', async () => {
  await withSpyHandler({ maxUploadFiles: 2 }, async ({ baseUrl, seen }) => {
    const files = ['a.pdf', 'b.pdf', 'c.pdf'].map((filename) => ({
      name: 'file',
      filename,
      buffer: Buffer.from('%PDF-1.4'),
    }));
    const { contentType, body } = buildMultipartRequest(files, { op: 'merge' });
    const res = await post(baseUrl, '/v1/pdf/create', contentType, body);

    assert.equal(res.status, 413);
    assert.match(JSON.parse(res.body).error, /2-file part cap/);
    assert.equal(seen.calls, 0, 'the handler must never see a partial file set');
  });
});

test('a request at the cap is unchanged — every byte and part reaches the handler', async () => {
  await withSpyHandler({ maxUploadBytes: 64, maxUploadFiles: 2 }, async ({ baseUrl, seen }) => {
    const exactly = Buffer.alloc(64, 0x42);
    const { contentType, body } = buildMultipartRequest(
      [
        { name: 'file', filename: 'a.pdf', buffer: exactly },
        { name: 'file', filename: 'b.pdf', buffer: Buffer.from('small') },
      ],
      { op: 'merge' }
    );
    const res = await post(baseUrl, '/v1/pdf/create', contentType, body);

    assert.equal(res.status, 200);
    assert.equal(seen.calls, 1);
    assert.equal(seen.files.length, 2);
    assert.deepEqual(seen.files[0].buffer, exactly, 'a file at the cap arrives whole');
  });
});

// The params field carries a model-authored script, so its cap is a real
// ingress bound, not a formality. Busboy flags truncation at
// fieldSize === limit with zero bytes dropped, so the wired limit must be
// MAX_FIELD_BYTES + 1 for a field of exactly the documented cap to survive.
test('a form field at the field cap is accepted, one byte over fails with 413', async () => {
  const padTo = (bytes) => 'x'.repeat(bytes - '{"blob":""}'.length);

  await withSpyHandler({}, async ({ baseUrl, seen }) => {
    const atCap = buildMultipartRequest([], { blob: padTo(MAX_FIELD_BYTES) });
    const ok = await post(baseUrl, '/v1/pdf/create', atCap.contentType, atCap.body);
    assert.equal(ok.status, 200, 'a field of exactly MAX_FIELD_BYTES must not be rejected');
    assert.equal(seen.calls, 1);

    const overCap = buildMultipartRequest([], { blob: padTo(MAX_FIELD_BYTES + 1) });
    const res = await post(baseUrl, '/v1/pdf/create', overCap.contentType, overCap.body);
    assert.equal(res.status, 413);
    assert.match(JSON.parse(res.body).error, new RegExp(`${MAX_FIELD_BYTES}-byte field cap`));
    assert.equal(seen.calls, 1, 'the handler must never see a truncated params field');
  });
});

test('more form fields than the fields cap fails with 413', async () => {
  await withSpyHandler({}, async ({ baseUrl, seen }) => {
    // "params" itself is one field, so MAX_FIELDS extras puts us one over.
    const extra = Object.fromEntries(
      Array.from({ length: MAX_FIELDS }, (_, i) => [`pad${i}`, 'v'])
    );
    const { contentType, body } = buildMultipartRequest([], {}, extra);
    const res = await post(baseUrl, '/v1/pdf/create', contentType, body);

    assert.equal(res.status, 413);
    assert.match(JSON.parse(res.body).error, new RegExp(`${MAX_FIELDS}-form field cap`));
    assert.equal(seen.calls, 0, 'the handler must never see a partial field set');
  });
});

test('runNodeScript spawns node with the per-job heap ceiling ahead of the script path', async () => {
  const jobDir = await mkdtemp(join(tmpdir(), 'office-worker-test-'));
  const received = [];
  const spawn = (cmd, args, opts) => {
    received.push({ cmd, args, opts });
    const child = new EventEmitter();
    child.stdout = new EventEmitter();
    child.stderr = new EventEmitter();
    setImmediate(() => child.emit('exit', 0, null));
    return child;
  };

  try {
    await runNodeScript({ jobDir, spawn, script: 'console.log(1)', args: ['out.docx'] });

    assert.equal(received.length, 1);
    const { args } = received[0];
    assert.equal(
      args[0],
      `--max-old-space-size=${NODE_MAX_OLD_SPACE_MB}`,
      'the V8 flag must precede the script path or node treats it as a script argument'
    );
    assert.equal(args[1], join(jobDir, 'script.cjs'));
    assert.deepEqual(args.slice(2), ['out.docx'], 'caller args still follow the script path');
  } finally {
    await rm(jobDir, { recursive: true, force: true });
  }
});
