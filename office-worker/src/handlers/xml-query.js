// Shared docx.extract/pptx.extract `format:"xml"` mode: returns a named
// archive member (bounded by an echo cap), optionally narrowed to regions
// matching a `pattern` with surrounding context — so the model can aim
// *.edit ops at real OOXML instead of guessing.
import { join } from 'node:path';
import { HandlerInputError, readJobJson, runFixedPythonScript, writeJobFile } from './run-script.js';
import { MAX_REGEX_LENGTH } from './zip-edit.js';

// The helper copies context_chars around every match and returns up to
// echo_cap chars; unbounded, one small upload makes it copy tens of MiB per
// match and write a result the broker refuses anyway.
const MAX_QUERY_PARAMS = { context_chars: 2000, echo_cap: 200000 };

export async function runXmlQuery({ jobDir, spawn, inputPath, params }) {
  if (typeof params.member !== 'string' || !params.member) {
    throw new HandlerInputError('params.member (archive member name) is required for format:"xml"');
  }
  if (params.pattern != null && (typeof params.pattern !== 'string' || params.pattern.length > MAX_REGEX_LENGTH)) {
    throw new HandlerInputError(`params.pattern must be a regex string of at most ${MAX_REGEX_LENGTH} characters`);
  }
  for (const [key, max] of Object.entries(MAX_QUERY_PARAMS)) {
    const value = params[key];
    if (value != null && !(Number.isInteger(value) && value >= 0 && value <= max)) {
      throw new HandlerInputError(`params.${key} must be an integer in [0, ${max}]`);
    }
  }

  const queryParams = {
    member: params.member,
    pattern: params.pattern,
    context_chars: params.context_chars,
    echo_cap: params.echo_cap,
  };
  await writeJobFile(jobDir, 'xml-query-params.json', JSON.stringify(queryParams));
  const outputPath = join(jobDir, 'xml-query-result.json');

  await runFixedPythonScript({
    jobDir,
    spawn,
    script: 'zip_xml_query.py',
    args: [inputPath, join(jobDir, 'xml-query-params.json'), outputPath],
  });

  return readJobJson(jobDir, 'xml-query-result.json', 'xml query did not produce a result');
}
