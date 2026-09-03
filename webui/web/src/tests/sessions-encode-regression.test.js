// CP1 regression pin: encodeJson (api/sessions.js) already chunks base64 encoding to avoid
// blowing the String.fromCharCode.apply argument-count limit on large payloads. This test
// pins that behavior via the public writeSession/readSession round-trip (encodeJson/decodeJson
// are private) so a future refactor can't silently regress it.
import { describe, it, expect, vi, beforeEach } from "vitest";

vi.mock("../api/client.js", () => ({
  get: vi.fn(),
  post: vi.fn(),
  del: vi.fn(),
  patch: vi.fn(),
}));

import * as clientMod from "../api/client.js";
import { writeSession, readSession } from "../api/sessions.js";

describe("api/sessions.js — encodeJson/decodeJson round-trip (>1 MiB payload)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("round-trips a >1 MiB record without throwing, deep-equal", async () => {
    // ~1.2 MiB of message text.
    const bigText = "x".repeat(1.2 * 1024 * 1024);
    const record = {
      id: "big-session",
      title: "Big",
      messages: [{ role: "assistant", text: bigText }],
    };

    let stored = null;
    clientMod.post.mockImplementation(async (_path, { body }) => {
      stored = body.contentBase64;
      return {};
    });
    clientMod.get.mockImplementation(async () => ({ contentBase64: stored }));

    await expect(writeSession(record)).resolves.not.toThrow();

    const roundTripped = await readSession("big-session");
    expect(roundTripped).toEqual(record);
  });
});
