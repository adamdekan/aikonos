// Shared engine behind docx.edit and pptx.edit: both formats are OOXML zip
// archives, so one declarative string/regex-replace-on-named-member engine
// covers both. No script
// execution — ops are data, applied by the fixed src/scripts/zip_edit.py.
import { join } from 'node:path';
import {
  HandlerInputError,
  assertPathWithinJobDir,
  readJobFile,
  runFixedPythonScript,
  writeJobFile,
} from './run-script.js';

function validateOps(ops) {
  if (!Array.isArray(ops) || ops.length === 0) {
    throw new HandlerInputError('params.ops must be a non-empty array');
  }
  for (const op of ops) {
    if (!op || typeof op.member !== 'string' || !op.member) {
      throw new HandlerInputError('every op requires a string "member"');
    }
    if (typeof op.find !== 'string' || typeof op.replace !== 'string') {
      throw new HandlerInputError(`op on member "${op.member}" requires string "find" and "replace"`);
    }
  }
}

export function createZipEditHandler({ mimeType, defaultOutputFilename }) {
  return async function zipEditHandler({ jobDir, files, params, spawn }) {
    const input = files.find((f) => f.fieldname === 'file');
    if (!input) throw new HandlerInputError('a "file" part is required');
    validateOps(params.ops);

    const outputFilename = params.output_filename || defaultOutputFilename;
    assertPathWithinJobDir(jobDir, outputFilename);
    const inputPath = await writeJobFile(jobDir, 'input.zip', input.buffer);
    const opsPath = join(jobDir, 'ops.json');
    await writeJobFile(jobDir, 'ops.json', JSON.stringify(params.ops));
    const outputPath = join(jobDir, 'output.zip');

    await runFixedPythonScript({
      jobDir,
      spawn,
      script: 'zip_edit.py',
      args: [inputPath, opsPath, outputPath],
    });

    const buffer = await readJobFile(jobDir, 'output.zip', 'zip edit did not produce an output archive');
    return {
      outputs: [{ name: 'file', filename: outputFilename, contentType: mimeType, buffer }],
      result: { filename: outputFilename, size: buffer.length },
    };
  };
}
