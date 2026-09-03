import { test } from 'node:test';
import assert from 'node:assert/strict';
import '../src/handlers/index.js';
import { startServer, post, buildMultipartRequest, parseMultipartResponse } from '../test-support/helpers.js';

const MAX_OUTPUT = 5_000_000;

const CREATE_SCRIPT = `
const { Document, Packer, Paragraph, TextRun } = require('docx');
const { writeFileSync } = require('node:fs');
(async () => {
  const doc = new Document({ sections: [{ children: [new Paragraph({ children: [new TextRun('Hello ScriptRuntime Docx')] })] }] });
  const buffer = await Packer.toBuffer(doc);
  writeFileSync('output.docx', buffer);
})();
`;

async function createDocx(baseUrl, script = CREATE_SCRIPT) {
  const { contentType, body } = buildMultipartRequest([], { script, max_output_bytes: MAX_OUTPUT });
  const res = await post(baseUrl, '/v1/docx/create', contentType, body);
  assert.equal(res.status, 200, res.body.toString());
  const parts = parseMultipartResponse(res.headers['content-type'], res.body);
  const filePart = parts.find((p) => p.name === 'file');
  const resultPart = parts.find((p) => p.name === 'result');
  return { fileBuffer: filePart.buffer, result: JSON.parse(resultPart.buffer.toString('utf8')) };
}

async function extractDocx(baseUrl, buffer, params = {}) {
  const { contentType, body } = buildMultipartRequest(
    [{ name: 'file', filename: 'in.docx', buffer }],
    { max_output_bytes: MAX_OUTPUT, ...params }
  );
  const res = await post(baseUrl, '/v1/docx/extract', contentType, body);
  return res;
}

test('script-runtime: docx.create produces a valid .docx that docx.extract reads back', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const { fileBuffer, result } = await createDocx(baseUrl);
    assert.ok(fileBuffer.length > 0, 'created docx should be non-empty');
    assert.equal(result.filename, 'output.docx');

    const res = await extractDocx(baseUrl, fileBuffer);
    assert.equal(res.status, 200, res.body.toString());
    const parts = parseMultipartResponse(res.headers['content-type'], res.body);
    const resultPart = JSON.parse(parts.find((p) => p.name === 'result').buffer.toString('utf8'));
    assert.match(resultPart.content, /Hello ScriptRuntime Docx/);
  } finally {
    await close();
  }
});

test('script-runtime: docx.create rejects a path-escaping output_filename with 400', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const { contentType, body } = buildMultipartRequest([], {
      script: CREATE_SCRIPT,
      output_filename: '../../x',
      max_output_bytes: MAX_OUTPUT,
    });
    const res = await post(baseUrl, '/v1/docx/create', contentType, body);
    assert.equal(res.status, 400, res.body.toString());
    assert.match(JSON.parse(res.body).error, /output_filename/);
  } finally {
    await close();
  }
});

test('script-runtime: docx.create rejects an absolute output_filename with 400', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const { contentType, body } = buildMultipartRequest([], {
      script: CREATE_SCRIPT,
      output_filename: '/etc/hostname',
      max_output_bytes: MAX_OUTPUT,
    });
    const res = await post(baseUrl, '/v1/docx/create', contentType, body);
    assert.equal(res.status, 400, res.body.toString());
    assert.match(JSON.parse(res.body).error, /output_filename/);
  } finally {
    await close();
  }
});

test('script-runtime: docx.create still accepts a benign nested output_filename', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const nestedScript = `
const { Document, Packer, Paragraph, TextRun } = require('docx');
const { writeFileSync, mkdirSync } = require('node:fs');
(async () => {
  const doc = new Document({ sections: [{ children: [new Paragraph({ children: [new TextRun('Nested')] })] }] });
  const buffer = await Packer.toBuffer(doc);
  mkdirSync('out', { recursive: true });
  writeFileSync('out/result.docx', buffer);
})();
`;
    const { contentType, body } = buildMultipartRequest([], {
      script: nestedScript,
      output_filename: 'out/result.docx',
      max_output_bytes: MAX_OUTPUT,
    });
    const res = await post(baseUrl, '/v1/docx/create', contentType, body);
    assert.equal(res.status, 200, res.body.toString());
    const parts = parseMultipartResponse(res.headers['content-type'], res.body);
    const filePart = parts.find((p) => p.name === 'file');
    assert.ok(filePart.buffer.length > 0);
  } finally {
    await close();
  }
});

test('script-runtime: docx.create fails with 400 when params.script is missing', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const { contentType, body } = buildMultipartRequest([], { max_output_bytes: MAX_OUTPUT });
    const res = await post(baseUrl, '/v1/docx/create', contentType, body);
    assert.equal(res.status, 400);
    assert.match(JSON.parse(res.body).error, /script/);
  } finally {
    await close();
  }
});

test('script-runtime: docx.edit changes text visible in re-extract', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const { fileBuffer } = await createDocx(baseUrl);

    const ops = [{ member: 'word/document.xml', find: 'Hello ScriptRuntime Docx', replace: 'Edited By ScriptRuntime' }];
    const { contentType, body } = buildMultipartRequest(
      [{ name: 'file', filename: 'in.docx', buffer: fileBuffer }],
      { ops, output_filename: 'edited.docx', max_output_bytes: MAX_OUTPUT }
    );
    const editRes = await post(baseUrl, '/v1/docx/edit', contentType, body);
    assert.equal(editRes.status, 200, editRes.body.toString());
    const editParts = parseMultipartResponse(editRes.headers['content-type'], editRes.body);
    const editedBuffer = editParts.find((p) => p.name === 'file').buffer;
    assert.equal(editParts.find((p) => p.name === 'file').filename, 'edited.docx');

    const extractRes = await extractDocx(baseUrl, editedBuffer);
    const extractParts = parseMultipartResponse(extractRes.headers['content-type'], extractRes.body);
    const result = JSON.parse(extractParts.find((p) => p.name === 'result').buffer.toString('utf8'));
    assert.match(result.content, /Edited By ScriptRuntime/);
    assert.doesNotMatch(result.content, /Hello ScriptRuntime Docx/);
  } finally {
    await close();
  }
});

test('script-runtime: docx.extract format:"xml" mode returns member content with pattern context and respects the echo cap', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const { fileBuffer } = await createDocx(baseUrl);

    const patternRes = await extractDocx(baseUrl, fileBuffer, {
      format: 'xml',
      member: 'word/document.xml',
      pattern: 'Hello ScriptRuntime Docx',
      context_chars: 20,
    });
    assert.equal(patternRes.status, 200, patternRes.body.toString());
    const patternParts = parseMultipartResponse(patternRes.headers['content-type'], patternRes.body);
    const patternResult = JSON.parse(patternParts.find((p) => p.name === 'result').buffer.toString('utf8'));
    assert.equal(patternResult.member, 'word/document.xml');
    assert.equal(patternResult.matches.length, 1);
    assert.match(patternResult.matches[0].text, /Hello ScriptRuntime Docx/);

    const cappedRes = await extractDocx(baseUrl, fileBuffer, {
      format: 'xml',
      member: 'word/document.xml',
      echo_cap: 50,
    });
    const cappedParts = parseMultipartResponse(cappedRes.headers['content-type'], cappedRes.body);
    const cappedResult = JSON.parse(cappedParts.find((p) => p.name === 'result').buffer.toString('utf8'));
    assert.ok(cappedResult.content.length <= 50, 'content must respect the echo cap');
    assert.equal(cappedResult.truncated, true);
  } finally {
    await close();
  }
});

test('script-runtime: docx.extract requires a "file" part', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const { contentType, body } = buildMultipartRequest([], { max_output_bytes: MAX_OUTPUT });
    const res = await post(baseUrl, '/v1/docx/extract', contentType, body);
    assert.equal(res.status, 400);
  } finally {
    await close();
  }
});
