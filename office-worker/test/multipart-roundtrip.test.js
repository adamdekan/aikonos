import { test } from 'node:test';
import assert from 'node:assert/strict';
import { startServer, post, buildMultipartRequest, parseMultipartResponse } from '../test-support/helpers.js';
import { registerHandler, unregisterHandler } from '../src/ops.js';

// Pins the wire contract end to end: a stub handler that echoes its input
// file back as an output must round-trip byte-for-byte through the real
// multipart parse (request) + build (response) path — this is what
// checkpoint 4's Go client is written against.
test('multipart round trip: input file bytes echo back through a stub handler', async () => {
  const { baseUrl, close } = await startServer();
  registerHandler('docx/edit', async ({ files, jobDir }) => {
    assert.ok(jobDir, 'handler should receive a job dir');
    const input = files.find((f) => f.fieldname === 'file');
    return {
      outputs: [
        {
          name: 'file',
          filename: 'echoed.docx',
          contentType: 'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
          buffer: input.buffer,
        },
      ],
      result: { path: 'out/echoed.docx', size: input.buffer.length },
    };
  });
  try {
    const inputBytes = Buffer.from('pretend-docx-bytes');
    const { contentType, body } = buildMultipartRequest(
      [{ name: 'file', filename: 'in.docx', buffer: inputBytes }],
      { max_output_bytes: 1_000_000 }
    );
    const res = await post(baseUrl, '/v1/docx/edit', contentType, body);
    assert.equal(res.status, 200);

    const parts = parseMultipartResponse(res.headers['content-type'], res.body);
    const filePart = parts.find((p) => p.name === 'file');
    const resultPart = parts.find((p) => p.name === 'result');

    assert.equal(filePart.filename, 'echoed.docx');
    assert.deepEqual(filePart.buffer, inputBytes);
    assert.deepEqual(JSON.parse(resultPart.buffer.toString('utf8')), {
      path: 'out/echoed.docx',
      size: inputBytes.length,
    });
  } finally {
    unregisterHandler('docx/edit');
    await close();
  }
});

test('an oversize output fails the job with one error naming file/size/cap, no partial parts', async () => {
  const { baseUrl, close } = await startServer();
  registerHandler('docx/edit', async () => ({
    outputs: [{ name: 'file', filename: 'too-big.docx', buffer: Buffer.alloc(50) }],
    result: {},
  }));
  try {
    const { contentType, body } = buildMultipartRequest([], { max_output_bytes: 10 });
    const res = await post(baseUrl, '/v1/docx/edit', contentType, body);
    assert.equal(res.status, 413);
    const parsed = JSON.parse(res.body);
    assert.match(parsed.error, /too-big\.docx/);
    assert.match(parsed.error, /50/);
    assert.match(parsed.error, /10/);
  } finally {
    unregisterHandler('docx/edit');
    await close();
  }
});
