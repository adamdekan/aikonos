import { test } from 'node:test';
import assert from 'node:assert/strict';
import '../src/handlers/index.js';
import { startServer, post, buildMultipartRequest, parseMultipartResponse } from '../test-support/helpers.js';

const MAX_OUTPUT = 5_000_000;

const CREATE_SCRIPT = `
const pptxgen = require('pptxgenjs');
(async () => {
  const pres = new pptxgen();
  const slide = pres.addSlide();
  slide.addText('Hello ScriptRuntime Pptx', { x: 1, y: 1, w: 5, h: 1 });
  await pres.writeFile({ fileName: 'output.pptx' });
})();
`;

async function createPptx(baseUrl, script = CREATE_SCRIPT) {
  const { contentType, body } = buildMultipartRequest([], { script, max_output_bytes: MAX_OUTPUT });
  const res = await post(baseUrl, '/v1/pptx/create', contentType, body);
  assert.equal(res.status, 200, res.body.toString());
  const parts = parseMultipartResponse(res.headers['content-type'], res.body);
  const filePart = parts.find((p) => p.name === 'file');
  const resultPart = parts.find((p) => p.name === 'result');
  return { fileBuffer: filePart.buffer, result: JSON.parse(resultPart.buffer.toString('utf8')) };
}

async function extractPptx(baseUrl, buffer, params = {}) {
  const { contentType, body } = buildMultipartRequest(
    [{ name: 'file', filename: 'in.pptx', buffer }],
    { max_output_bytes: MAX_OUTPUT, ...params }
  );
  return post(baseUrl, '/v1/pptx/extract', contentType, body);
}

test('script-runtime: pptx.create produces a valid .pptx that pptx.extract reads back', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const { fileBuffer, result } = await createPptx(baseUrl);
    assert.ok(fileBuffer.length > 0);
    assert.equal(result.filename, 'output.pptx');

    const res = await extractPptx(baseUrl, fileBuffer);
    assert.equal(res.status, 200, res.body.toString());
    const parts = parseMultipartResponse(res.headers['content-type'], res.body);
    const resultPart = JSON.parse(parts.find((p) => p.name === 'result').buffer.toString('utf8'));
    assert.match(resultPart.content, /Hello ScriptRuntime Pptx/);
  } finally {
    await close();
  }
});

test('script-runtime: pptx.edit changes text visible in re-extract', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const { fileBuffer } = await createPptx(baseUrl);

    const ops = [{ member: 'ppt/slides/slide1.xml', find: 'Hello ScriptRuntime Pptx', replace: 'Edited Pptx Slide' }];
    const { contentType, body } = buildMultipartRequest(
      [{ name: 'file', filename: 'in.pptx', buffer: fileBuffer }],
      { ops, output_filename: 'edited.pptx', max_output_bytes: MAX_OUTPUT }
    );
    const editRes = await post(baseUrl, '/v1/pptx/edit', contentType, body);
    assert.equal(editRes.status, 200, editRes.body.toString());
    const editParts = parseMultipartResponse(editRes.headers['content-type'], editRes.body);
    const editedBuffer = editParts.find((p) => p.name === 'file').buffer;

    const extractRes = await extractPptx(baseUrl, editedBuffer);
    const extractParts = parseMultipartResponse(extractRes.headers['content-type'], extractRes.body);
    const result = JSON.parse(extractParts.find((p) => p.name === 'result').buffer.toString('utf8'));
    assert.match(result.content, /Edited Pptx Slide/);
    assert.doesNotMatch(result.content, /Hello ScriptRuntime Pptx/);
  } finally {
    await close();
  }
});

test('script-runtime: pptx.extract format:"xml" mode returns member content with pattern context and respects the echo cap', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const { fileBuffer } = await createPptx(baseUrl);

    const patternRes = await extractPptx(baseUrl, fileBuffer, {
      format: 'xml',
      member: 'ppt/slides/slide1.xml',
      pattern: 'Hello ScriptRuntime Pptx',
      context_chars: 20,
    });
    assert.equal(patternRes.status, 200, patternRes.body.toString());
    const patternParts = parseMultipartResponse(patternRes.headers['content-type'], patternRes.body);
    const patternResult = JSON.parse(patternParts.find((p) => p.name === 'result').buffer.toString('utf8'));
    assert.equal(patternResult.matches.length, 1);
    assert.match(patternResult.matches[0].text, /Hello ScriptRuntime Pptx/);

    const cappedRes = await extractPptx(baseUrl, fileBuffer, {
      format: 'xml',
      member: 'ppt/slides/slide1.xml',
      echo_cap: 50,
    });
    const cappedParts = parseMultipartResponse(cappedRes.headers['content-type'], cappedRes.body);
    const cappedResult = JSON.parse(cappedParts.find((p) => p.name === 'result').buffer.toString('utf8'));
    assert.ok(cappedResult.content.length <= 50);
    assert.equal(cappedResult.truncated, true);
  } finally {
    await close();
  }
});
