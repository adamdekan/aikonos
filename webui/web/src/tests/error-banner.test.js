// CP2 (F34): shared ErrorBanner — renders message, optional retry action.
import { mount } from "@vue/test-utils";
import { describe, it, expect } from "vitest";
import ErrorBanner from "../components/ui/ErrorBanner.vue";

describe("ErrorBanner.vue", () => {
  it("renders the message prop", () => {
    const w = mount(ErrorBanner, { props: { message: "Load failed" } });
    expect(w.find("[data-testid='error-banner']").text()).toContain("Load failed");
  });

  it("renders no action controls when the action slot is unused", () => {
    const w = mount(ErrorBanner, { props: { message: "Load failed" } });
    expect(w.find("button").exists()).toBe(false);
  });

  it("renders an action slot's contents alongside the message", () => {
    const w = mount(ErrorBanner, {
      props: { message: "Mention and tool palettes unavailable" },
      slots: {
        action: `<button data-testid="retry-btn">Retry</button>`,
      },
    });
    expect(w.find("[data-testid='retry-btn']").exists()).toBe(true);
    expect(w.find("[data-testid='error-banner']").text()).toContain("Mention and tool palettes unavailable");
  });
});
