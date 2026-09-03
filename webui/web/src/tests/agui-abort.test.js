import { describe, it, expect, vi } from "vitest";
import { runAgui } from "../api/agui.js";

const testToken = async () => "test-bearer-token";

describe("runAgui — signal passthrough", () => {
  it("passes signal to fetchFn as part of fetch options", async () => {
    const ac = new AbortController();
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      body: new ReadableStream({ start(c) { c.close(); } }),
    });
    await runAgui({ prompt: "hi", threadId: "t1", signal: ac.signal }, {}, fetchMock, testToken);
    const [, opts] = fetchMock.mock.calls[0];
    expect(opts.signal).toBe(ac.signal);
  });

  it("resolves without calling onError when fetch itself throws AbortError (abort before response)", async () => {
    const ac = new AbortController();
    const onError = vi.fn();
    const handlers = { onError };

    // Simulate the browser behaviour when fetch is aborted before a response arrives:
    // the fetch promise rejects with a DOMException whose name is "AbortError".
    const abortErr = new DOMException("The user aborted a request.", "AbortError");
    const fetchMock = vi.fn().mockRejectedValue(abortErr);

    const result = await runAgui({ prompt: "hi", threadId: "t1", signal: ac.signal }, handlers, fetchMock, testToken);

    // Must return the threadId, not throw.
    expect(typeof result).toBe("string");
    expect(result).toBeTruthy();

    // Must NOT have called onError — abort is user-initiated, not an error.
    expect(onError).not.toHaveBeenCalled();
  });

  it("resolves without calling onError when stream read throws AbortError (abort mid-stream)", async () => {
    const ac = new AbortController();
    const onError = vi.fn();
    const handlers = { onError };

    // Simulate abort during the reader.read() loop: the stream reader
    // throws a DOMException("AbortError") after we abort.
    // We use a deferred promise pattern: read() returns a promise whose
    // reject function is exposed so the test can trigger it after runAgui
    // has entered the read loop.
    let rejectRead;
    const readPromise = new Promise((_res, rej) => { rejectRead = rej; });

    let readCallCount = 0;
    const hangingBody = {
      getReader() {
        return {
          read() {
            readCallCount++;
            // First read hangs (simulates waiting for data); subsequent reads
            // resolve as done so runAgui exits cleanly if the abort path is taken.
            if (readCallCount === 1) return readPromise;
            return Promise.resolve({ value: undefined, done: true });
          },
          releaseLock() {},
        };
      },
    };

    const fetchMock = vi.fn().mockResolvedValue({ ok: true, body: hangingBody });

    const promise = runAgui({ prompt: "hi", threadId: "t1", signal: ac.signal }, handlers, fetchMock, testToken);

    // Yield so runAgui enters the read() loop and the first read() is awaited.
    await Promise.resolve();
    await Promise.resolve();

    // Reject the pending read() with an AbortError (mirrors real browser behaviour).
    rejectRead(new DOMException("The user aborted a request.", "AbortError"));

    const result = await promise;

    expect(typeof result).toBe("string");
    expect(result).toBeTruthy();
    expect(onError).not.toHaveBeenCalled();
  });

  it("still calls onError for non-abort fetch failures", async () => {
    const onError = vi.fn();
    const fetchMock = vi.fn().mockResolvedValue({ ok: false, status: 500 });
    await runAgui({ prompt: "hi", threadId: "t1" }, { onError }, fetchMock, testToken);
    expect(onError).toHaveBeenCalledWith("request failed (500)");
  });
});
