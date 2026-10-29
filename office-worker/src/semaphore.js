// Simple counting semaphore bounding job concurrency (OFFICE_MAX_CONCURRENCY).
// Excess acquire() calls queue in FIFO order and resolve as slots free up. At
// most maxQueue callers wait; past that acquire() rejects with QueueFullError
// (503) instead of piling up requests a busy worker can never serve in time.
export class QueueFullError extends Error {
  constructor(maxQueue) {
    super(`office-worker is busy (${maxQueue} jobs already queued); retry later`);
    this.name = 'QueueFullError';
    this.statusCode = 503;
  }
}

export function createSemaphore(max, maxQueue = Infinity) {
  let active = 0;
  const queue = [];

  function acquire() {
    if (active < max) {
      active++;
      return Promise.resolve();
    }
    if (queue.length >= maxQueue) return Promise.reject(new QueueFullError(maxQueue));
    return new Promise((resolve) => queue.push(resolve));
  }

  function release() {
    active--;
    const next = queue.shift();
    if (next) {
      active++;
      next();
    }
  }

  return { acquire, release };
}
