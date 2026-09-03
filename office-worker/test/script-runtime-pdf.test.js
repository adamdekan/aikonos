import { test } from 'node:test';
import assert from 'node:assert/strict';
import '../src/handlers/index.js';
import { startServer, post, buildMultipartRequest, parseMultipartResponse } from '../test-support/helpers.js';

const MAX_OUTPUT = 5_000_000;

const CREATE_SCRIPT = `
from reportlab.pdfgen import canvas
c = canvas.Canvas('output.pdf')
c.setFont('Helvetica-Bold', 24)
c.drawString(72, 700, 'Hello ScriptRuntime Pdf')
c.save()
`;

async function createPdf(baseUrl, script = CREATE_SCRIPT) {
  const { contentType, body } = buildMultipartRequest([], { script, max_output_bytes: MAX_OUTPUT });
  const res = await post(baseUrl, '/v1/pdf/create', contentType, body);
  assert.equal(res.status, 200, res.body.toString());
  const parts = parseMultipartResponse(res.headers['content-type'], res.body);
  const filePart = parts.find((p) => p.name === 'file');
  const resultPart = parts.find((p) => p.name === 'result');
  return { fileBuffer: filePart.buffer, result: JSON.parse(resultPart.buffer.toString('utf8')) };
}

async function extractPdf(baseUrl, buffer, params = {}) {
  const { contentType, body } = buildMultipartRequest(
    [{ name: 'file', filename: 'in.pdf', buffer }],
    { max_output_bytes: MAX_OUTPUT, ...params }
  );
  return post(baseUrl, '/v1/pdf/extract', contentType, body);
}

test('script-runtime: pdf.create produces a valid .pdf that pdf.extract reads back', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const { fileBuffer, result } = await createPdf(baseUrl);
    assert.ok(fileBuffer.length > 0);
    assert.equal(result.filename, 'output.pdf');
    assert.equal(fileBuffer.toString('latin1', 0, 5), '%PDF-');

    const res = await extractPdf(baseUrl, fileBuffer);
    assert.equal(res.status, 200, res.body.toString());
    const parts = parseMultipartResponse(res.headers['content-type'], res.body);
    const result2 = JSON.parse(parts.find((p) => p.name === 'result').buffer.toString('utf8'));
    const collapsed = result2.text.replace(/\s+/g, ' ').trim();
    assert.match(collapsed, /Hello ScriptRuntime Pdf/);
    assert.equal(result2.method, 'pdfplumber');
  } finally {
    await close();
  }
});

test('script-runtime: pdf.extract forced OCR (ocr:"true") uses the pytesseract fallback path', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const { fileBuffer } = await createPdf(baseUrl);
    const res = await extractPdf(baseUrl, fileBuffer, { ocr: 'true' });
    assert.equal(res.status, 200, res.body.toString());
    const parts = parseMultipartResponse(res.headers['content-type'], res.body);
    const result = JSON.parse(parts.find((p) => p.name === 'result').buffer.toString('utf8'));
    assert.equal(result.method, 'ocr');
    assert.match(result.text, /ScriptRuntime/i);
  } finally {
    await close();
  }
});

test('script-runtime: pdf.create fails with 400 when params.script is missing', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const { contentType, body } = buildMultipartRequest([], { max_output_bytes: MAX_OUTPUT });
    const res = await post(baseUrl, '/v1/pdf/create', contentType, body);
    assert.equal(res.status, 400);
  } finally {
    await close();
  }
});
