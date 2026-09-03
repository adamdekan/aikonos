// Tests for runWorkflowStream (api/workflows.js) — the fetch-SSE run parser.
// WHY: the modal's live progress depends on this correctly splitting the SSE
// body into `step` events and one terminal `result` event, tolerating chunk
// boundaries mid-frame and ignoring non-data frames (retry:). Follows the
// injected-fetchFn/getTokenFn precedent in agui-abort.test.js.
import { describe, it, expect, vi } from "vitest";
import { runWorkflowStream } from "../api/workflows.js";

const testToken = async () => "test-bearer-token";

// Builds a ReadableStream that emits the given text in the given chunk sizes so
// a frame can be split across two reads (exercises the buffer stitching).
function sseBody(text, chunkSize = text.length) {
  const bytes = new TextEncoder().encode(text);
  let off = 0;
  return new ReadableStream({
    pull(c) {
      if (off >= bytes.length) { c.close(); return; }
      c.enqueue(bytes.slice(off, off + chunkSize));
      off += chunkSize;
    },
  });
}

const SSE =
  "retry: 3000\n\n" +
  'event: step\ndata: {"index":0,"skill":"web.fetch","ok":true}\n\n' +
  'event: step\ndata: {"index":1,"skill":"doc.write","ok":false,"denyReason":"not permitted"}\n\n' +
  'event: result\ndata: {"ok":true,"result":{"halted":false,"steps":[]}}\n\n';

describe("runWorkflowStream", () => {
  it("dispatches one onStep per step event and returns the result payload", async () => {
    const steps = [];
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, body: sseBody(SSE) });

    const result = await runWorkflowStream(
      "l1", { q: "x" },
      { onStep: (s) => steps.push(s) },
      fetchMock, testToken,
    );

    expect(steps).toEqual([
      { index: 0, skill: "web.fetch", ok: true },
      { index: 1, skill: "doc.write", ok: false, denyReason: "not permitted" },
    ]);
    expect(result).toEqual({ ok: true, result: { halted: false, steps: [] } });

    // POSTs the streaming variant with the inputs wrapped.
    const [url, opts] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/workflows/l1/run?stream=1");
    expect(opts.method).toBe("POST");
    expect(JSON.parse(opts.body)).toEqual({ inputs: { q: "x" } });
  });

  it("stitches frames split across chunk boundaries", async () => {
    const steps = [];
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, body: sseBody(SSE, 7) });
    const result = await runWorkflowStream("l1", {}, { onStep: (s) => steps.push(s) }, fetchMock, testToken);
    expect(steps.length).toBe(2);
    expect(result.ok).toBe(true);
  });

  it("throws when the stream yields no result frame (fallback trigger)", async () => {
    const noResult =
      'event: step\ndata: {"index":0,"skill":"web.fetch","ok":true}\n\n';
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, body: sseBody(noResult) });
    await expect(runWorkflowStream("l1", {}, {}, fetchMock, testToken)).rejects.toThrow(/no result/);
  });

  it("throws on a non-ok response (fallback trigger)", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: false, status: 502, body: null });
    await expect(runWorkflowStream("l1", {}, {}, fetchMock, testToken)).rejects.toThrow(/502/);
  });
});
