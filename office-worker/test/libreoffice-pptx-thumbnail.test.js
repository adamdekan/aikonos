// Checkpoint 3: pptx.thumbnail. Requires soffice — skips cleanly on a host
// without it (see test-support/helpers.js hasSoffice / API.md).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import '../src/handlers/index.js';
import { startServer, post, buildMultipartRequest, parseMultipartResponse, hasSoffice } from '../test-support/helpers.js';

const MAX_OUTPUT = 5_000_000;

// Builds a pptx with `count` slides via the (soffice-free) pptx.create op —
// no committed multi-slide fixture needed.
async function createSlides(baseUrl, count) {
  const script = `
import PptxGenJS from 'pptxgenjs';
const pptx = new PptxGenJS();
for (let i = 1; i <= ${count}; i++) {
  const slide = pptx.addSlide();
  slide.addText('Slide ' + i, { x: 1, y: 1, w: 4, h: 1, fontSize: 24 });
}
await pptx.writeFile({ fileName: 'output.pptx' });
`;
  const { contentType, body } = buildMultipartRequest([], { script, max_output_bytes: MAX_OUTPUT });
  const res = await post(baseUrl, '/v1/pptx/create', contentType, body);
  assert.equal(res.status, 200, res.body.toString());
  const parts = parseMultipartResponse(res.headers['content-type'], res.body);
  return parts.find((p) => p.name === 'file').buffer;
}

test('libreoffice: pptx.thumbnail renders one JPEG per slide under the default cap', async (t) => {
  if (!hasSoffice()) {
    t.skip('soffice not installed on this host — see office-doc-tools brief host constraint');
    return;
  }
  const { baseUrl, close } = await startServer();
  try {
    const buffer = await createSlides(baseUrl, 4);
    const { contentType, body } = buildMultipartRequest(
      [{ name: 'file', filename: 'in.pptx', buffer }],
      { max_output_bytes: MAX_OUTPUT }
    );
    const res = await post(baseUrl, '/v1/pptx/thumbnail', contentType, body);
    assert.equal(res.status, 200, res.body.toString());

    const parts = parseMultipartResponse(res.headers['content-type'], res.body);
    const imageParts = parts.filter((p) => p.name !== 'result');
    const result = JSON.parse(parts.find((p) => p.name === 'result').buffer.toString('utf8'));

    assert.equal(imageParts.length, 4);
    assert.equal(result.count, 4);
    assert.equal(result.skipped, 0);
    assert.equal(result.total_pages, 4);
    assert.equal(result.dpi, 150);
    for (const part of imageParts) assert.ok(part.buffer.length > 0);
  } finally {
    await close();
  }
});

test('libreoffice: pptx.thumbnail caps at 20 pages by default, skipping the rest', async (t) => {
  if (!hasSoffice()) {
    t.skip('soffice not installed on this host — see office-doc-tools brief host constraint');
    return;
  }
  const { baseUrl, close } = await startServer();
  try {
    const buffer = await createSlides(baseUrl, 25);
    const { contentType, body } = buildMultipartRequest(
      [{ name: 'file', filename: 'in.pptx', buffer }],
      { max_output_bytes: MAX_OUTPUT }
    );
    const res = await post(baseUrl, '/v1/pptx/thumbnail', contentType, body);
    assert.equal(res.status, 200, res.body.toString());

    const parts = parseMultipartResponse(res.headers['content-type'], res.body);
    const imageParts = parts.filter((p) => p.name !== 'result');
    const result = JSON.parse(parts.find((p) => p.name === 'result').buffer.toString('utf8'));

    assert.equal(imageParts.length, 20);
    assert.equal(result.count, 20);
    assert.equal(result.skipped, 5);
    assert.equal(result.total_pages, 25);
  } finally {
    await close();
  }
});

test('libreoffice: pptx.thumbnail accepts a custom dpi param', async (t) => {
  if (!hasSoffice()) {
    t.skip('soffice not installed on this host — see office-doc-tools brief host constraint');
    return;
  }
  const { baseUrl, close } = await startServer();
  try {
    const buffer = await createSlides(baseUrl, 1);
    const { contentType, body } = buildMultipartRequest(
      [{ name: 'file', filename: 'in.pptx', buffer }],
      { max_output_bytes: MAX_OUTPUT, dpi: 72 }
    );
    const res = await post(baseUrl, '/v1/pptx/thumbnail', contentType, body);
    assert.equal(res.status, 200, res.body.toString());
    const parts = parseMultipartResponse(res.headers['content-type'], res.body);
    const result = JSON.parse(parts.find((p) => p.name === 'result').buffer.toString('utf8'));
    assert.equal(result.dpi, 72);
  } finally {
    await close();
  }
});
