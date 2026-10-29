// Regression tests for the p6 security fixes inside the real op handlers and
// their fixed python helpers (finding ids in each test name).
import { execFileSync } from 'node:child_process';
import { test } from 'node:test';
import assert from 'node:assert/strict';
import '../src/handlers/index.js';
import { VENV_PYTHON } from '../src/handlers/run-script.js';
import { startServer, post, buildMultipartRequest, parseMultipartResponse } from '../test-support/helpers.js';

const MAX_OUTPUT = 5_000_000;

async function call(baseUrl, route, files, params) {
  const { contentType, body } = buildMultipartRequest(files, { max_output_bytes: MAX_OUTPUT, ...params });
  return post(baseUrl, route, contentType, body);
}

async function withServer(fn) {
  const { baseUrl, close } = await startServer();
  try {
    return await fn(baseUrl);
  } finally {
    await close();
  }
}

// A one-member zip built by python's zipfile: the member's declared size is
// what the caps check, and 51 MiB of zeros deflates to ~50 KiB.
function zipWithMember(member, size, fill = '0') {
  const script =
    'import io,sys,zipfile\n' +
    'b=io.BytesIO()\n' +
    'with zipfile.ZipFile(b,"w",zipfile.ZIP_DEFLATED) as z: z.writestr(sys.argv[1], sys.argv[3]*int(sys.argv[2]))\n' +
    'sys.stdout.buffer.write(b.getvalue())';
  return execFileSync(VENV_PYTHON, ['-c', script, member, String(size), fill], { maxBuffer: 64 << 20 });
}

const PDF_SCRIPT = `
from reportlab.pdfgen import canvas
c = canvas.Canvas('output.pdf')
c.drawString(100, 700, 'p1')
c.showPage()
c.drawString(100, 700, 'p2')
c.save()
`;

// ---------------------------------------------------------------- F80 / F87

test('F80: pdf.transform split refuses an @argfile or option-shaped page range with 400', async () => {
  await withServer(async (baseUrl) => {
    const created = await call(baseUrl, '/v1/pdf/create', [], { script: PDF_SCRIPT });
    assert.equal(created.status, 200, created.body.toString());
    const pdf = parseMultipartResponse(created.headers['content-type'], created.body).find((p) => p.name === 'file');
    const file = [{ name: 'file', filename: 'in.pdf', buffer: pdf.buffer }];

    for (const pages of ['@input.pdf', '--add-attachment', '-1', '1,@x', '1-2 --x', 'a'.repeat(300)]) {
      const res = await call(baseUrl, '/v1/pdf/transform', file, { op: 'split', ranges: [{ pages }] });
      assert.equal(res.status, 400, `split pages ${JSON.stringify(pages)}: ${res.body}`);
    }
    const rotate = await call(baseUrl, '/v1/pdf/transform', file, { op: 'rotate', angle: 90, pages: '1,@x' });
    assert.equal(rotate.status, 400);

    // The documented qpdf forms still work.
    const ok = await call(baseUrl, '/v1/pdf/transform', file, {
      op: 'split',
      ranges: [{ pages: 'r1' }, { pages: '1-z:odd' }, { pages: '1,2' }],
    });
    assert.equal(ok.status, 200, ok.body.toString());
  });
});

test('F87: docx.extract refuses a track_changes value outside its allowlist', async () => {
  await withServer(async (baseUrl) => {
    const file = [{ name: 'file', filename: 'a.docx', buffer: Buffer.from('x') }];
    const res = await call(baseUrl, '/v1/docx/extract', file, { track_changes: 'all --lua-filter=/tmp/x.lua' });
    assert.equal(res.status, 400);
  });
});

test('F87: docx.edit refuses more than 200 ops and an over-long regex', async () => {
  await withServer(async (baseUrl) => {
    const file = [{ name: 'file', filename: 'a.docx', buffer: Buffer.from('x') }];
    const op = { member: 'word/document.xml', find: 'a', replace: 'b' };
    const many = await call(baseUrl, '/v1/docx/edit', file, { ops: Array(201).fill(op) });
    assert.equal(many.status, 400);
    const longRe = await call(baseUrl, '/v1/docx/edit', file, {
      ops: [{ ...op, regex: true, find: 'a'.repeat(1025) }],
    });
    assert.equal(longRe.status, 400);
  });
});

// ---------------------------------------------------------------- F82 (M-32)

test('F82: docx.edit and format:"xml" extract refuse a member over the decompressed cap', async () => {
  await withServer(async (baseUrl) => {
    const bomb = [{ name: 'file', filename: 'bomb.docx', buffer: zipWithMember('word/document.xml', 51 << 20) }];
    const edit = await call(baseUrl, '/v1/docx/edit', bomb, {
      ops: [{ member: 'word/document.xml', find: '0', replace: '1' }],
    });
    assert.equal(edit.status, 500);
    assert.match(JSON.parse(edit.body).error, /over the \d+-byte cap/);

    const xml = await call(baseUrl, '/v1/docx/extract', bomb, { format: 'xml', member: 'word/document.xml' });
    assert.equal(xml.status, 500);
    assert.match(JSON.parse(xml.body).error, /over the \d+-byte cap/);
  });
});

test('F82: docx.edit refuses an edit that would grow a member past the output cap', async () => {
  await withServer(async (baseUrl) => {
    const doc = [{ name: 'file', filename: 'a.docx', buffer: zipWithMember('word/document.xml', 1 << 20, 'a') }];
    const big = 'x'.repeat(1 << 20);
    // An empty-match regex inserts `replace` between every character.
    const regex = await call(baseUrl, '/v1/docx/edit', doc, {
      ops: [{ member: 'word/document.xml', find: '', replace: big, regex: true }],
    });
    assert.equal(regex.status, 500);
    assert.match(JSON.parse(regex.body).error, /would grow it past/);
    const plain = await call(baseUrl, '/v1/docx/edit', doc, {
      ops: [{ member: 'word/document.xml', find: 'a', replace: big }],
    });
    assert.equal(plain.status, 500);
    assert.match(JSON.parse(plain.body).error, /would grow it past/);
  });
});

test('F82: xlsx.extract clamps a whole-sheet range to the used area', async () => {
  await withServer(async (baseUrl) => {
    const created = await call(baseUrl, '/v1/xlsx/create', [], {
      script: "import openpyxl\nwb=openpyxl.Workbook()\nwb.active['B2']=1\nwb.save('output.xlsx')\n",
    });
    assert.equal(created.status, 200, created.body.toString());
    const xlsx = parseMultipartResponse(created.headers['content-type'], created.body).find((p) => p.name === 'file');
    const res = await call(baseUrl, '/v1/xlsx/extract', [{ name: 'file', filename: 'a.xlsx', buffer: xlsx.buffer }], {
      range: 'A1:XFD1048576',
      format: 'json',
    });
    assert.equal(res.status, 200, res.body.toString());
    const parts = parseMultipartResponse(res.headers['content-type'], res.body);
    const result = JSON.parse(parts.find((p) => p.name === 'result').buffer);
    assert.equal(result.rows, 2);
  });
});

test('F82: pptx.thumbnail refuses a non-integer dpi/max_pages before any rendering', async () => {
  await withServer(async (baseUrl) => {
    const file = [{ name: 'file', filename: 'a.pptx', buffer: Buffer.from('x') }];
    assert.equal((await call(baseUrl, '/v1/pptx/thumbnail', file, { dpi: '200000' })).status, 400);
    assert.equal((await call(baseUrl, '/v1/pptx/thumbnail', file, { max_pages: 1.5 })).status, 400);
  });
});

// ---------------------------------------------------------------- F217

test('F217: a model script allocating past the address-space limit fails inside the job', async () => {
  await withServer(async (baseUrl) => {
    const py = await call(baseUrl, '/v1/xlsx/create', [], { script: 'b = bytearray(3 << 30)\n' });
    assert.equal(py.status, 500);
    assert.match(JSON.parse(py.body).error, /MemoryError/);

    const node = await call(baseUrl, '/v1/docx/create', [], { script: 'Buffer.alloc(3e9, 1)' });
    assert.equal(node.status, 500);
    assert.match(JSON.parse(node.body).error, /RangeError|allocation failed|Array buffer/i);
  });
});
