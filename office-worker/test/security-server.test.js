// Regression tests for the p6 security fixes that live in the server, job
// runner and run-script plumbing (finding ids in each test name). Stub
// handlers only — the real handlers are covered in security-handlers.test.js.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { spawn as nodeSpawn } from 'node:child_process';
import { mkdtemp, mkdir, rm, symlink, writeFile, readFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { startServer, post, get, buildMultipartRequest } from '../test-support/helpers.js';
import { registerHandler, unregisterHandler } from '../src/ops.js';
import { createJobRunner } from '../src/job.js';
import { assertTokenConfigured, TOKEN_HEADER } from '../src/server.js';
import { OUTPUT_CAPTURE_BYTES, ScriptError, readJobFile, runCommand } from '../src/handlers/run-script.js';

async function withStub(handler, serverOpts, fn) {
  registerHandler('pdf/create', handler);
  const { baseUrl, close } = await startServer(serverOpts);
  try {
    return await fn(baseUrl);
  } finally {
    unregisterHandler('pdf/create');
    await close();
  }
}

const okHandler = async () => ({ outputs: [], result: { ok: true } });

function postWithHeaders(baseUrl, path, headers, body) {
  return new Promise((resolve, reject) => {
    const req = http.request(new URL(path, baseUrl), { method: 'POST', headers }, (res) => {
      const chunks = [];
      res.on('data', (c) => chunks.push(c));
      res.on('end', () => resolve({ status: res.statusCode, body: Buffer.concat(chunks) }));
    });
    req.on('error', reject);
    req.end(body);
  });
}

// ---------------------------------------------------------------- F84 (M-29)

test('F84: a job request without the shared token is 401 and never reaches the handler', async () => {
  let calls = 0;
  await withStub(
    async () => {
      calls++;
      return { outputs: [], result: {} };
    },
    { token: 'office-secret' },
    async (baseUrl) => {
      const { contentType, body } = buildMultipartRequest([], {});
      const missing = await post(baseUrl, '/v1/pdf/create', contentType, body);
      assert.equal(missing.status, 401);
      const wrong = await postWithHeaders(
        baseUrl,
        '/v1/pdf/create',
        { 'Content-Type': contentType, [TOKEN_HEADER]: 'office-secreT' },
        body
      );
      assert.equal(wrong.status, 401);
      const right = await postWithHeaders(
        baseUrl,
        '/v1/pdf/create',
        { 'Content-Type': contentType, [TOKEN_HEADER]: 'office-secret' },
        body
      );
      assert.equal(right.status, 200, right.body.toString());
      assert.equal(calls, 1);
    }
  );
});

test('F84: /healthz stays reachable without the token', async () => {
  const { baseUrl, close } = await startServer({ token: 'office-secret' });
  try {
    assert.equal((await get(baseUrl, '/healthz')).status, 200);
  } finally {
    await close();
  }
});

test('F84: the worker refuses to start without a token unless AIKONOS_DEV_MODE=true', () => {
  assert.throws(() => assertTokenConfigured({}), /AIKONOS_OFFICE_WORKER_TOKEN/);
  assert.throws(() => assertTokenConfigured({ AIKONOS_DEV_MODE: 'false' }), /refusing to start/);
  assert.doesNotThrow(() => assertTokenConfigured({ AIKONOS_DEV_MODE: 'true' }));
  assert.doesNotThrow(() => assertTokenConfigured({ AIKONOS_OFFICE_WORKER_TOKEN: 'x' }));
});

// ---------------------------------------------------------------- F87 (L-31)

test('F87: params that is not a JSON object is a 400, not a 500 TypeError', async () => {
  await withStub(okHandler, {}, async (baseUrl) => {
    for (const raw of ['null', '[]', '42']) {
      const boundary = 'b';
      const body = Buffer.from(
        `--${boundary}\r\nContent-Disposition: form-data; name="params"\r\n\r\n${raw}\r\n--${boundary}--\r\n`
      );
      const res = await post(baseUrl, '/v1/pdf/create', `multipart/form-data; boundary=${boundary}`, body);
      assert.equal(res.status, 400, `params ${raw}`);
      assert.match(JSON.parse(res.body).error, /params must be a JSON object/);
    }
  });
});

// ---------------------------------------------------------------- F83 (M-31)

test('F83: a stalled request body is cut off (408) without holding the only job slot', async () => {
  await withStub(okHandler, { maxConcurrency: 1, bodyTimeoutMs: 300 }, async (baseUrl) => {
    // Promises a body it never sends.
    const stalled = new Promise((resolve) => {
      const req = http.request(new URL('/v1/pdf/create', baseUrl), {
        method: 'POST',
        headers: { 'Content-Type': 'multipart/form-data; boundary=x', 'Content-Length': 1000 },
      });
      req.on('response', (res) => resolve(res.statusCode));
      req.on('error', () => resolve('reset'));
      req.write('--x\r\n');
    });

    // The one slot is free while the stalled body is still outstanding.
    const { contentType, body } = buildMultipartRequest([], {});
    const res = await post(baseUrl, '/v1/pdf/create', contentType, body);
    assert.equal(res.status, 200, res.body.toString());

    const outcome = await stalled;
    assert.ok(outcome === 408 || outcome === 'reset', `stalled request ended with ${outcome}`);
  });
});

test('F83: a full wait queue answers 503 instead of queueing without bound', async () => {
  let release;
  const gate = new Promise((r) => (release = r));
  await withStub(
    async () => {
      await gate;
      return { outputs: [], result: {} };
    },
    { maxConcurrency: 1, maxQueue: 0 },
    async (baseUrl) => {
      const { contentType, body } = buildMultipartRequest([], {});
      const first = post(baseUrl, '/v1/pdf/create', contentType, body);
      await new Promise((r) => setTimeout(r, 100));
      const second = await post(baseUrl, '/v1/pdf/create', contentType, body);
      assert.equal(second.status, 503);
      release();
      assert.equal((await first).status, 200);
    }
  );
});

test('F83: request bodies past the buffered-bytes budget get 503 before they are read', async () => {
  let release;
  const gate = new Promise((r) => (release = r));
  const big = [{ name: 'file', filename: 'a.pdf', buffer: Buffer.alloc(600 << 10) }];
  await withStub(
    async () => {
      await gate;
      return { outputs: [], result: {} };
    },
    { maxConcurrency: 1, maxBufferedBytes: 1 << 20 },
    async (baseUrl) => {
      const { contentType, body } = buildMultipartRequest(big, {});
      const first = post(baseUrl, '/v1/pdf/create', contentType, body);
      await new Promise((r) => setTimeout(r, 100));
      // The first body is still held (its job is running); a second would
      // take the total past the budget, so it is refused, not queued.
      const second = await post(baseUrl, '/v1/pdf/create', contentType, body);
      assert.equal(second.status, 503, second.body.toString());
      release();
      assert.equal((await first).status, 200);
      // The budget is given back when a job ends.
      assert.equal((await post(baseUrl, '/v1/pdf/create', contentType, body)).status, 200);
    }
  );
});

// ---------------------------------------------------------------- F79 (M-30)

function isGone(pid) {
  try {
    process.kill(pid, 0);
  } catch {
    return true;
  }
  // Killed but not yet reaped by its new parent still counts as gone.
  return readFile(`/proc/${pid}/stat`, 'utf8').then(
    (stat) => stat.split(') ')[1]?.startsWith('Z'),
    () => true
  );
}

test('F79: a setsid-escaped background process does not outlive its job', async () => {
  const runner = createJobRunner({ timeoutMs: 10000 });
  const { stdout } = await runner.run(({ spawn }) =>
    runCommand({
      spawn,
      cmd: 'python3',
      args: ['-c', "import subprocess;print(subprocess.Popen(['sleep','30'],start_new_session=True).pid)"],
      cwd: tmpdir(),
    })
  );
  const pid = Number(stdout.trim());
  assert.ok(pid > 0, `expected a pid, got ${JSON.stringify(stdout)}`);
  await new Promise((r) => setTimeout(r, 100));
  assert.ok(await isGone(pid), `escaped process ${pid} is still running after the job`);
});

// The script still runs as the worker's uid, so it can read the server's own
// /proc/<pid>/environ: this pins only what spawn hands it (residual in
// API.md "Job isolation").
test('F79: a job subprocess gets a minimal env tagged with the job id, without the worker secret', async () => {
  const before = process.env.AIKONOS_OFFICE_WORKER_TOKEN;
  process.env.AIKONOS_OFFICE_WORKER_TOKEN = 'must-not-leak';
  try {
    const runner = createJobRunner({ timeoutMs: 10000 });
    const { stdout } = await runner.run(({ spawn }) => runCommand({ spawn, cmd: 'env', args: [], cwd: tmpdir() }));
    assert.doesNotMatch(stdout, /must-not-leak/);
    assert.match(stdout, /^AIKONOS_JOB_ID=[0-9a-f-]{36}$/m);
  } finally {
    if (before === undefined) delete process.env.AIKONOS_OFFICE_WORKER_TOKEN;
    else process.env.AIKONOS_OFFICE_WORKER_TOKEN = before;
  }
});

// Pinned residual (API.md "Job isolation"): the sweep finds leftovers by the
// job id in their environment, and a process that execs with an empty one
// escapes it. Closing it needs a per-job PID namespace.
test(
  'F79: a process that clears its environment does not outlive its job',
  { todo: 'shared uid: needs a per-job PID namespace sandbox' },
  async () => {
    const runner = createJobRunner({ timeoutMs: 10000 });
    const { stdout } = await runner.run(({ spawn }) =>
      runCommand({
        spawn,
        cmd: 'python3',
        args: [
          '-c',
          "import subprocess,time;p=subprocess.Popen(['env','-i','sleep','30'],start_new_session=True);time.sleep(0.2);print(p.pid)",
        ],
        cwd: tmpdir(),
      })
    );
    const pid = Number(stdout.trim());
    try {
      await new Promise((r) => setTimeout(r, 100));
      assert.ok(await isGone(pid), `env -i process ${pid} is still running after the job`);
    } finally {
      try {
        process.kill(pid, 'SIGKILL');
      } catch {
        // gone
      }
    }
  }
);

test('F79: the post-job sweep holds one environ at a time, not every process environ at once', async () => {
  // Untagged leftovers with ~1.8 MiB environments, as a script could leave
  // behind: a parallel scan held all of them (as Buffer and string) at once.
  const env = { PATH: process.env.PATH };
  for (let i = 0; i < 15; i++) env[`PAD${i}`] = 'x'.repeat(120 << 10);
  const leftovers = Array.from({ length: 60 }, () => nodeSpawn('sleep', ['30'], { env, stdio: 'ignore' }));
  try {
    await new Promise((r) => setTimeout(r, 300));
    const before = process.resourceUsage().maxRSS;
    await createJobRunner({ timeoutMs: 10000 }).run(async () => {});
    const grownMb = (process.resourceUsage().maxRSS - before) / 1024;
    assert.ok(grownMb < 120, `the sweep raised peak RSS by ${grownMb.toFixed(0)} MB`);
  } finally {
    for (const p of leftovers) p.kill('SIGKILL');
  }
});

test('F79: readJobFile refuses a symlink (final component or parent dir) out of the job dir', async () => {
  const jobDir = await mkdtemp(join(tmpdir(), 'office-worker-test-'));
  const otherJob = await mkdtemp(join(tmpdir(), 'office-worker-other-'));
  try {
    await writeFile(join(otherJob, 'input.docx'), 'another tenant');
    await symlink(join(otherJob, 'input.docx'), join(jobDir, 'output.docx'));
    await assert.rejects(readJobFile(jobDir, 'output.docx', 'missing'), ScriptError);

    await symlink(otherJob, join(jobDir, 'out'));
    await assert.rejects(readJobFile(jobDir, 'out/input.docx', 'missing'), ScriptError);

    await mkdir(join(jobDir, 'real'));
    await writeFile(join(jobDir, 'real', 'ok.docx'), 'mine');
    assert.equal((await readJobFile(jobDir, 'real/ok.docx', 'missing')).toString(), 'mine');
  } finally {
    await rm(jobDir, { recursive: true, force: true });
    await rm(otherJob, { recursive: true, force: true });
  }
});

// ---------------------------------------------------------------- F81

test('F81: child output is captured only up to a bounded tail', async () => {
  const runner = createJobRunner({ timeoutMs: 20000 });
  const err = await runner
    .run(({ spawn }) =>
      runCommand({
        spawn,
        cmd: process.execPath,
        args: [
          '-e',
          "for (let i = 0; i < 200; i++) process.stderr.write('x'.repeat(100000));" +
            "process.stderr.write('TAIL'); process.exitCode = 1",
        ],
        cwd: tmpdir(),
      })
    )
    .catch((e) => e);
  assert.ok(err instanceof ScriptError);
  assert.ok(err.message.length < OUTPUT_CAPTURE_BYTES + 1024, `error message is ${err.message.length} chars`);
  assert.match(err.message, /TAIL$/, 'the tail (where a traceback is) is kept');
});
