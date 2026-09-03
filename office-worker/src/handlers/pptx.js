import { readdir, readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { registerHandler } from '../ops.js';
import { JPEG_MIME, PPTX_MIME } from './mime.js';
import { createZipEditHandler } from './zip-edit.js';
import { runXmlQuery } from './xml-query.js';
import { runSoffice } from './soffice.js';
import {
  HandlerInputError,
  ScriptError,
  VENV_BIN_DIR,
  readJobFile,
  runCommand,
  runNodeScript,
  writeJobFile,
} from './run-script.js';

const DEFAULT_OUTPUT = 'output.pptx';
const MARKITDOWN = join(VENV_BIN_DIR, 'markitdown');
const DEFAULT_THUMBNAIL_DPI = 150;
const DEFAULT_MAX_PAGES = 20;

registerHandler('pptx/create', async ({ jobDir, params, spawn }) => {
  if (!params.script) throw new HandlerInputError('params.script (pptxgenjs source) is required');
  const outputFilename = params.output_filename || DEFAULT_OUTPUT;

  await runNodeScript({ jobDir, spawn, script: params.script });

  const buffer = await readJobFile(jobDir, outputFilename, `script did not produce "${outputFilename}"`);
  return {
    outputs: [{ name: 'file', filename: outputFilename, contentType: PPTX_MIME, buffer }],
    result: { filename: outputFilename, size: buffer.length },
  };
});

registerHandler('pptx/edit', createZipEditHandler({ mimeType: PPTX_MIME, defaultOutputFilename: DEFAULT_OUTPUT }));

registerHandler('pptx/extract', async ({ jobDir, files, params, spawn }) => {
  const input = files.find((f) => f.fieldname === 'file');
  if (!input) throw new HandlerInputError('a "file" part is required');
  const inputPath = await writeJobFile(jobDir, 'input.pptx', input.buffer);

  if (params.format === 'xml') {
    const result = await runXmlQuery({ jobDir, spawn, inputPath, params });
    return { outputs: [], result };
  }

  const outputPath = join(jobDir, 'output.md');
  await runCommand({ spawn, cmd: MARKITDOWN, args: [inputPath, '-o', outputPath], cwd: jobDir });

  const content = await readJobFile(jobDir, 'output.md', 'markitdown did not produce markdown output');
  return { outputs: [], result: { content: content.toString('utf8') } };
});

// pptx.thumbnail: slides -> JPEG page images via soffice -> pdf -> pdftoppm
//. Renders at most
// params.max_pages (default 20); excess slides are skipped, not errored —
// pdftoppm's -l bound stops it from rendering pages we'd discard anyway.
registerHandler('pptx/thumbnail', async ({ jobDir, files, params, spawn }) => {
  const input = files.find((f) => f.fieldname === 'file');
  if (!input) throw new HandlerInputError('a "file" part is required');
  const dpi = params.dpi || DEFAULT_THUMBNAIL_DPI;
  const maxPages = params.max_pages || DEFAULT_MAX_PAGES;

  const inputPath = await writeJobFile(jobDir, 'input.pptx', input.buffer);
  const outDir = join(jobDir, 'out');
  await runSoffice({ jobDir, spawn, extraArgs: ['--convert-to', 'pdf', '--outdir', outDir, inputPath] });

  const pdfPath = join(outDir, 'input.pdf');
  const { stdout } = await runCommand({ spawn, cmd: 'pdfinfo', args: [pdfPath], cwd: jobDir });
  const pagesMatch = stdout.match(/^Pages:\s+(\d+)/m);
  if (!pagesMatch) throw new ScriptError('pdfinfo did not report a page count for the converted PDF');
  const totalPages = Number(pagesMatch[1]);
  const rendered = Math.min(totalPages, maxPages);
  const skipped = Math.max(totalPages - maxPages, 0);

  const prefix = join(outDir, 'slide');
  await runCommand({
    spawn,
    cmd: 'pdftoppm',
    args: ['-r', String(dpi), '-jpeg', '-f', '1', '-l', String(rendered), pdfPath, prefix],
    cwd: jobDir,
  });

  const slideFilenames = (await readdir(outDir))
    .filter((name) => name.startsWith('slide-') && name.endsWith('.jpg'))
    .sort((a, b) => Number(a.match(/(\d+)/)[1]) - Number(b.match(/(\d+)/)[1]));

  const outputs = [];
  for (const filename of slideFilenames) {
    const buffer = await readFile(join(outDir, filename));
    outputs.push({ name: filename, filename, contentType: JPEG_MIME, buffer });
  }

  return {
    outputs,
    result: { count: outputs.length, skipped, total_pages: totalPages, dpi },
  };
});
