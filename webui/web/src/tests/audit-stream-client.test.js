// api/auditStream.js: the audit SSE is read over fetch so the bearer is sent
// (H-07: the gateway scopes the stream to the caller and refuses anonymous).
import { describe, it, expect, vi, afterEach } from "vitest";

vi.mock("../auth/oidc.js", () => ({ getAccessToken: vi.fn(async () => "tok-1") }));

import { authedEventSource } from "../api/auditStream.js";

afterEach(() => vi.unstubAllGlobals());

function sseBody(chunks) {
  return new ReadableStream({
    start(c) { for (const s of chunks) c.enqueue(new TextEncoder().encode(s)); c.close(); },
  });
}

describe("authedEventSource", () => {
  it("sends the bearer and delivers each data: line", async () => {
    const fetchMock = vi.fn(async () => ({ ok: true, body: sseBody(["retry: 3000\n\ndata: {\"event_id\":\"e1\"}\n\n: ping\n\nda", "ta: {\"event_id\":\"e2\"}\n\n"]) }));
    vi.stubGlobal("fetch", fetchMock);
    const got = [];
    const es = authedEventSource("/audit/stream");
    let opened = false, errored = false;
    es.onopen = () => { opened = true; };
    es.onmessage = (m) => got.push(JSON.parse(m.data).event_id);
    es.onerror = () => { errored = true; };
    await vi.waitFor(() => expect(errored).toBe(true)); // stream end reports an error so the view reconnects
    expect(fetchMock.mock.calls[0][1].headers.Authorization).toBe("Bearer tok-1");
    expect(opened).toBe(true);
    expect(got).toEqual(["e1", "e2"]);
  });

  it("reports an error, never open, on a refused stream", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => ({ ok: false, status: 401, body: null })));
    const es = authedEventSource("/audit/stream");
    let opened = false, errored = false;
    es.onopen = () => { opened = true; };
    es.onerror = () => { errored = true; };
    await vi.waitFor(() => expect(errored).toBe(true));
    expect(opened).toBe(false);
  });
});
