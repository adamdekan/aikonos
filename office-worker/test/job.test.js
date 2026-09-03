import { test } from 'node:test';
import assert from 'node:assert/strict';
import { existsSync } from 'node:fs';
import { withJobDir, createJobRunner, JobTimeoutError } from '../src/job.js';

test('withJobDir creates a unique dir and wipes it after a successful run', async () => {
  let capturedDir;
  await withJobDir(async (dir) => {
    capturedDir = dir;
    assert.ok(existsSync(dir), 'job dir should exist while the handler runs');
  });
  assert.ok(!existsSync(capturedDir), 'job dir should be wiped after the handler returns');
});

test('withJobDir wipes the dir even when the handler throws', async () => {
  let capturedDir;
  await assert.rejects(
    withJobDir(async (dir) => {
      capturedDir = dir;
      throw new Error('boom');
    }),
    /boom/
  );
  assert.ok(!existsSync(capturedDir), 'job dir should be wiped even on error');
});

test('two calls to withJobDir get distinct directories', async () => {
  const dirs = [];
  await withJobDir(async (dir) => dirs.push(dir));
  await withJobDir(async (dir) => dirs.push(dir));
  assert.notEqual(dirs[0], dirs[1]);
});

test('createJobRunner rejects with JobTimeoutError once the wall clock expires', async () => {
  const runner = createJobRunner({ timeoutMs: 30 });
  await assert.rejects(
    runner.run(() => new Promise(() => {})), // never resolves
    JobTimeoutError
  );
});

test('createJobRunner kills a spawned subprocess group on timeout', async () => {
  const runner = createJobRunner({ timeoutMs: 50 });
  let child;
  await assert.rejects(
    runner.run(
      ({ spawn }) =>
        new Promise((resolve) => {
          // sleep well past the timeout; if the group kill doesn't happen this
          // process (and its "sleep" child) would still be running after run().
          child = spawn('sh', ['-c', 'sleep 5'], { stdio: 'ignore' });
          child.on('exit', resolve);
        })
    ),
    JobTimeoutError
  );
  // Give the OS a beat to reap the killed process, then confirm it's gone.
  await new Promise((r) => setTimeout(r, 100));
  assert.ok(child.killed || child.exitCode !== null || child.signalCode !== null);
});
