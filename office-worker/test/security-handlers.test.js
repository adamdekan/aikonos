// Regression tests for the p6 security fixes inside the real op handlers and
// their fixed python helpers (finding ids in each test name).
import { execFileSync, spawnSync } from 'node:child_process';
import { EventEmitter } from 'node:events';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import assert from 'node:assert/strict';
import '../src/handlers/index.js';
import { HANDLERS } from '../src/ops.js';
import { withJobDir } from '../src/job.js';
import { SCRIPTS_DIR, VENV_PYTHON } from '../src/handlers/run-script.js';
import {
  startServer,
  post,
  buildMultipartRequest,
  parseMultipartResponse,
  hasSoffice,
} from '../test-support/helpers.js';

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

// A one-member zip whose central directory claims 10 bytes (with their CRC)
// for a deflate stream of realSize zeros, streamed so the generator itself
// never holds realSize bytes.
function forgedSizeZip(member, realSize) {
  const script =
    'import io,struct,sys,zipfile,zlib\n' +
    'b=io.BytesIO()\n' +
    'with zipfile.ZipFile(b,"w",zipfile.ZIP_DEFLATED) as z:\n' +
    ' with z.open(sys.argv[1],"w") as f:\n' +
    '  for _ in range(int(sys.argv[2])>>20): f.write(bytes(1<<20).replace(b"\\0",b"0"))\n' +
    'd=bytearray(b.getvalue()); cd=d.rfind(b"PK\\x01\\x02")\n' +
    'struct.pack_into("<II",d,cd+16,zlib.crc32(b"0"*10),struct.unpack_from("<I",d,cd+20)[0])\n' +
    'struct.pack_into("<I",d,cd+24,10)\n' +
    'sys.stdout.buffer.write(bytes(d))';
  return execFileSync(VENV_PYTHON, ['-c', script, member, String(realSize)], { maxBuffer: 64 << 20 });
}

// Runs zip_edit.py directly under a 200 MiB RLIMIT_AS: a helper that holds
// more than its caps in memory dies with MemoryError instead of passing.
function zipEditUnderTightLimit(zip, ops) {
  const dir = mkdtempSync(join(tmpdir(), 'office-worker-test-'));
  try {
    writeFileSync(join(dir, 'in.docx'), zip);
    writeFileSync(join(dir, 'ops.json'), JSON.stringify(ops));
    const args = [join(SCRIPTS_DIR, 'zip_edit.py'), join(dir, 'in.docx'), join(dir, 'ops.json'), join(dir, 'out.docx')];
    return spawnSync('prlimit', [`--as=${200 << 20}`, '--', VENV_PYTHON, ...args], { encoding: 'utf8' });
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
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

test('F82: zip_edit never decompresses past the cap when the header forges a small size', () => {
  // 300 MiB of zeros declared as 10 bytes: zin.read() inflated all of it.
  const zip = forgedSizeZip('word/document.xml', 300 << 20);
  const run = zipEditUnderTightLimit(zip, [{ member: 'word/document.xml', find: '0', replace: '1' }]);
  assert.doesNotMatch(run.stderr, /MemoryError/);
  assert.equal(run.status, 0, run.stderr);
});

test('F82: a regex replace full of group references is refused before it is expanded', () => {
  const zip = zipWithMember('word/document.xml', 1 << 20, 'a');
  const run = zipEditUnderTightLimit(zip, [
    { member: 'word/document.xml', find: '(?s)^.*\\Z', replace: '\\g<0>'.repeat(2000), regex: true },
  ]);
  assert.notEqual(run.status, 0);
  assert.doesNotMatch(run.stderr, /MemoryError/);
  assert.match(run.stderr, /would grow it past/);
});

test('F82: format:"xml" extract refuses context_chars/echo_cap past their bounds', async () => {
  await withServer(async (baseUrl) => {
    const doc = [{ name: 'file', filename: 'a.docx', buffer: zipWithMember('word/document.xml', 1000, 'a') }];
    const base = { format: 'xml', member: 'word/document.xml', pattern: 'a' };
    for (const extra of [{ echo_cap: 1e15 }, { context_chars: 20 << 20 }]) {
      const res = await call(baseUrl, '/v1/docx/extract', doc, { ...base, ...extra });
      assert.equal(res.status, 400, `${JSON.stringify(extra)}: ${res.body}`);
    }
    const ok = await call(baseUrl, '/v1/docx/extract', doc, { ...base, echo_cap: 200000, context_chars: 2000 });
    assert.equal(ok.status, 200, ok.body.toString());
  });
});

test('F82: pandoc, markitdown and the fixed helpers parse caller documents under a memory cap', async () => {
  const seen = [];
  const spawn = (cmd, args) => {
    seen.push([cmd, ...args].join(' '));
    const child = new EventEmitter();
    child.stdout = new EventEmitter();
    child.stderr = new EventEmitter();
    setImmediate(() => child.emit('exit', 0, null));
    return child;
  };
  const files = [{ fieldname: 'file', buffer: Buffer.from('x') }];
  for (const op of ['docx/extract', 'pptx/extract', 'xlsx/extract']) {
    // The fake spawn writes nothing, so each handler then fails reading its output.
    await withJobDir((jobDir) => HANDLERS[op]({ jobDir, files, params: {}, spawn }).catch(() => {}));
  }
  assert.equal(seen.length, 3);
  assert.match(seen[0], /^pandoc \+RTS -M\d+m -RTS /);
  for (const line of seen.slice(1)) assert.match(line, /^prlimit --as=\d+ /, line);
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

// ---------------------------------------------------------------- F88

// A flat ODF spreadsheet that pulls another file's content into its cells in
// every way Calc offers: an external reference, INDIRECT to one, WEBSERVICE
// on a file URL and DDE. soffice recalculates on load (soffice.js), so a
// followed link would land in the converted CSV. LibreOffice 25.2 refuses all
// four in batch mode even without the profile hardening (Err:540, #VALUE!,
// #N/A), so this pins the behaviour rather than reproducing a leak.
function leakyFods(url, sheet, path) {
  const ref = `'${url}'#$${sheet}.A1`;
  return `<?xml version="1.0" encoding="UTF-8"?>
<office:document xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0"
 xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0"
 xmlns:of="urn:oasis:names:tc:opendocument:xmlns:of:1.2"
 office:version="1.2" office:mimetype="application/vnd.oasis.opendocument.spreadsheet">
 <office:body><office:spreadsheet><table:table table:name="Sheet1"><table:table-row>
  <table:table-cell table:formula="of:=[${ref}]"/>
  <table:table-cell table:formula="of:=INDIRECT(&quot;${ref}&quot;)"/>
  <table:table-cell table:formula="of:=COM.MICROSOFT.WEBSERVICE(&quot;${url}&quot;)"/>
  <table:table-cell table:formula="of:=DDE(&quot;soffice&quot;;&quot;${path}&quot;;&quot;A1&quot;)"/>
 </table:table-row></table:table></office:spreadsheet></office:body>
</office:document>`;
}

test('F88: office.convert does not follow a file:// reference out of the document', async (t) => {
  if (!hasSoffice()) {
    t.skip('soffice not installed on this host — see office-doc-tools brief host constraint');
    return;
  }
  // Stands in for another job's input on the shared /tmp.
  const dir = mkdtempSync(join(tmpdir(), 'office-worker-test-'));
  const secret = `leak${Date.now()}`;
  writeFileSync(join(dir, 'secret.csv'), `${secret}\n`);
  try {
    await withServer(async (baseUrl) => {
      const path = join(dir, 'secret.csv');
      const fods = Buffer.from(leakyFods(`file://${path}`, 'secret', path));
      const res = await call(baseUrl, '/v1/office/convert', [{ name: 'file', filename: 'leak.fods', buffer: fods }], {
        target_format: 'csv',
      });
      assert.equal(res.status, 200, res.body.toString());
      const csv = parseMultipartResponse(res.headers['content-type'], res.body).find((p) => p.name === 'file');
      assert.ok(!csv.buffer.toString().includes(secret), `the CSV carries the linked file: ${csv.buffer}`);
    });
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
