// Shared plumbing for checkpoint 2's script-runtime ops: writing a
// model-authored or fixed script into the job dir and running it under the
// pinned node/python interpreter via the injected `spawn` (job.js's
// group-kill timeout contract requires every subprocess to go through it —
// see API.md "Timeout semantics").
import { constants } from 'node:fs';
import { open, readlink, realpath, writeFile, symlink } from 'node:fs/promises';
import { join, dirname, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

// office-worker/ root: two levels up from src/handlers/run-script.js.
export const WORKER_ROOT = join(dirname(fileURLToPath(import.meta.url)), '..', '..');
export const NODE_MODULES_DIR = join(WORKER_ROOT, 'node_modules');
export const VENV_PYTHON = join(WORKER_ROOT, '.venv', 'bin', 'python3');
export const VENV_BIN_DIR = join(WORKER_ROOT, '.venv', 'bin');
export const SCRIPTS_DIR = join(WORKER_ROOT, 'src', 'scripts');

// Per-job V8 old-space ceiling for model-authored node scripts. It bounds the
// JS heap only — Buffers/ArrayBuffers live outside it — so the real per-job
// memory bound is the RLIMIT_AS below, applied through prlimit.
// Deliberately not compose-substituted — same precedent as SOFFICE_TIMEOUT_MS
// in server.js and the gateway's AIKONOS_WORKFLOW_REASON_MAX_TOKENS: a 4th env
// key in compose.yaml would drift from scripts/check-env-drift.sh's three .env
// templates.
export const NODE_MAX_OLD_SPACE_MB = Number(process.env.OFFICE_NODE_MAX_OLD_SPACE_MB) || 256;

// Address-space ceiling (RLIMIT_AS) for a model-authored script, so a
// `bytearray(2<<30)` or `Buffer.alloc(1.9e9)` fails inside the script instead
// of OOM-killing the shared container. python gets it as is; node gets it on
// top of its heap, because V8 reserves ~0.7 GiB of address space it never
// touches (measured on node 22 with --v8-pool-size=2: 3x the heap cannot even
// allocate a 50 MB Buffer). Per job, not aggregate: OFFICE_MAX_CONCURRENCY
// jobs can still sum past the container's mem_limit, which stays the
// backstop. Same not-in-compose precedent as above.
export const SCRIPT_MAX_AS_MB = Number(process.env.OFFICE_SCRIPT_MAX_AS_MB) || 1024;
// RLIMIT_FSIZE: largest single file a script may write (outputs are capped at
// the broker's 10 MiB anyway). It bounds one file, not the job's total.
const SCRIPT_MAX_FILE_BYTES = 64 << 20;
// RLIMIT_NPROC counts every task of the uid, host-wide, so it cannot be a
// per-job cap; it is a fork-bomb backstop for a runner without compose's
// pids_limit (512), set high enough not to trip on a busy dev host.
const SCRIPT_MAX_PROCS = 1024;

// Bytes of each child stream kept in memory. Everything past it is drained
// and dropped (never left in the pipe, or the child would block), keeping the
// tail — where a traceback is.
export const OUTPUT_CAPTURE_BYTES = 16 << 10;

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

function appendCapped(buf, chunk) {
  const next = buf + chunk;
  return next.length > OUTPUT_CAPTURE_BYTES ? next.slice(-OUTPUT_CAPTURE_BYTES) : next;
}

// Runs cmd/args to completion under cwd via the injected spawn, capturing
// the last OUTPUT_CAPTURE_BYTES of stdout/stderr. Rejects with ScriptError on
// non-zero exit or spawn failure — stderr (falling back to stdout) rides the
// error message for diagnostics.
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
    child.stdout.on('data', (chunk) => (stdout = appendCapped(stdout, chunk)));
    child.stderr.on('data', (chunk) => (stderr = appendCapped(stderr, chunk)));
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
  // The V8 flags must precede the script path — node stops parsing its own
  // options at the first positional argument. --v8-pool-size pins V8's
  // worker-thread count so its address-space reservation (and so the
  // RLIMIT_AS headroom) does not grow with the host's core count.
  return runCommand({
    spawn,
    ...limited(NODE_MAX_OLD_SPACE_MB + SCRIPT_MAX_AS_MB, process.execPath, [
      `--max-old-space-size=${NODE_MAX_OLD_SPACE_MB}`,
      '--v8-pool-size=2',
      scriptPath,
      ...args,
    ]),
    cwd: jobDir,
  });
}

// Writes a model-authored python script into jobDir and runs it under the
// worker's uv-managed venv interpreter (pinned openpyxl/reportlab/etc).
export async function runPythonScript({ jobDir, spawn, script, filename = 'script.py', args = [] }) {
  const scriptPath = join(jobDir, filename);
  await writeFile(scriptPath, script, 'utf8');
  return runCommand({ spawn, ...limited(SCRIPT_MAX_AS_MB, VENV_PYTHON, [scriptPath, ...args]), cwd: jobDir });
}

// Wraps a model-authored script's interpreter in prlimit (util-linux, in the
// base image). Fixed helpers and soffice are not wrapped: they are not
// attacker code and legitimately need more address space. A missing prlimit
// fails the op (spawn error) rather than running the script unbounded.
function limited(asMb, cmd, args) {
  return {
    cmd: 'prlimit',
    args: [
      `--as=${asMb * 1024 * 1024}`,
      `--fsize=${SCRIPT_MAX_FILE_BYTES}`,
      `--nproc=${SCRIPT_MAX_PROCS}`,
      '--',
      cmd,
      ...args,
    ],
  };
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
//
// The lexical check above is not enough: a script can leave a symlink (or a
// symlinked parent dir) pointing at another job's input or /proc/1/environ.
// O_NOFOLLOW refuses a final-component symlink, O_NONBLOCK keeps a planted
// FIFO from hanging the open, and the opened file's real path (read back
// from the fd, so a dir swapped after the open cannot fool it) must sit
// under jobDir's real path.
export async function readJobFile(jobDir, filename, missingMessage, paramName = 'output_filename') {
  assertPathWithinJobDir(jobDir, filename, paramName);
  const path = join(jobDir, filename);
  let fh;
  try {
    fh = await open(path, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
  } catch (err) {
    if (err.code === 'ENOENT') throw new ScriptError(missingMessage);
    if (err.code === 'ELOOP') throw new ScriptError(`"${filename}" is a symlink; refusing to read it`);
    throw err;
  }
  try {
    const [real, root] = await Promise.all([
      readlink(`/proc/self/fd/${fh.fd}`).catch(() => realpath(path)),
      realpath(jobDir),
    ]);
    if (!real.startsWith(root + sep) || !(await fh.stat()).isFile()) {
      throw new ScriptError(`"${filename}" is not a regular file inside the job directory`);
    }
    return await fh.readFile();
  } finally {
    await fh.close();
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
