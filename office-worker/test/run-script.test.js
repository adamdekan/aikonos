// Unit coverage for the write-path boundary guard — writeJobFile must reject a
// caller-influenced filename that would escape jobDir, same as readJobFile.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { writeJobFile, HandlerInputError } from '../src/handlers/run-script.js';

test('writeJobFile rejects a filename that escapes the job dir', async () => {
  const jobDir = await mkdtemp(join(tmpdir(), 'office-worker-test-'));
  try {
    await assert.rejects(
      writeJobFile(jobDir, '../../../../tmp/evil', Buffer.from('x')),
      HandlerInputError
    );
  } finally {
    await rm(jobDir, { recursive: true, force: true });
  }
});

test('writeJobFile writes a legitimate filename inside the job dir', async () => {
  const jobDir = await mkdtemp(join(tmpdir(), 'office-worker-test-'));
  try {
    const path = await writeJobFile(jobDir, 'input.docx', Buffer.from('hello'));
    assert.equal(await readFile(path, 'utf8'), 'hello');
  } finally {
    await rm(jobDir, { recursive: true, force: true });
  }
});
