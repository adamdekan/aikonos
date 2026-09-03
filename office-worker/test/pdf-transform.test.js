// Checkpoint 3: pdf.transform (qpdf + pypdf, declarative — no soffice, no
// script execution). qpdf/pypdf are present on every dev host, so these
// tests always run (never skip).
import { execFileSync } from 'node:child_process';
import { writeFileSync, readFileSync, unlinkSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import assert from 'node:assert/strict';
import '../src/handlers/index.js';
import { startServer, post, buildMultipartRequest, parseMultipartResponse } from '../test-support/helpers.js';

const MAX_OUTPUT = 5_000_000;

function tmpPath(name) {
  return join(tmpdir(), `pdf-transform-test-${process.pid}-${name}`);
}

async function createPdf(baseUrl, script) {
  const { contentType, body } = buildMultipartRequest([], { script, max_output_bytes: MAX_OUTPUT });
  const res = await post(baseUrl, '/v1/pdf/create', contentType, body);
  assert.equal(res.status, 200, res.body.toString());
  const parts = parseMultipartResponse(res.headers['content-type'], res.body);
  return parts.find((p) => p.name === 'file').buffer;
}

function onePagePdfScript(label) {
  return `
from reportlab.pdfgen import canvas
c = canvas.Canvas('output.pdf')
c.drawString(100, 700, '${label}')
c.save()
`;
}

function formPdfScript() {
  return `
from reportlab.pdfgen import canvas
c = canvas.Canvas('output.pdf')
c.drawString(100, 750, 'Name:')
form = c.acroForm
form.textfield(name='name_field', tooltip='Name', x=150, y=740, width=200, height=20, borderStyle='inset')
c.save()
`;
}

async function transform(baseUrl, files, params) {
  const { contentType, body } = buildMultipartRequest(files, { ...params, max_output_bytes: MAX_OUTPUT });
  return post(baseUrl, '/v1/pdf/transform', contentType, body);
}

test('pdf.transform "merge" combines multiple PDFs into one, in order', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const page1 = await createPdf(baseUrl, onePagePdfScript('Page 1'));
    const page2 = await createPdf(baseUrl, onePagePdfScript('Page 2'));

    const res = await transform(
      baseUrl,
      [
        { name: 'file', filename: 'p1.pdf', buffer: page1 },
        { name: 'file', filename: 'p2.pdf', buffer: page2 },
      ],
      { op: 'merge' }
    );
    assert.equal(res.status, 200, res.body.toString());
    const parts = parseMultipartResponse(res.headers['content-type'], res.body);
    const merged = parts.find((p) => p.name === 'file').buffer;

    const tmp = tmpPath('merged.pdf');
    writeFileSync(tmp, merged);
    const pages = execFileSync('qpdf', ['--show-npages', tmp]).toString().trim();
    unlinkSync(tmp);
    assert.equal(pages, '2');
  } finally {
    await close();
  }
});

test('pdf.transform "split" extracts a page range into its own file', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const page1 = await createPdf(baseUrl, onePagePdfScript('Page 1'));
    const page2 = await createPdf(baseUrl, onePagePdfScript('Page 2'));
    const mergeRes = await transform(
      baseUrl,
      [
        { name: 'file', filename: 'p1.pdf', buffer: page1 },
        { name: 'file', filename: 'p2.pdf', buffer: page2 },
      ],
      { op: 'merge' }
    );
    const mergeParts = parseMultipartResponse(mergeRes.headers['content-type'], mergeRes.body);
    const merged = mergeParts.find((p) => p.name === 'file').buffer;

    const res = await transform(baseUrl, [{ name: 'file', filename: 'merged.pdf', buffer: merged }], {
      op: 'split',
      ranges: [{ pages: '2', output_filename: 'second.pdf' }],
    });
    assert.equal(res.status, 200, res.body.toString());
    const parts = parseMultipartResponse(res.headers['content-type'], res.body);
    const filePart = parts.find((p) => p.name !== 'result');
    const result = JSON.parse(parts.find((p) => p.name === 'result').buffer.toString('utf8'));
    assert.equal(filePart.filename, 'second.pdf');
    assert.equal(result.files.length, 1);
    assert.equal(result.files[0].filename, 'second.pdf');
  } finally {
    await close();
  }
});

test('pdf.transform "rotate" rotates all pages by the given angle', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const page = await createPdf(baseUrl, onePagePdfScript('Page 1'));
    const res = await transform(baseUrl, [{ name: 'file', filename: 'in.pdf', buffer: page }], {
      op: 'rotate',
      angle: 90,
    });
    assert.equal(res.status, 200, res.body.toString());
    const parts = parseMultipartResponse(res.headers['content-type'], res.body);
    const rotated = parts.find((p) => p.name === 'file').buffer;
    assert.equal(rotated.subarray(0, 4).toString('latin1'), '%PDF');
  } finally {
    await close();
  }
});

test('pdf.transform "rotate" rejects an unsupported angle with 400', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const page = await createPdf(baseUrl, onePagePdfScript('Page 1'));
    const res = await transform(baseUrl, [{ name: 'file', filename: 'in.pdf', buffer: page }], {
      op: 'rotate',
      angle: 45,
    });
    assert.equal(res.status, 400, res.body.toString());
    assert.match(JSON.parse(res.body).error, /angle/);
  } finally {
    await close();
  }
});

test('pdf.transform "decrypt" removes password protection given the correct password', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const page = await createPdf(baseUrl, onePagePdfScript('Secret'));
    const plainPath = tmpPath('decrypt-plain.pdf');
    const encPath = tmpPath('decrypt-enc.pdf');
    writeFileSync(plainPath, page);
    execFileSync('qpdf', ['--encrypt', 'secret', 'secret', '256', '--', plainPath, encPath]);
    const encrypted = readFileSync(encPath);
    unlinkSync(plainPath);
    unlinkSync(encPath);

    const res = await transform(baseUrl, [{ name: 'file', filename: 'enc.pdf', buffer: encrypted }], {
      op: 'decrypt',
      password: 'secret',
    });
    assert.equal(res.status, 200, res.body.toString());
    const parts = parseMultipartResponse(res.headers['content-type'], res.body);
    const decrypted = parts.find((p) => p.name === 'file').buffer;
    const outPath = tmpPath('decrypt-out.pdf');
    writeFileSync(outPath, decrypted);
    // qpdf --check exits 0 on a well-formed, unencrypted file.
    execFileSync('qpdf', ['--check', outPath]);
    unlinkSync(outPath);
  } finally {
    await close();
  }
});

test('pdf.transform "decrypt" fails with 500 given the wrong password', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const page = await createPdf(baseUrl, onePagePdfScript('Secret'));
    const plainPath = tmpPath('decrypt-wrong-plain.pdf');
    const encPath = tmpPath('decrypt-wrong-enc.pdf');
    writeFileSync(plainPath, page);
    execFileSync('qpdf', ['--encrypt', 'secret', 'secret', '256', '--', plainPath, encPath]);
    const encrypted = readFileSync(encPath);
    unlinkSync(plainPath);
    unlinkSync(encPath);

    const res = await transform(baseUrl, [{ name: 'file', filename: 'enc.pdf', buffer: encrypted }], {
      op: 'decrypt',
      password: 'wrong',
    });
    assert.equal(res.status, 500, res.body.toString());
  } finally {
    await close();
  }
});

test('pdf.transform "fill-forms" sets an AcroForm text field value', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const form = await createPdf(baseUrl, formPdfScript());
    const res = await transform(baseUrl, [{ name: 'file', filename: 'form.pdf', buffer: form }], {
      op: 'fill-forms',
      fields: { name_field: 'Ada Lovelace' },
    });
    assert.equal(res.status, 200, res.body.toString());
    const parts = parseMultipartResponse(res.headers['content-type'], res.body);
    const filled = parts.find((p) => p.name === 'file').buffer;
    // The field value is written as a plain (Ada Lovelace) PDF string literal.
    assert.match(filled.toString('latin1'), /Ada Lovelace/);
  } finally {
    await close();
  }
});

test('pdf.transform rejects an unknown op with 400', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const page = await createPdf(baseUrl, onePagePdfScript('Page 1'));
    const res = await transform(baseUrl, [{ name: 'file', filename: 'in.pdf', buffer: page }], {
      op: 'not-a-real-op',
    });
    assert.equal(res.status, 400, res.body.toString());
    assert.match(JSON.parse(res.body).error, /params\.op/);
  } finally {
    await close();
  }
});
