import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createSemaphore } from '../src/semaphore.js';

test('semaphore admits up to max concurrently and queues the rest', async () => {
  const sem = createSemaphore(2);
  let active = 0;
  let maxSeen = 0;

  async function job() {
    await sem.acquire();
    active++;
    maxSeen = Math.max(maxSeen, active);
    await new Promise((r) => setTimeout(r, 20));
    active--;
    sem.release();
  }

  await Promise.all([job(), job(), job(), job()]);
  assert.equal(maxSeen, 2);
});
