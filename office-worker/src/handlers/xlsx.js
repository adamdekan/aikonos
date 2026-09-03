import { join } from 'node:path';
import { registerHandler } from '../ops.js';
import { XLSX_MIME } from './mime.js';
import { runSoffice } from './soffice.js';
import {
  HandlerInputError,
  readJobFile,
  readJobJson,
  runFixedPythonScript,
  runPythonScript,
  writeJobFile,
} from './run-script.js';

const DEFAULT_OUTPUT = 'output.xlsx';

registerHandler('xlsx/create', async ({ jobDir, params, spawn }) => {
  if (!params.script) throw new HandlerInputError('params.script (openpyxl source) is required');
  const outputFilename = params.output_filename || DEFAULT_OUTPUT;

  await runPythonScript({ jobDir, spawn, script: params.script });

  const buffer = await readJobFile(jobDir, outputFilename, `script did not produce "${outputFilename}"`);
  return {
    outputs: [{ name: 'file', filename: outputFilename, contentType: XLSX_MIME, buffer }],
    result: { filename: outputFilename, size: buffer.length },
  };
});

// xlsx.edit: the model's script opens the workbook written to jobDir as
// "input.xlsx" (the convention its skill-bundle prompt teaches) and writes
// its result to output_filename (default "output.xlsx").
registerHandler('xlsx/edit', async ({ jobDir, files, params, spawn }) => {
  const input = files.find((f) => f.fieldname === 'file');
  if (!input) throw new HandlerInputError('a "file" part is required');
  if (!params.script) throw new HandlerInputError('params.script (openpyxl source) is required');
  const outputFilename = params.output_filename || DEFAULT_OUTPUT;

  await writeJobFile(jobDir, 'input.xlsx', input.buffer);
  await runPythonScript({ jobDir, spawn, script: params.script });

  const buffer = await readJobFile(jobDir, outputFilename, `script did not produce "${outputFilename}"`);
  return {
    outputs: [{ name: 'file', filename: outputFilename, contentType: XLSX_MIME, buffer }],
    result: { filename: outputFilename, size: buffer.length },
  };
});

registerHandler('xlsx/extract', async ({ jobDir, files, params, spawn }) => {
  const input = files.find((f) => f.fieldname === 'file');
  if (!input) throw new HandlerInputError('a "file" part is required');

  const inputPath = await writeJobFile(jobDir, 'input.xlsx', input.buffer);
  const extractParams = { sheet: params.sheet, range: params.range, format: params.format };
  const paramsPath = join(jobDir, 'xlsx-extract-params.json');
  await writeJobFile(jobDir, 'xlsx-extract-params.json', JSON.stringify(extractParams));
  const outputPath = join(jobDir, 'xlsx-extract-result.json');

  await runFixedPythonScript({
    jobDir,
    spawn,
    script: 'xlsx_extract.py',
    args: [inputPath, paramsPath, outputPath],
  });

  const result = await readJobJson(jobDir, 'xlsx-extract-result.json', 'xlsx extract did not produce a result');
  return { outputs: [], result };
});

// xlsx.recalc: soffice round-trips the workbook through --convert-to xlsx
// under soffice.js's always-recalculate profile, then a fixed helper scans
// the recalculated cells for Excel error tokens (upstream recalc.py port —
//  "Tool surface").
registerHandler('xlsx/recalc', async ({ jobDir, files, params, spawn }) => {
  const input = files.find((f) => f.fieldname === 'file');
  if (!input) throw new HandlerInputError('a "file" part is required');
  const outputFilename = params.output_filename || DEFAULT_OUTPUT;

  const inputPath = await writeJobFile(jobDir, 'input.xlsx', input.buffer);
  // --outdir must differ from the input's own dir: soffice refuses to
  // convert a file onto its own path (see soffice.js).
  const outDir = join(jobDir, 'out');
  await runSoffice({ jobDir, spawn, extraArgs: ['--convert-to', 'xlsx', '--outdir', outDir, inputPath] });

  const recalculated = await readJobFile(
    jobDir,
    join('out', 'input.xlsx'),
    'soffice did not produce a recalculated workbook'
  );

  const scanResultPath = join(jobDir, 'xlsx-recalc-result.json');
  await runFixedPythonScript({
    jobDir,
    spawn,
    script: 'xlsx_recalc_scan.py',
    args: [join(outDir, 'input.xlsx'), scanResultPath],
  });
  const scan = await readJobJson(jobDir, 'xlsx-recalc-result.json', 'recalc scan did not produce a result');

  return {
    outputs: [{ name: 'file', filename: outputFilename, contentType: XLSX_MIME, buffer: recalculated }],
    result: { filename: outputFilename, size: recalculated.length, ...scan },
  };
});
