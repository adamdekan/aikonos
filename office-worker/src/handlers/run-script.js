// Shared plumbing for checkpoint 2's script-runtime ops: writing a
// model-authored or fixed script into the job dir and running it under the
// pinned node/python interpreter via the injected `spawn` (job.js's
// group-kill timeout contract requires every subprocess to go through it —
// see API.md "Timeout semantics").
import { writeFile, readFile, symlink } from 'node:fs/promises';
import { join, dirname, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

// office-worker/ root: two levels up from src/handlers/run-script.js.
export const WORKER_ROOT = join(dirname(fileURLToPath(import.meta.url)), '..', '..');
export const NODE_MODULES_DIR = join(WORKER_ROOT, 'node_modules');
export const VENV_PYTHON = join(WORKER_ROOT, '.venv', 'bin', 'python3');
export const VENV_BIN_DIR = join(WORKER_ROOT, '.venv', 'bin');
export const SCRIPTS_DIR = join(WORKER_ROOT, 'src', 'scripts');

// Per-job V8 old-space ceiling for model-authored node scripts, so one runaway
// script OOMs itself rather than the shared process every other tenant's
// in-flight job is also using. The bound that matters is the *aggregate*:
// NODE_MAX_OLD_SPACE_MB × OFFICE_MAX_CONCURRENCY must stay well under the
// container's mem_limit (2g, compose.yaml), since an attacker picks the
// multiplier by sending that many requests at once. At the defaults that is
// 256 × 4 = 1 GiB, leaving the other GiB for the worker process itself, any
// concurrent python/soffice job, and buffered ingress. Raising either knob
// without re-checking the product re-opens the hole.
// Deliberately not compose-substituted — same precedent as SOFFICE_TIMEOUT_MS
// in server.js and the gateway's AIKONOS_WORKFLOW_REASON_MAX_TOKENS: a 4th env
// key in compose.yaml would drift from scripts/check-env-drift.sh's three .env
// templates.
export const NODE_MAX_OLD_SPACE_MB = Number(process.env.OFFICE_NODE_MAX_OLD_SPACE_MB) || 256;

// Caller-supplied params were invalid (missing required field, bad shape) —
// distinct from a script/subprocess failure, mapped to 400 not 500.
export class HandlerInputError extends Error {
  constructor(message) {
    super(message);
    this.name = 'HandlerInputError';
    this.statusCode = 400;
  }
}

// A model-authored script, or a fixed helper script, exited non-zero or
// failed to produce its expected output file.
export class ScriptError extends Error {
  constructor(message) {
    super(message);
    this.name = 'ScriptError';
    this.statusCode = 500;
  }
}

// Runs cmd/args to completion under cwd via the injected spawn, capturing
// stdout/stderr. Rejects with ScriptError on non-zero exit or spawn failure —
// stderr (falling back to stdout) rides the error message for diagnostics.
export function runCommand({ spawn, cmd, args, cwd }) {
  return new Promise((resolve, reject) => {
    let child;
    try {
      child = spawn(cmd, args, { cwd, stdio: ['ignore', 'pipe', 'pipe'] });
    } catch (err) {
      reject(new ScriptError(`failed to start ${cmd}: ${err.message}`));
      return;
    }
    let stdout = '';
    let stderr = '';
    child.stdout.on('data', (chunk) => (stdout += chunk));
    child.stderr.on('data', (chunk) => (stderr += chunk));
    child.on('error', (err) => reject(new ScriptError(`${cmd} failed to start: ${err.message}`)));
    child.on('exit', (code, signal) => {
      if (code === 0) {
        resolve({ stdout, stderr });
      } else {
        reject(
          new ScriptError(
            `${cmd} exited with code ${code}${signal ? ` (signal ${signal})` : ''}: ${
              stderr.trim() || stdout.trim() || '(no output)'
            }`
          )
        );
      }
    });
  });
}

// Writes a model-authored node script into jobDir and symlinks the worker's
// own node_modules into it — CJS module resolution walks up from the
// importing file's directory, so a node_modules dir alongside the script is
// enough to resolve pinned libs (docx, pptxgenjs) without NODE_PATH or a
// shared volume. Default filename is .cjs (not .mjs) despite package.json's
// "type":"module": model-authored scripts (and the upstream Anthropic
// docx/pptx skills they're adapted from) use CommonJS require("docx") —
// a .cjs extension forces CommonJS regardless of the package's module type,
// so require works.
export async function runNodeScript({ jobDir, spawn, script, filename = 'script.cjs', args = [] }) {
  const scriptPath = join(jobDir, filename);
  await writeFile(scriptPath, script, 'utf8');
  await symlink(NODE_MODULES_DIR, join(jobDir, 'node_modules'), 'dir').catch(() => {});
  // The V8 flag must precede the script path — node stops parsing its own
  // options at the first positional argument.
  return runCommand({
    spawn,
    cmd: process.execPath,
    args: [`--max-old-space-size=${NODE_MAX_OLD_SPACE_MB}`, scriptPath, ...args],
    cwd: jobDir,
  });
}

// Writes a model-authored python script into jobDir and runs it under the
// worker's uv-managed venv interpreter (pinned openpyxl/reportlab/etc).
export async function runPythonScript({ jobDir, spawn, script, filename = 'script.py', args = [] }) {
  const scriptPath = join(jobDir, filename);
  await writeFile(scriptPath, script, 'utf8');
  return runCommand({ spawn, cmd: VENV_PYTHON, args: [scriptPath, ...args], cwd: jobDir });
}

// Runs one of this worker's own fixed (non-model-authored) helper scripts
// under src/scripts/ — used by the declarative XML-edit and extract ops that
// take no user code, only a script name relative to SCRIPTS_DIR plus args.
export function runFixedPythonScript({ jobDir, spawn, script, args = [] }) {
  return runCommand({ spawn, cmd: VENV_PYTHON, args: [join(SCRIPTS_DIR, script), ...args], cwd: jobDir });
}

// Rejects a caller-supplied filename (params.output_filename) that would
// resolve outside jobDir — absolute paths and "../" traversal — while still
// allowing legitimate nested relative paths a model script writes (e.g.
// "out/result.docx"), since path.resolve keeps those under jobDir.
export function assertPathWithinJobDir(jobDir, filename, paramName = 'output_filename') {
  const resolved = resolve(jobDir, filename);
  const boundary = jobDir.endsWith(sep) ? jobDir : jobDir + sep;
  if (resolved !== jobDir && !resolved.startsWith(boundary)) {
    throw new HandlerInputError(`params.${paramName} must resolve inside the job directory`);
  }
}

// Reads an expected output file out of jobDir; a missing file means the
// script silently failed to produce its result, which is a script failure
// (500), not a caller input error.
export async function readJobFile(jobDir, filename, missingMessage, paramName = 'output_filename') {
  assertPathWithinJobDir(jobDir, filename, paramName);
  try {
    return await readFile(join(jobDir, filename));
  } catch (err) {
    if (err.code === 'ENOENT') throw new ScriptError(missingMessage);
    throw err;
  }
}

// Guarded the same way as readJobFile — every call site today passes a
// hardcoded literal so this is a no-op for them, but office.js's
// source_extension-derived filename is caller-influenced and must not be
// allowed to escape jobDir.
export async function writeJobFile(jobDir, filename, data, paramName = 'output_filename') {
  assertPathWithinJobDir(jobDir, filename, paramName);
  const path = join(jobDir, filename);
  await writeFile(path, data);
  return path;
}

export async function readJobJson(jobDir, filename, missingMessage) {
  const buf = await readJobFile(jobDir, filename, missingMessage);
  return JSON.parse(buf.toString('utf8'));
}
