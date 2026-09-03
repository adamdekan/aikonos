// Checkpoint 3: office.convert. Requires soffice — skips cleanly on a host
// without it (see test-support/helpers.js hasSoffice / API.md).
import { readFile } from 'node:fs/promises';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';
import assert from 'node:assert/strict';
import '../src/handlers/index.js';
import { startServer, post, buildMultipartRequest, parseMultipartResponse, hasSoffice } from '../test-support/helpers.js';

const FIXTURES_DIR = join(dirname(fileURLToPath(import.meta.url)), 'fixtures');
const MAX_OUTPUT = 5_000_000;

test('libreoffice: office.convert converts a legacy .doc to .docx', async (t) => {
  if (!hasSoffice()) {
    t.skip('soffice not installed on this host — see office-doc-tools brief host constraint');
    return;
  }
  const { baseUrl, close } = await startServer();
  try {
    const buffer = await readFile(join(FIXTURES_DIR, 'legacy.doc'));
    const { contentType, body } = buildMultipartRequest(
      [{ name: 'file', filename: 'legacy.doc', buffer }],
      { target_format: 'docx', max_output_bytes: MAX_OUTPUT }
    );
    const res = await post(baseUrl, '/v1/office/convert', contentType, body);
    assert.equal(res.status, 200, res.body.toString());

    const parts = parseMultipartResponse(res.headers['content-type'], res.body);
    const filePart = parts.find((p) => p.name === 'file');
    const result = JSON.parse(parts.find((p) => p.name === 'result').buffer.toString('utf8'));
    assert.ok(filePart.buffer.length > 0);
    // Microsoft Word 2007+ (.docx) is a zip archive — "PK" magic bytes.
    assert.equal(filePart.buffer.subarray(0, 2).toString('latin1'), 'PK');
    assert.equal(result.filename, 'output.docx');
  } finally {
    await close();
  }
});

test('libreoffice: office.convert converts docx to pdf', async (t) => {
  if (!hasSoffice()) {
    t.skip('soffice not installed on this host — see office-doc-tools brief host constraint');
    return;
  }
  const { baseUrl, close } = await startServer();
  try {
    const createScript = `
import { Document, Packer, Paragraph, TextRun } from 'docx';
import { writeFileSync } from 'node:fs';
const doc = new Document({ sections: [{ children: [new Paragraph({ children: [new TextRun('hello')] })] }] });
const buffer = await Packer.toBuffer(doc);
writeFileSync('output.docx', buffer);
`;
    const { contentType: cType, body: cBody } = buildMultipartRequest([], {
      script: createScript,
      max_output_bytes: MAX_OUTPUT,
    });
    const createRes = await post(baseUrl, '/v1/docx/create', cType, cBody);
    assert.equal(createRes.status, 200, createRes.body.toString());
    const createParts = parseMultipartResponse(createRes.headers['content-type'], createRes.body);
    const buffer = createParts.find((p) => p.name === 'file').buffer;

    const { contentType, body } = buildMultipartRequest(
      [{ name: 'file', filename: 'in.docx', buffer }],
      { target_format: 'pdf', max_output_bytes: MAX_OUTPUT }
    );
    const res = await post(baseUrl, '/v1/office/convert', contentType, body);
    assert.equal(res.status, 200, res.body.toString());
    const parts = parseMultipartResponse(res.headers['content-type'], res.body);
    const filePart = parts.find((p) => p.name === 'file');
    assert.equal(filePart.buffer.subarray(0, 4).toString('latin1'), '%PDF');
  } finally {
    await close();
  }
});

test('libreoffice: office.convert on a macro-bearing document succeeds without executing the macro', async (t) => {
  if (!hasSoffice()) {
    t.skip('soffice not installed on this host — see office-doc-tools brief host constraint');
    return;
  }
  const { baseUrl, close } = await startServer();
  try {
    const buffer = await readFile(join(FIXTURES_DIR, 'macro.odt'));
    const { contentType, body } = buildMultipartRequest(
      [{ name: 'file', filename: 'macro.odt', buffer }],
      { target_format: 'txt', max_output_bytes: MAX_OUTPUT }
    );
    const res = await post(baseUrl, '/v1/office/convert', contentType, body);
    assert.equal(res.status, 200, res.body.toString());

    const parts = parseMultipartResponse(res.headers['content-type'], res.body);
    const content = parts.find((p) => p.name === 'file').buffer.toString('utf8');
    // The fixture's embedded "AutoOpen" macro overwrites the body with
    // "MACRO_RAN" if it executes — it must not (see soffice.js / build-macro
    // -odt.py for the verified disposition of this assertion).
    assert.match(content, /ORIGINAL_TEXT_MARKER/);
    assert.doesNotMatch(content, /MACRO_RAN/);
  } finally {
    await close();
  }
});

test('libreoffice: office.convert rejects a missing target_format with 400', async (t) => {
  if (!hasSoffice()) {
    t.skip('soffice not installed on this host — see office-doc-tools brief host constraint');
    return;
  }
  const { baseUrl, close } = await startServer();
  try {
    const buffer = await readFile(join(FIXTURES_DIR, 'legacy.doc'));
    const { contentType, body } = buildMultipartRequest(
      [{ name: 'file', filename: 'legacy.doc', buffer }],
      { max_output_bytes: MAX_OUTPUT }
    );
    const res = await post(baseUrl, '/v1/office/convert', contentType, body);
    assert.equal(res.status, 400, res.body.toString());
    assert.match(JSON.parse(res.body).error, /target_format/);
  } finally {
    await close();
  }
});

test('libreoffice: office.convert rejects a path-escaping target_format with 400', async () => {
  // No hasSoffice() skip — this must 400 at param validation, before soffice
  // is ever invoked, on every host.
  const { baseUrl, close } = await startServer();
  try {
    const buffer = await readFile(join(FIXTURES_DIR, 'legacy.doc'));
    const { contentType, body } = buildMultipartRequest(
      [{ name: 'file', filename: 'legacy.doc', buffer }],
      { target_format: '../../evil', max_output_bytes: MAX_OUTPUT }
    );
    const res = await post(baseUrl, '/v1/office/convert', contentType, body);
    assert.equal(res.status, 400, res.body.toString());
    assert.match(JSON.parse(res.body).error, /target_format/);
  } finally {
    await close();
  }
});

test('libreoffice: office.convert rejects a target_format containing a path separator with 400', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const buffer = await readFile(join(FIXTURES_DIR, 'legacy.doc'));
    const { contentType, body } = buildMultipartRequest(
      [{ name: 'file', filename: 'legacy.doc', buffer }],
      { target_format: 'pd/../f', max_output_bytes: MAX_OUTPUT }
    );
    const res = await post(baseUrl, '/v1/office/convert', contentType, body);
    assert.equal(res.status, 400, res.body.toString());
    assert.match(JSON.parse(res.body).error, /target_format/);
  } finally {
    await close();
  }
});

test('libreoffice: office.convert rejects a path-escaping source_extension with 400', async () => {
  // No hasSoffice() skip — this must 400 at param validation, before soffice
  // is ever invoked, on every host.
  const { baseUrl, close } = await startServer();
  try {
    const buffer = await readFile(join(FIXTURES_DIR, 'legacy.doc'));
    const { contentType, body } = buildMultipartRequest(
      [{ name: 'file', filename: 'legacy', buffer }],
      { target_format: 'pdf', source_extension: '../../evil', max_output_bytes: MAX_OUTPUT }
    );
    const res = await post(baseUrl, '/v1/office/convert', contentType, body);
    assert.equal(res.status, 400, res.body.toString());
    assert.match(JSON.parse(res.body).error, /source_extension/);
  } finally {
    await close();
  }
});

test('libreoffice: office.convert rejects a source_extension containing a path separator with 400', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const buffer = await readFile(join(FIXTURES_DIR, 'legacy.doc'));
    const { contentType, body } = buildMultipartRequest(
      [{ name: 'file', filename: 'legacy', buffer }],
      { target_format: 'pdf', source_extension: 'do/../x', max_output_bytes: MAX_OUTPUT }
    );
    const res = await post(baseUrl, '/v1/office/convert', contentType, body);
    assert.equal(res.status, 400, res.body.toString());
    assert.match(JSON.parse(res.body).error, /source_extension/);
  } finally {
    await close();
  }
});

test('libreoffice: office.convert accepts a normal source_extension and proceeds past validation', async () => {
  // Deliberately no hasSoffice() skip: without soffice this fails at the
  // subprocess spawn (ScriptError, 500) — proof it got past the 400
  // validation gate either way.
  const { baseUrl, close } = await startServer();
  try {
    const buffer = await readFile(join(FIXTURES_DIR, 'legacy.doc'));
    const { contentType, body } = buildMultipartRequest(
      [{ name: 'file', filename: 'legacy', buffer }],
      { target_format: 'pdf', source_extension: 'doc', max_output_bytes: MAX_OUTPUT }
    );
    const res = await post(baseUrl, '/v1/office/convert', contentType, body);
    if (res.status === 400) {
      assert.doesNotMatch(JSON.parse(res.body).error, /source_extension/);
    }
  } finally {
    await close();
  }
});
