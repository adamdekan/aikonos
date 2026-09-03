// Simple counting semaphore bounding job concurrency (OFFICE_MAX_CONCURRENCY).
// Excess acquire() calls queue in FIFO order and resolve as slots free up.
export function createSemaphore(max) {
  let active = 0;
  const queue = [];

  function acquire() {
    if (active < max) {
      active++;
      return Promise.resolve();
    }
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
