import { describe, it, expect, vi } from "vitest";
import { runAgui } from "../api/agui.js";

const testToken = async () => "test-bearer-token";

function makeResponse() {
  return {
    ok: true,
    body: new ReadableStream({ start(c) { c.close(); } }),
  };
}

describe("runAgui — history field in POST body", () => {
  it("includes history in the POST body when a non-empty array is passed", async () => {
    const fetchMock = vi.fn().mockResolvedValue(makeResponse());
    const history = [
      { role: "user", content: "first question" },
      { role: "assistant", content: "first answer" },
    ];

    await runAgui({ prompt: "follow-up", threadId: "t1", history }, {}, fetchMock, testToken);

    const [, opts] = fetchMock.mock.calls[0];
    const body = JSON.parse(opts.body);
    expect(body.history).toEqual(history);
  });

  it("omits history from the POST body when an empty array is passed", async () => {
    const fetchMock = vi.fn().mockResolvedValue(makeResponse());

    await runAgui({ prompt: "hi", threadId: "t1", history: [] }, {}, fetchMock, testToken);

    const [, opts] = fetchMock.mock.calls[0];
    const body = JSON.parse(opts.body);
    expect(body.history).toBeUndefined();
  });

  it("omits history from the POST body when history is not provided", async () => {
    const fetchMock = vi.fn().mockResolvedValue(makeResponse());

    await runAgui({ prompt: "hi", threadId: "t1" }, {}, fetchMock, testToken);

    const [, opts] = fetchMock.mock.calls[0];
    const body = JSON.parse(opts.body);
    expect(body.history).toBeUndefined();
  });

  it("omits history when history is undefined", async () => {
    const fetchMock = vi.fn().mockResolvedValue(makeResponse());

    await runAgui({ prompt: "hi", threadId: "t1", history: undefined }, {}, fetchMock, testToken);

    const [, opts] = fetchMock.mock.calls[0];
    const body = JSON.parse(opts.body);
    expect(body.history).toBeUndefined();
  });
});
