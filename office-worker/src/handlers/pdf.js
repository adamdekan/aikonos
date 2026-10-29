import { join } from 'node:path';
import { registerHandler } from '../ops.js';
import { PDF_MIME } from './mime.js';
import {
  HandlerInputError,
  readJobFile,
  readJobJson,
  runCommand,
  runFixedPythonScript,
  runPythonScript,
  writeJobFile,
} from './run-script.js';

const DEFAULT_OUTPUT = 'output.pdf';
const QPDF_BIN = process.env.QPDF_BIN || 'qpdf';

registerHandler('pdf/create', async ({ jobDir, params, spawn }) => {
  if (!params.script) throw new HandlerInputError('params.script (reportlab source) is required');
  const outputFilename = params.output_filename || DEFAULT_OUTPUT;

  await runPythonScript({ jobDir, spawn, script: params.script });

  const buffer = await readJobFile(jobDir, outputFilename, `script did not produce "${outputFilename}"`);
  return {
    outputs: [{ name: 'file', filename: outputFilename, contentType: PDF_MIME, buffer }],
    result: { filename: outputFilename, size: buffer.length },
  };
});

registerHandler('pdf/extract', async ({ jobDir, files, params, spawn }) => {
  const input = files.find((f) => f.fieldname === 'file');
  if (!input) throw new HandlerInputError('a "file" part is required');

  const inputPath = await writeJobFile(jobDir, 'input.pdf', input.buffer);
  const paramsPath = join(jobDir, 'pdf-extract-params.json');
  await writeJobFile(jobDir, 'pdf-extract-params.json', JSON.stringify({ ocr: params.ocr }));
  const outputPath = join(jobDir, 'pdf-extract-result.json');

  await runFixedPythonScript({
    jobDir,
    spawn,
    script: 'pdf_extract.py',
    args: [inputPath, paramsPath, outputPath],
  });

  const result = await readJobJson(jobDir, 'pdf-extract-result.json', 'pdf extract did not produce a result');
  return { outputs: [], result };
});

// pdf.transform: declarative ops via qpdf + pypdf — no model-authored script
//. params.op selects
// the op; each is verified against the real qpdf/pypdf on this host.

function outputPart(filename, buffer) {
  return { name: 'file', filename, contentType: PDF_MIME, buffer };
}

// qpdf page-range syntax, allowlisted: n, rN (from the end), z (last page),
// a-b ranges, an optional :even/:odd suffix, comma-separated. A caller value
// reaches qpdf's argv, where a leading "@" is expanded as an argument file
// (@input.pdf -> attacker-chosen qpdf options, e.g. --add-attachment of any
// readable file) and a leading "-" is an option — neither can match.
const PAGE = '(?:r?\\d+|z)';
const PAGE_RANGE = `${PAGE}(?:-${PAGE})?(?::(?:even|odd))?`;
const PAGE_RANGE_RE = new RegExp(`^${PAGE_RANGE}(?:,${PAGE_RANGE})*$`);
const MAX_PAGE_RANGE_LENGTH = 256;
// One qpdf run and one output part per range; matches pptx.thumbnail's page
// cap so a response never carries more parts than the broker accepts.
const MAX_SPLIT_RANGES = 50;

function assertPageRange(value, paramName) {
  if (typeof value !== 'string' || value.length > MAX_PAGE_RANGE_LENGTH || !PAGE_RANGE_RE.test(value)) {
    throw new HandlerInputError(`${paramName} must be a qpdf page-range string (e.g. "1-2", "1,3,r1", "1-z:odd")`);
  }
}

async function opMerge({ jobDir, files, params, spawn }) {
  if (!files.length) throw new HandlerInputError('pdf.transform "merge" requires one or more "file" parts');
  const outputFilename = params.output_filename || DEFAULT_OUTPUT;

  const inputPaths = [];
  for (const [i, file] of files.entries()) {
    inputPaths.push(await writeJobFile(jobDir, `merge-in-${i}.pdf`, file.buffer));
  }
  await runCommand({
    spawn,
    cmd: QPDF_BIN,
    args: ['--empty', '--pages', ...inputPaths, '--', join(jobDir, 'output.pdf')],
    cwd: jobDir,
  });

  const buffer = await readJobFile(jobDir, 'output.pdf', 'qpdf did not produce a merged PDF');
  return { outputs: [outputPart(outputFilename, buffer)], result: { filename: outputFilename, size: buffer.length } };
}

async function opSplit({ jobDir, files, params, spawn }) {
  const input = files.find((f) => f.fieldname === 'file');
  if (!input) throw new HandlerInputError('a "file" part is required');
  if (!Array.isArray(params.ranges) || params.ranges.length === 0) {
    throw new HandlerInputError('pdf.transform "split" requires a non-empty params.ranges array');
  }
  if (params.ranges.length > MAX_SPLIT_RANGES) {
    throw new HandlerInputError(`pdf.transform "split" accepts at most ${MAX_SPLIT_RANGES} ranges`);
  }

  // Validated up front, before qpdf runs for any range.
  for (const [i, range] of params.ranges.entries()) assertPageRange(range?.pages, `params.ranges[${i}].pages`);

  const inputPath = await writeJobFile(jobDir, 'input.pdf', input.buffer);
  const outputs = [];
  const files_ = [];
  for (const [i, range] of params.ranges.entries()) {
    const outputFilename = range.output_filename || `split-${i + 1}.pdf`;
    const splitBasename = `split-${i}.pdf`;
    await runCommand({
      spawn,
      cmd: QPDF_BIN,
      args: [inputPath, '--pages', inputPath, range.pages, '--', join(jobDir, splitBasename)],
      cwd: jobDir,
    });
    const buffer = await readJobFile(
      jobDir,
      splitBasename,
      `qpdf did not produce split output for range "${range.pages}"`
    );
    outputs.push(outputPart(outputFilename, buffer));
    files_.push({ filename: outputFilename, size: buffer.length, pages: range.pages });
  }
  return { outputs, result: { files: files_ } };
}

const ROTATE_ANGLES = new Set([90, 180, 270, -90, -180, -270]);

async function opRotate({ jobDir, files, params, spawn }) {
  const input = files.find((f) => f.fieldname === 'file');
  if (!input) throw new HandlerInputError('a "file" part is required');
  if (!ROTATE_ANGLES.has(params.angle)) {
    throw new HandlerInputError('params.angle must be one of 90, 180, 270, -90, -180, -270');
  }
  const pages = params.pages || '1-z';
  assertPageRange(pages, 'params.pages');
  const outputFilename = params.output_filename || DEFAULT_OUTPUT;

  const inputPath = await writeJobFile(jobDir, 'input.pdf', input.buffer);
  const sign = params.angle >= 0 ? '+' : '';
  await runCommand({
    spawn,
    cmd: QPDF_BIN,
    args: [inputPath, `--rotate=${sign}${params.angle}:${pages}`, join(jobDir, 'output.pdf')],
    cwd: jobDir,
  });

  const buffer = await readJobFile(jobDir, 'output.pdf', 'qpdf did not produce a rotated PDF');
  return { outputs: [outputPart(outputFilename, buffer)], result: { filename: outputFilename, size: buffer.length } };
}

async function opDecrypt({ jobDir, files, params, spawn }) {
  const input = files.find((f) => f.fieldname === 'file');
  if (!input) throw new HandlerInputError('a "file" part is required');
  if (typeof params.password !== 'string' || !params.password) {
    throw new HandlerInputError('params.password is required for pdf.transform "decrypt"');
  }
  // qpdf reads only the first line of a password file.
  if (/[\r\n]/.test(params.password)) {
    throw new HandlerInputError('params.password must not contain a line break');
  }
  const outputFilename = params.output_filename || DEFAULT_OUTPUT;

  const inputPath = await writeJobFile(jobDir, 'input.pdf', input.buffer);
  // Via a file in the 0700 job dir, not argv: /proc/<pid>/cmdline is
  // readable by every process in the container.
  const passwordPath = await writeJobFile(jobDir, 'password.txt', params.password);
  await runCommand({
    spawn,
    cmd: QPDF_BIN,
    args: [`--password-file=${passwordPath}`, '--decrypt', inputPath, join(jobDir, 'output.pdf')],
    cwd: jobDir,
  });

  const buffer = await readJobFile(jobDir, 'output.pdf', 'qpdf did not produce a decrypted PDF');
  return { outputs: [outputPart(outputFilename, buffer)], result: { filename: outputFilename, size: buffer.length } };
}

async function opFillForms({ jobDir, files, params, spawn }) {
  const input = files.find((f) => f.fieldname === 'file');
  if (!input) throw new HandlerInputError('a "file" part is required');
  if (!params.fields || typeof params.fields !== 'object' || Array.isArray(params.fields)) {
    throw new HandlerInputError('params.fields must be an object of field name -> value');
  }
  const outputFilename = params.output_filename || DEFAULT_OUTPUT;

  const inputPath = await writeJobFile(jobDir, 'input.pdf', input.buffer);
  const fieldsPath = join(jobDir, 'fill-forms-fields.json');
  await writeJobFile(jobDir, 'fill-forms-fields.json', JSON.stringify(params.fields));

  await runFixedPythonScript({
    jobDir,
    spawn,
    script: 'pdf_fill_forms.py',
    args: [inputPath, fieldsPath, join(jobDir, 'output.pdf')],
  });

  const buffer = await readJobFile(jobDir, 'output.pdf', 'pypdf did not produce a filled PDF');
  return { outputs: [outputPart(outputFilename, buffer)], result: { filename: outputFilename, size: buffer.length } };
}

const TRANSFORM_OPS = {
  merge: opMerge,
  split: opSplit,
  rotate: opRotate,
  decrypt: opDecrypt,
  'fill-forms': opFillForms,
};

registerHandler('pdf/transform', async (ctx) => {
  const handler = TRANSFORM_OPS[ctx.params.op];
  if (!handler) {
    throw new HandlerInputError(
      `params.op must be one of ${Object.keys(TRANSFORM_OPS).join(', ')}, got ${JSON.stringify(ctx.params.op)}`
    );
  }
  return handler(ctx);
});
