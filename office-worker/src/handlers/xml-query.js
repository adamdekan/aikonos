// Shared docx.extract/pptx.extract `format:"xml"` mode: returns a named
// archive member (bounded by an echo cap), optionally narrowed to regions
// matching a `pattern` with surrounding context — so the model can aim
// *.edit ops at real OOXML instead of guessing.
import { join } from 'node:path';
import { HandlerInputError, readJobJson, runFixedPythonScript, writeJobFile } from './run-script.js';

export async function runXmlQuery({ jobDir, spawn, inputPath, params }) {
  if (typeof params.member !== 'string' || !params.member) {
    throw new HandlerInputError('params.member (archive member name) is required for format:"xml"');
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
