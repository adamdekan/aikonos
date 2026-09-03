// CP4 test: chunked base64 encode/decode in session persistence must not
// RangeError on large payloads (the old spread-per-byte approach blows the
// call stack around a few hundred KB of UTF-8 bytes).
import { describe, it, expect, vi, beforeEach } from "vitest";

vi.mock("../api/client.js", () => ({
  get: vi.fn(),
  post: vi.fn(),
  del: vi.fn(),
  patch: vi.fn(),
}));

import * as clientMod from "../api/client.js";
import { readSession, writeSession } from "../api/sessions.js";

beforeEach(() => {
  vi.clearAllMocks();
});

describe("api/sessions.js — large payload base64 round-trip", () => {
  it("writeSession encodes a >1MB record without RangeError, and readSession decodes it back intact", async () => {
    // A single JS string char is at most 3 UTF-8 bytes; repeat to comfortably exceed 1MB encoded.
    const big = "x".repeat(1_200_000);
    const record = { id: "big", title: "t", payload: big };

    clientMod.post.mockResolvedValue({});
    await expect(writeSession(record)).resolves.not.toThrow();

    const sentBase64 = clientMod.post.mock.calls[0][1].body.contentBase64;
    expect(typeof sentBase64).toBe("string");
    expect(sentBase64.length).toBeGreaterThan(0);

    clientMod.get.mockResolvedValue({ contentBase64: sentBase64 });
    const decoded = await readSession("big");
    expect(decoded).toEqual(record);
  });

  it("writeSession/readSession round-trips a >1MB multi-byte UTF-8 payload straddling 32768-byte chunk boundaries", async () => {
    // Unit mixes 1/3/4-byte UTF-8 sequences (z=1, €=3, 😀=4 surrogate-pair, 日本語=3x3=9)
    // for a 17-byte-per-unit pattern. 17 shares no common factor with the 0x8000 (32768)
    // chunk size, so as the pattern repeats, chunk boundaries fall at every possible offset
    // within the unit — guaranteeing some repetitions have a multi-byte character's UTF-8
    // bytes split across a chunk boundary, unlike a pattern length that evenly divides 32768.
    const unit = "z€😀日本語";
    const unitBytes = new TextEncoder().encode(unit).length;
    expect(unitBytes).toBe(17);

    const repeats = Math.ceil(1_200_000 / unitBytes) + 100; // comfortably >1MB encoded
    const bigMultiByte = unit.repeat(repeats);
    expect(new TextEncoder().encode(bigMultiByte).length).toBeGreaterThan(1_200_000);

    const record = { id: "big-utf8", title: "t", payload: bigMultiByte };

    clientMod.post.mockResolvedValue({});
    await expect(writeSession(record)).resolves.not.toThrow();

    const sentBase64 = clientMod.post.mock.calls[0][1].body.contentBase64;
    expect(typeof sentBase64).toBe("string");
    expect(sentBase64.length).toBeGreaterThan(0);

    clientMod.get.mockResolvedValue({ contentBase64: sentBase64 });
    const decoded = await readSession("big-utf8");
    expect(decoded).toEqual(record);
    expect(decoded.payload).toBe(bigMultiByte);
  });
});
