// office.convert: any<->any LibreOffice conversion, including legacy
// .doc -> .docx.
import { extname, join } from 'node:path';
import { registerHandler } from '../ops.js';
import { runSoffice } from './soffice.js';
import { HandlerInputError, readJobFile, writeJobFile } from './run-script.js';

const CONTENT_TYPE_BY_EXT = {
  pdf: 'application/pdf',
  docx: 'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
  doc: 'application/msword',
  xlsx: 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
  xls: 'application/vnd.ms-excel',
  pptx: 'application/vnd.openxmlformats-officedocument.presentationml.presentation',
  ppt: 'application/vnd.ms-powerpoint',
  odt: 'application/vnd.oasis.opendocument.text',
  ods: 'application/vnd.oasis.opendocument.spreadsheet',
  odp: 'application/vnd.oasis.opendocument.presentation',
  rtf: 'application/rtf',
  txt: 'text/plain',
};

registerHandler('office/convert', async ({ jobDir, files, params, spawn }) => {
  const input = files.find((f) => f.fieldname === 'file');
  if (!input) throw new HandlerInputError('a "file" part is required');
  if (typeof params.target_format !== 'string' || !params.target_format) {
    throw new HandlerInputError('params.target_format (e.g. "pdf", "docx") is required');
  }
  const targetFormat = params.target_format.toLowerCase().replace(/^\./, '');
  // A bare extension only — mirrors the source_extension guard below: it
  // flows into readJobFile(... 'input.' + targetFormat) and --convert-to,
  // so an unvalidated value is a "../"-style traversal vector exactly like
  // source_extension's.
  if (!/^[a-zA-Z0-9]+$/.test(targetFormat)) {
    throw new HandlerInputError('params.target_format must be a bare file extension (letters/digits only)');
  }

  // soffice picks the input filter from the file's extension — derive it
  // from the uploaded part's own filename (legacy .doc round-trips need
  // this) or an explicit override for a caller that stripped it.
  const sourceExt = (params.source_extension || extname(input.filename || '')).replace(/^\./, '');
  if (!sourceExt) {
    throw new HandlerInputError(
      'params.source_extension is required when the input file part carries no filename extension'
    );
  }
  // A bare extension only — blocks "../"-style traversal via
  // params.source_extension before it ever reaches writeJobFile (which also
  // guards the write path itself, defense in depth).
  if (!/^[a-zA-Z0-9]+$/.test(sourceExt)) {
    throw new HandlerInputError('params.source_extension must be a bare file extension (letters/digits only)');
  }

  const inputPath = await writeJobFile(jobDir, `input.${sourceExt}`, input.buffer);
  // --outdir must differ from the input's own dir (see soffice.js).
  const outDir = join(jobDir, 'out');
  await runSoffice({ jobDir, spawn, extraArgs: ['--convert-to', targetFormat, '--outdir', outDir, inputPath] });

  const outputFilename = params.output_filename || `output.${targetFormat}`;
  const buffer = await readJobFile(
    jobDir,
    join('out', `input.${targetFormat}`),
    `soffice did not produce a "${targetFormat}" output`
  );

  return {
    outputs: [
      {
        name: 'file',
        filename: outputFilename,
        contentType: CONTENT_TYPE_BY_EXT[targetFormat] || 'application/octet-stream',
        buffer,
      },
    ],
    result: { filename: outputFilename, size: buffer.length },
  };
});
