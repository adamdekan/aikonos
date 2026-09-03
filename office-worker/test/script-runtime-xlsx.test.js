import { test } from 'node:test';
import assert from 'node:assert/strict';
import '../src/handlers/index.js';
import { startServer, post, buildMultipartRequest, parseMultipartResponse } from '../test-support/helpers.js';

const MAX_OUTPUT = 5_000_000;

const CREATE_SCRIPT = `
import openpyxl
wb = openpyxl.Workbook()
ws = wb.active
ws['A1'] = 'Name'
ws['B1'] = 'Qty'
ws['A2'] = 'Widget'
ws['B2'] = 42
wb.save('output.xlsx')
`;

async function createXlsx(baseUrl, script = CREATE_SCRIPT) {
  const { contentType, body } = buildMultipartRequest([], { script, max_output_bytes: MAX_OUTPUT });
  const res = await post(baseUrl, '/v1/xlsx/create', contentType, body);
  assert.equal(res.status, 200, res.body.toString());
  const parts = parseMultipartResponse(res.headers['content-type'], res.body);
  const filePart = parts.find((p) => p.name === 'file');
  const resultPart = parts.find((p) => p.name === 'result');
  return { fileBuffer: filePart.buffer, result: JSON.parse(resultPart.buffer.toString('utf8')) };
}

async function extractXlsx(baseUrl, buffer, params = {}) {
  const { contentType, body } = buildMultipartRequest(
    [{ name: 'file', filename: 'in.xlsx', buffer }],
    { max_output_bytes: MAX_OUTPUT, ...params }
  );
  return post(baseUrl, '/v1/xlsx/extract', contentType, body);
}

test('script-runtime: xlsx.create produces a valid .xlsx that xlsx.extract reads back', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const { fileBuffer, result } = await createXlsx(baseUrl);
    assert.ok(fileBuffer.length > 0);
    assert.equal(result.filename, 'output.xlsx');

    const res = await extractXlsx(baseUrl, fileBuffer);
    assert.equal(res.status, 200, res.body.toString());
    const parts = parseMultipartResponse(res.headers['content-type'], res.body);
    const resultPart = JSON.parse(parts.find((p) => p.name === 'result').buffer.toString('utf8'));
    assert.match(resultPart.content, /Widget/);
    assert.match(resultPart.content, /42/);
  } finally {
    await close();
  }
});

test('script-runtime: xlsx.edit rejects a path-escaping output_filename with 400', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const { fileBuffer } = await createXlsx(baseUrl);
    const editScript = `
import openpyxl
wb = openpyxl.load_workbook('input.xlsx')
wb.save('output.xlsx')
`;
    const { contentType, body } = buildMultipartRequest(
      [{ name: 'file', filename: 'in.xlsx', buffer: fileBuffer }],
      { script: editScript, output_filename: '../../x', max_output_bytes: MAX_OUTPUT }
    );
    const res = await post(baseUrl, '/v1/xlsx/edit', contentType, body);
    assert.equal(res.status, 400, res.body.toString());
    assert.match(JSON.parse(res.body).error, /output_filename/);
  } finally {
    await close();
  }
});

test('script-runtime: xlsx.edit rejects an absolute output_filename with 400', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const { fileBuffer } = await createXlsx(baseUrl);
    const editScript = `
import openpyxl
wb = openpyxl.load_workbook('input.xlsx')
wb.save('output.xlsx')
`;
    const { contentType, body } = buildMultipartRequest(
      [{ name: 'file', filename: 'in.xlsx', buffer: fileBuffer }],
      { script: editScript, output_filename: '/etc/hostname', max_output_bytes: MAX_OUTPUT }
    );
    const res = await post(baseUrl, '/v1/xlsx/edit', contentType, body);
    assert.equal(res.status, 400, res.body.toString());
    assert.match(JSON.parse(res.body).error, /output_filename/);
  } finally {
    await close();
  }
});

test('script-runtime: xlsx.edit still accepts a benign nested output_filename', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const { fileBuffer } = await createXlsx(baseUrl);
    const editScript = `
import openpyxl, os
wb = openpyxl.load_workbook('input.xlsx')
os.makedirs('out', exist_ok=True)
wb.save('out/result.xlsx')
`;
    const { contentType, body } = buildMultipartRequest(
      [{ name: 'file', filename: 'in.xlsx', buffer: fileBuffer }],
      { script: editScript, output_filename: 'out/result.xlsx', max_output_bytes: MAX_OUTPUT }
    );
    const res = await post(baseUrl, '/v1/xlsx/edit', contentType, body);
    assert.equal(res.status, 200, res.body.toString());
    const parts = parseMultipartResponse(res.headers['content-type'], res.body);
    const filePart = parts.find((p) => p.name === 'file');
    assert.ok(filePart.buffer.length > 0);
  } finally {
    await close();
  }
});

test('script-runtime: xlsx.extract supports json format and a cell range', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const { fileBuffer } = await createXlsx(baseUrl);
    const res = await extractXlsx(baseUrl, fileBuffer, { format: 'json', range: 'A1:B2' });
    assert.equal(res.status, 200, res.body.toString());
    const parts = parseMultipartResponse(res.headers['content-type'], res.body);
    const result = JSON.parse(parts.find((p) => p.name === 'result').buffer.toString('utf8'));
    const rows = JSON.parse(result.content);
    assert.deepEqual(rows, [
      ['Name', 'Qty'],
      ['Widget', 42],
    ]);
  } finally {
    await close();
  }
});

test('script-runtime: xlsx.edit modifies an existing workbook via a model-authored script', async () => {
  const { baseUrl, close } = await startServer();
  try {
    const { fileBuffer } = await createXlsx(baseUrl);

    const editScript = `
import openpyxl
wb = openpyxl.load_workbook('input.xlsx')
ws = wb.active
ws['B2'] = 99
wb.save('output.xlsx')
`;
    const { contentType, body } = buildMultipartRequest(
      [{ name: 'file', filename: 'in.xlsx', buffer: fileBuffer }],
      { script: editScript, max_output_bytes: MAX_OUTPUT }
    );
    const editRes = await post(baseUrl, '/v1/xlsx/edit', contentType, body);
    assert.equal(editRes.status, 200, editRes.body.toString());
    const editParts = parseMultipartResponse(editRes.headers['content-type'], editRes.body);
    const editedBuffer = editParts.find((p) => p.name === 'file').buffer;

    const extractRes = await extractXlsx(baseUrl, editedBuffer);
    const extractParts = parseMultipartResponse(extractRes.headers['content-type'], extractRes.body);
    const result = JSON.parse(extractParts.find((p) => p.name === 'result').buffer.toString('utf8'));
    assert.match(result.content, /99/);
    assert.doesNotMatch(result.content, /42/);
  } finally {
    await close();
  }
});
