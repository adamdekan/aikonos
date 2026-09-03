// Per-job lifecycle: a unique tmpfs dir, a wall-clock timeout that kills any
// subprocess group the handler spawned, and a guaranteed wipe of the dir.
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawn as nodeSpawn } from 'node:child_process';

const JOB_DIR_PREFIX = 'office-job-';

export class JobTimeoutError extends Error {
  constructor(timeoutMs) {
    super(`job exceeded ${timeoutMs}ms timeout`);
    this.name = 'JobTimeoutError';
    this.statusCode = 504;
  }
}

// Creates a job dir under root (default OS tmpdir, which is the tmpfs /tmp
// mount in the container), runs fn(jobDir), and always wipes the dir after —
// including when fn throws.
export async function withJobDir(fn, { root = tmpdir() } = {}) {
  const dir = await mkdtemp(join(root, JOB_DIR_PREFIX));
  try {
    return await fn(dir);
  } finally {
    await rm(dir, { recursive: true, force: true }).catch(() => {});
  }
}

// Runs handler({ spawn }) with a wall-clock timeout. handler must use the
// injected `spawn` (not node:child_process directly) for any subprocess it
// starts, so the runner can kill the whole process group on timeout — a
// plain child.kill() leaves grandchildren (e.g. soffice's real worker
// process) running.
export function createJobRunner({ timeoutMs }) {
  const children = new Set();

  function trackedSpawn(...args) {
    const child = nodeSpawn(...withDetached(args));
    children.add(child);
    child.once('exit', () => children.delete(child));
    return child;
  }

  function killAll() {
    for (const child of children) {
      if (child.pid == null) continue;
      try {
        // Negative pid == the process group (detached: true made the child
        // its own group leader), killing it and anything it forked.
        process.kill(-child.pid, 'SIGKILL');
      } catch {
        try {
          child.kill('SIGKILL');
        } catch {
          // already dead
        }
      }
    }
  }

  async function run(handler) {
    let timer;
    let timedOut = false;
    const timeout = new Promise((_, reject) => {
      timer = setTimeout(() => {
        timedOut = true;
        killAll();
        reject(new JobTimeoutError(timeoutMs));
      }, timeoutMs);
    });
    try {
      return await Promise.race([handler({ spawn: trackedSpawn }), timeout]);
    } finally {
      clearTimeout(timer);
      if (timedOut) killAll();
    }
  }

  return { run };
}

function withDetached(args) {
  const opts = { ...(args[2] || {}), detached: true };
  return [args[0], args[1], opts];
}
