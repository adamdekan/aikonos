// Checkpoint 3: xlsx.recalc. Requires soffice — skips cleanly on a host
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

test('libreoffice: xlsx.recalc reports #DIV/0! and #NAME? errors and recalculates a valid formula', async (t) => {
  if (!hasSoffice()) {
    t.skip('soffice not installed on this host — see office-doc-tools brief host constraint');
    return;
  }
  const { baseUrl, close } = await startServer();
  try {
    const buffer = await readFile(join(FIXTURES_DIR, 'broken-formula.xlsx'));
    const { contentType, body } = buildMultipartRequest(
      [{ name: 'file', filename: 'broken-formula.xlsx', buffer }],
      { max_output_bytes: MAX_OUTPUT }
    );
    const res = await post(baseUrl, '/v1/xlsx/recalc', contentType, body);
    assert.equal(res.status, 200, res.body.toString());

    const parts = parseMultipartResponse(res.headers['content-type'], res.body);
    const filePart = parts.find((p) => p.name === 'file');
    const result = JSON.parse(parts.find((p) => p.name === 'result').buffer.toString('utf8'));

    assert.ok(filePart.buffer.length > 0);
    assert.equal(result.error_count, 2);
    const byCell = Object.fromEntries(result.errors.map((e) => [e.cell, e.error]));
    assert.equal(byCell.A3, '#DIV/0!');
    assert.equal(byCell.A5, '#NAME?');
  } finally {
    await close();
  }
});

test('libreoffice: xlsx.recalc on an all-valid workbook reports zero errors', async (t) => {
  if (!hasSoffice()) {
    t.skip('soffice not installed on this host — see office-doc-tools brief host constraint');
    return;
  }
  const { baseUrl, close } = await startServer();
  try {
    const createScript = `
import openpyxl
wb = openpyxl.Workbook()
ws = wb.active
ws['A1'] = 10
ws['A2'] = '=A1+5'
wb.save('output.xlsx')
`;
    const { contentType: cType, body: cBody } = buildMultipartRequest([], {
      script: createScript,
      max_output_bytes: MAX_OUTPUT,
    });
    const createRes = await post(baseUrl, '/v1/xlsx/create', cType, cBody);
    assert.equal(createRes.status, 200, createRes.body.toString());
    const createParts = parseMultipartResponse(createRes.headers['content-type'], createRes.body);
    const buffer = createParts.find((p) => p.name === 'file').buffer;

    const { contentType, body } = buildMultipartRequest(
      [{ name: 'file', filename: 'valid.xlsx', buffer }],
      { max_output_bytes: MAX_OUTPUT }
    );
    const res = await post(baseUrl, '/v1/xlsx/recalc', contentType, body);
    assert.equal(res.status, 200, res.body.toString());
    const parts = parseMultipartResponse(res.headers['content-type'], res.body);
    const result = JSON.parse(parts.find((p) => p.name === 'result').buffer.toString('utf8'));
    assert.equal(result.error_count, 0);
    assert.deepEqual(result.errors, []);
  } finally {
    await close();
  }
});
