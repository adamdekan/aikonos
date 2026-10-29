// Per-job lifecycle: a unique tmpfs dir, a wall-clock timeout that kills any
// subprocess group the handler spawned, a post-job sweep of every process the
// job left behind, and a guaranteed wipe of the dir.
import { mkdtemp, readdir, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import { setTimeout as sleep } from 'node:timers/promises';
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
// including when fn throws. mkdtemp creates it 0700, which keeps other uids
// out but not a concurrent job's script: every job runs as the same uid.
export async function withJobDir(fn, { root = tmpdir() } = {}) {
  const dir = await mkdtemp(join(root, JOB_DIR_PREFIX));
  try {
    return await fn(dir);
  } finally {
    await rm(dir, { recursive: true, force: true }).catch(() => {});
  }
}

// Only these keys of the worker's own environment reach a subprocess, so a
// script does not inherit AIKONOS_OFFICE_WORKER_TOKEN through its env. That
// is not secrecy: a script runs as the worker's uid and can read the server's
// (and tini's) /proc/<pid>/environ directly — the shared-uid residual in
// API.md "Job isolation", closed only by a per-job sandbox.
// OPENBLAS_NUM_THREADS=1 keeps numpy's address-space footprint independent of
// the host's core count, so run-script.js's RLIMIT_AS fits on any host.
const PASSTHROUGH_ENV = ['PATH', 'HOME', 'LANG', 'LC_ALL', 'TZ', 'TMPDIR'];
const SWEEP_ROUNDS = 20;

function jobEnv(extra, jobId) {
  const env = { OPENBLAS_NUM_THREADS: '1' };
  for (const key of PASSTHROUGH_ENV) {
    if (process.env[key] !== undefined) env[key] = process.env[key];
  }
  return { ...env, ...extra, AIKONOS_JOB_ID: jobId };
}

// Pids of every live process whose environment carries tag. setsid/nohup
// leave the job's process group, but they keep the environment they were
// started with. Linux only: without /proc there is nothing to scan and the
// tracked groups are the only kill.
//
// One environ in memory at a time, searched as raw bytes: this runs in the
// shared server after every job, and a script can leave hundreds of untagged
// processes with ~2 MiB environments behind. ponytail: sequential scan, a
// few ms per round on a container's pid count.
async function taggedPids(tag) {
  let entries;
  try {
    entries = await readdir('/proc');
  } catch {
    return [];
  }
  const first = Buffer.from(`${tag}\0`);
  const inner = Buffer.from(`\0${tag}\0`);
  const pids = [];
  for (const e of entries) {
    if (!/^\d+$/.test(e) || Number(e) === process.pid) continue;
    let env;
    try {
      env = await readFile(`/proc/${e}/environ`);
    } catch {
      continue; // exited, or another uid's process
    }
    if (env.subarray(0, first.length).equals(first) || env.includes(inner)) pids.push(Number(e));
  }
  return pids;
}

// Runs handler({ spawn }) with a wall-clock timeout. handler must use the
// injected `spawn` (not node:child_process directly) for any subprocess it
// starts, so the runner can kill the whole process group on timeout — a
// plain child.kill() leaves grandchildren (e.g. soffice's real worker
// process) running.
//
// Every spawned process carries AIKONOS_JOB_ID=<random id> in its env. When
// the job ends — success, error or timeout — the runner SIGKILLs every
// tracked group and then every process whose environment still carries that
// id, so a script's `setsid sleep &` cannot outlive the job and watch the
// next tenant's documents. ponytail: env tagging, not a sandbox — a process
// that clears its own environ (exec env -i, prctl non-dumpable) escapes the
// sweep; a per-job PID namespace (bwrap/nsjail) is the upgrade path.
export function createJobRunner({ timeoutMs }) {
  const children = new Set();
  const jobId = randomUUID();
  const tag = `AIKONOS_JOB_ID=${jobId}`;

  function trackedSpawn(cmd, args, opts = {}) {
    const child = nodeSpawn(cmd, args, { ...opts, detached: true, env: jobEnv(opts.env, jobId) });
    children.add(child);
    // Dropped on exit so a recycled pid is never group-killed; anything the
    // leader left running still carries the tag and the sweep finds it.
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

  async function sweep() {
    killAll();
    // Repeat until a scan comes back empty: a process forked between a scan
    // and its parent's kill still carries the tag and is caught next round.
    for (let round = 0; round < SWEEP_ROUNDS; round++) {
      const pids = await taggedPids(tag);
      if (pids.length === 0) return;
      for (const pid of pids) {
        try {
          process.kill(pid, 'SIGKILL');
        } catch {
          // already gone
        }
      }
      await sleep(10);
    }
  }

  async function run(handler) {
    let timer;
    const timeout = new Promise((_, reject) => {
      timer = setTimeout(() => {
        killAll();
        reject(new JobTimeoutError(timeoutMs));
      }, timeoutMs);
    });
    try {
      return await Promise.race([handler({ spawn: trackedSpawn }), timeout]);
    } finally {
      clearTimeout(timer);
      await sweep();
    }
  }

  return { run };
}
