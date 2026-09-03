import { join } from 'node:path';
import { registerHandler } from '../ops.js';
import { DOCX_MIME } from './mime.js';
import { createZipEditHandler } from './zip-edit.js';
import { runXmlQuery } from './xml-query.js';
import { HandlerInputError, readJobFile, runCommand, runNodeScript, writeJobFile } from './run-script.js';

const DEFAULT_OUTPUT = 'output.docx';

registerHandler('docx/create', async ({ jobDir, params, spawn }) => {
  if (!params.script) throw new HandlerInputError('params.script (docx-js source) is required');
  const outputFilename = params.output_filename || DEFAULT_OUTPUT;

  await runNodeScript({ jobDir, spawn, script: params.script });

  const buffer = await readJobFile(jobDir, outputFilename, `script did not produce "${outputFilename}"`);
  return {
    outputs: [{ name: 'file', filename: outputFilename, contentType: DOCX_MIME, buffer }],
    result: { filename: outputFilename, size: buffer.length },
  };
});

registerHandler('docx/edit', createZipEditHandler({ mimeType: DOCX_MIME, defaultOutputFilename: DEFAULT_OUTPUT }));

registerHandler('docx/extract', async ({ jobDir, files, params, spawn }) => {
  const input = files.find((f) => f.fieldname === 'file');
  if (!input) throw new HandlerInputError('a "file" part is required');
  const inputPath = await writeJobFile(jobDir, 'input.docx', input.buffer);

  if (params.format === 'xml') {
    const result = await runXmlQuery({ jobDir, spawn, inputPath, params });
    return { outputs: [], result };
  }

  const outputPath = join(jobDir, 'output.md');
  const args = ['-f', 'docx', '-t', 'markdown'];
  if (params.track_changes) args.push(`--track-changes=${params.track_changes}`);
  args.push(inputPath, '-o', outputPath);
  await runCommand({ spawn, cmd: 'pandoc', args, cwd: jobDir });

  const content = await readJobFile(jobDir, 'output.md', 'pandoc did not produce markdown output');
  return { outputs: [], result: { content: content.toString('utf8') } };
});
