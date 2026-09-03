import { describe, it, expect } from "vitest";
import { toMicros, fromMicros, fmtAmount } from "../lib/money.js";

describe("lib/money.js", () => {
  it("round-trips whole major units", () => {
    expect(toMicros(10)).toBe(10_000_000);
    expect(fromMicros(10_000_000)).toBe(10);
  });

  it("round-trips cents", () => {
    expect(toMicros(19.99)).toBe(19_990_000);
    expect(fromMicros(19_990_000)).toBe(19.99);
  });

  it("guards fractional-cent float imprecision", () => {
    // 0.1 + 0.2 in raw float arithmetic is 0.30000000000000004 — the rounding
    // in toMicros must still land on the exact integer micro count.
    expect(toMicros(0.1 + 0.2)).toBe(300_000);
    expect(toMicros(2.005)).toBe(2_005_000);
  });

  it("treats non-positive/invalid input as zero", () => {
    expect(toMicros(0)).toBe(0);
    expect(toMicros(-5)).toBe(0);
    expect(toMicros(NaN)).toBe(0);
    expect(toMicros("")).toBe(0);
  });

  it("fmtAmount renders 2-decimal strings with no currency symbol", () => {
    expect(fmtAmount(1_000_000)).toBe("1.00");
    expect(fmtAmount(19_990_000)).toBe("19.99");
    expect(fmtAmount(0)).toBe("0.00");
  });
});
