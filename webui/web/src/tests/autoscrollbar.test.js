import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { autoScrollbar } from "../lib/autoScrollbar.js";

// The directive toggles `is-scrolling` while the element scrolls, then removes
// it after an idle window — the contract the .scroll-min CSS depends on.
describe("v-autoscroll directive", () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  function makeEl() {
    const el = document.createElement("div");
    autoScrollbar.mounted(el);
    return el;
  }

  it("adds is-scrolling on scroll and removes it after the idle window", () => {
    const el = makeEl();
    expect(el.classList.contains("is-scrolling")).toBe(false);

    el.dispatchEvent(new Event("scroll"));
    expect(el.classList.contains("is-scrolling")).toBe(true);

    vi.advanceTimersByTime(699);
    expect(el.classList.contains("is-scrolling")).toBe(true);

    vi.advanceTimersByTime(1);
    expect(el.classList.contains("is-scrolling")).toBe(false);
  });

  it("debounces: a later scroll extends the visible window", () => {
    const el = makeEl();
    el.dispatchEvent(new Event("scroll"));
    vi.advanceTimersByTime(500);
    el.dispatchEvent(new Event("scroll")); // resets the idle timer
    vi.advanceTimersByTime(500);
    expect(el.classList.contains("is-scrolling")).toBe(true); // still within window
    vi.advanceTimersByTime(200);
    expect(el.classList.contains("is-scrolling")).toBe(false);
  });

  it("detaches the listener on unmount", () => {
    const el = makeEl();
    autoScrollbar.unmounted(el);
    el.dispatchEvent(new Event("scroll"));
    expect(el.classList.contains("is-scrolling")).toBe(false);
    expect(el.__autoScrollbar).toBeUndefined();
  });
});
