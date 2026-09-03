import { test } from 'node:test';
import assert from 'node:assert/strict';
import { buildMultipartResponse } from '../src/multipart.js';
import { parseMultipartResponse } from '../test-support/helpers.js';

// A handler-supplied filename is attacker-influenced in some ops (e.g. an
// echoed/derived name); unescaped it can break multipart framing or smuggle
// an extra part. Proves the framing survives even a CRLF+quote payload.
test('buildMultipartResponse sanitizes a hostile filename so framing survives', () => {
  const hostileFilename =
    'evil".docx\r\nContent-Disposition: form-data; name="injected"\r\n\r\nHACKED\r\n--x';

  const { contentType, body } = buildMultipartResponse(
    [{ name: 'file', filename: hostileFilename, buffer: Buffer.from('payload') }],
    { ok: true }
  );

  const parts = parseMultipartResponse(contentType, body);
  assert.equal(parts.length, 2, 'exactly the file part + result part, nothing smuggled');

  const filePart = parts.find((p) => p.name === 'file');
  assert.ok(filePart, 'file part must still parse');
  assert.deepEqual(filePart.buffer, Buffer.from('payload'), 'real body bytes must survive intact');
  assert.equal(
    filePart.filename,
    'evil%22.docxContent-Disposition: form-data; name=%22injected%22HACKED--x',
    'CR/LF stripped and quotes percent-encoded'
  );

  const raw = body.toString('latin1');
  assert.ok(!raw.includes('name="injected"'), 'no literal injected header survives');
});
