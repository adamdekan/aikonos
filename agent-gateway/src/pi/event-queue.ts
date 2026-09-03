// Bounded async queue — bridges a synchronous push() to an async generator consumer.
//
// WHY this exists: the inline queue arrays in core.ts and child-entry.ts were
// unbounded. A slow or disconnected SSE consumer causes unlimited memory growth.
// This helper caps the backlog at maxSize; exceeding it is fail-closed: the
// next consumer pull throws an overflow error, so the caller can surface it as
// a terminal error event rather than silently growing memory.

export interface EventQueueOptions {
  maxSize?: number;
}

const DEFAULT_MAX_SIZE = 1024;

// Sentinel types carried in the internal buffer alongside real items.
type CloseSentinel = { readonly __close: true };
type OverflowSentinel = { readonly __overflow: true };

const CLOSE: CloseSentinel = { __close: true };
const OVERFLOW: OverflowSentinel = { __overflow: true };

type Slot<T> = T | CloseSentinel | OverflowSentinel;

function isClose<T>(s: Slot<T>): s is CloseSentinel {
  return typeof s === "object" && s !== null && "__close" in s;
}

function isOverflow<T>(s: Slot<T>): s is OverflowSentinel {
  return typeof s === "object" && s !== null && "__overflow" in s;
}

export class EventQueue<T> {
  private readonly maxSize: number;
  private readonly buf: Slot<T>[] = [];
  private notify: (() => void) | null = null;
  private overflowed = false;
  private closed = false;
  // disposed is set by generator return() so subsequent pushes are no-ops.
  private disposed = false;

  constructor(opts: EventQueueOptions = {}) {
    this.maxSize = opts.maxSize ?? DEFAULT_MAX_SIZE;
  }

  push(item: T): void {
    if (this.disposed || this.overflowed || this.closed) return;

    if (this.buf.length >= this.maxSize) {
      // Fail closed: push the overflow sentinel and enter overflowed state.
      // Subsequent pushes are no-ops (guard above).
      this.overflowed = true;
      this.buf.push(OVERFLOW);
      this.wake();
      return;
    }

    this.buf.push(item);
    this.wake();
  }

  close(): void {
    if (this.disposed || this.closed) return;
    this.closed = true;
    this.buf.push(CLOSE);
    this.wake();
  }

  // size() is exposed for testability — callers must not rely on it for flow control.
  size(): number {
    return this.buf.length;
  }

  async *drain(): AsyncGenerator<T> {
    try {
      while (true) {
        // WHY safe-wait ordering: assign this.notify (creating the wakeup slot)
        // BEFORE testing buf.length. Any push() that runs between the moment
        // the Promise executor fires and when we actually await will call wake(),
        // setting notify=null and resolving the promise immediately — no lost
        // wakeup. The old form (check buf first, then assign notify) left a
        // window where push could wake() a null notify and the await would hang.
        const wait = new Promise<void>((resolve) => { this.notify = resolve; });
        if (this.buf.length > 0) {
          // Buffer already has items — cancel the wakeup slot and proceed.
          this.notify = null;
        } else {
          await wait;
        }

        const slot = this.buf.shift();
        if (slot === undefined) continue;

        if (isClose(slot)) return;

        if (isOverflow(slot)) {
          throw new Error("EventQueue overflow: consumer too slow, backlog exceeded maxSize");
        }

        yield slot;
      }
    } finally {
      // Signal early-stop so push() becomes a no-op after consumer exits.
      this.disposed = true;
      this.notify = null;
    }
  }

  private wake(): void {
    if (this.notify) {
      const fn = this.notify;
      this.notify = null;
      fn();
    }
  }
}
