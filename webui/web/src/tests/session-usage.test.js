// Tests for SessionUsage.vue — the per-session model/tokens/price strip above
// the composer.
// WHY: the strip is read as authoritative ("this conversation cost X"), so the
// contracts that matter are (a) nothing is shown when nothing was billed, so an
// empty session never implies a zero cost, (b) a switch between sessions never
// leaves the previous session's numbers on screen, and (c) totals are re-read
// when a run finishes, including after the fire-and-forget usage emit settles.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";

vi.mock("../api/usage.js", () => ({
  getSessionUsage: vi.fn(),
}));

import SessionUsage from "../components/SessionUsage.vue";
import * as usageApi from "../api/usage.js";

function usage(over = {}) {
  return {
    models: ["gpt-5.6-terra"],
    tokensIn: 1234,
    tokensOut: 567,
    cacheRead: 0,
    cacheWrite: 0,
    costMicros: 24_304,
    calls: 3,
    ...over,
  };
}

beforeEach(() => {
  vi.clearAllMocks();
});

afterEach(() => {
  vi.useRealTimers();
});

describe("SessionUsage", () => {
  it("renders model, tokens and price for a billed session", async () => {
    usageApi.getSessionUsage.mockResolvedValue(usage());
    const w = mount(SessionUsage, { props: { sessionId: "s1", running: false } });
    await flushPromises();

    const text = w.get('[data-testid="session-usage"]').text();
    expect(text).toContain("gpt-5.6-terra");
    // Thousands separators come from toLocaleString.
    expect(text).toContain("1,234");
    expect(text).toContain("567");
    // 24304 micros = 0.024304. Rendered at 3dp ("0.024") rather than the 2dp
    // fmtAmount uses elsewhere, which would collapse a typical session to 0.00.
    expect(text).toContain("0.024");
  });

  it("renders nothing for an empty session with no billed calls", async () => {
    // WHY: there is no run to report on yet, so a zero row would be noise.
    usageApi.getSessionUsage.mockResolvedValue(usage({ calls: 0, costMicros: 0 }));
    const w = mount(SessionUsage, { props: { sessionId: "s1", running: false } });
    await flushPromises();
    expect(w.find('[data-testid="session-usage"]').exists()).toBe(false);
  });

  it("states no LLM cost when a session with content billed nothing", async () => {
    // WHY: a tool-only workflow (no kind:"reason" step) calls no model, so zero
    // is the correct answer. Hiding the strip there is indistinguishable from a
    // broken read — which is exactly how it was reported.
    usageApi.getSessionUsage.mockResolvedValue(usage({ calls: 0, costMicros: 0 }));
    const w = mount(SessionUsage, {
      props: { sessionId: "s1", running: false, hasContent: true },
    });
    await flushPromises();

    expect(w.find('[data-testid="session-usage-free"]').exists()).toBe(true);
    expect(w.get('[data-testid="session-usage"]').text()).toContain("no LLM cost");
    // No fabricated model name or token counts alongside it.
    expect(w.text()).not.toContain("Tokens In");
  });

  it("hides rather than claiming zero cost when the read fails", async () => {
    // WHY: billedNothing must mean "the broker said zero", never "we do not know".
    usageApi.getSessionUsage.mockRejectedValue(new Error("gateway down"));
    const w = mount(SessionUsage, {
      props: { sessionId: "s1", running: false, hasContent: true },
    });
    await flushPromises();
    expect(w.find('[data-testid="session-usage"]').exists()).toBe(false);
  });

  it("renders nothing when there is no session at all", async () => {
    const w = mount(SessionUsage, { props: { sessionId: "", running: false } });
    await flushPromises();
    expect(usageApi.getSessionUsage).not.toHaveBeenCalled();
    expect(w.find('[data-testid="session-usage"]').exists()).toBe(false);
  });

  it("summarises multiple models as first +N", async () => {
    usageApi.getSessionUsage.mockResolvedValue(
      usage({ models: ["expensive-model", "cheap-model", "cheap-model"] }),
    );
    const w = mount(SessionUsage, { props: { sessionId: "s1", running: false } });
    await flushPromises();
    // Deduped: 2 distinct models -> "+1", not "+2".
    expect(w.get('[data-testid="session-usage"]').text()).toContain("expensive-model +1");
  });

  it("clears the previous session's numbers when the session changes", async () => {
    // WHY: leaving stale totals on screen after a session switch misattributes
    // one conversation's cost to another.
    usageApi.getSessionUsage.mockResolvedValue(usage({ tokensIn: 111 }));
    const w = mount(SessionUsage, { props: { sessionId: "s1", running: false } });
    await flushPromises();
    expect(w.text()).toContain("111");

    usageApi.getSessionUsage.mockResolvedValue(usage({ calls: 0 }));
    await w.setProps({ sessionId: "s2" });
    await flushPromises();
    expect(w.text()).not.toContain("111");
  });

  it("hides the strip when the read fails rather than showing stale numbers", async () => {
    usageApi.getSessionUsage.mockResolvedValue(usage());
    const w = mount(SessionUsage, { props: { sessionId: "s1", running: false } });
    await flushPromises();
    expect(w.find('[data-testid="session-usage"]').exists()).toBe(true);

    usageApi.getSessionUsage.mockRejectedValue(new Error("gateway down"));
    await w.setProps({ sessionId: "s2" });
    await flushPromises();
    expect(w.find('[data-testid="session-usage"]').exists()).toBe(false);
  });

  it("refetches when a run finishes, then again after the emit settles", async () => {
    // WHY: the broker-side usage insert is fire-and-forget relative to the run
    // completing, so the immediate refetch can miss the final call. The delayed
    // second read is what makes the last turn's cost appear without a reload.
    vi.useFakeTimers();
    usageApi.getSessionUsage.mockResolvedValue(usage());
    const w = mount(SessionUsage, { props: { sessionId: "s1", running: false } });
    await flushPromises();
    expect(usageApi.getSessionUsage).toHaveBeenCalledTimes(1);

    await w.setProps({ running: true });
    await flushPromises();
    // A run starting must not refetch — nothing is billed yet.
    expect(usageApi.getSessionUsage).toHaveBeenCalledTimes(1);

    await w.setProps({ running: false });
    await flushPromises();
    expect(usageApi.getSessionUsage).toHaveBeenCalledTimes(2);

    await vi.advanceTimersByTimeAsync(2100);
    expect(usageApi.getSessionUsage).toHaveBeenCalledTimes(3);
  });

  it("does not fire a pending settle refetch after unmount", async () => {
    vi.useFakeTimers();
    usageApi.getSessionUsage.mockResolvedValue(usage());
    const w = mount(SessionUsage, { props: { sessionId: "s1", running: true } });
    await flushPromises();

    await w.setProps({ running: false });
    await flushPromises();
    const callsBefore = usageApi.getSessionUsage.mock.calls.length;

    w.unmount();
    await vi.advanceTimersByTimeAsync(2100);
    expect(usageApi.getSessionUsage).toHaveBeenCalledTimes(callsBefore);
  });
});
