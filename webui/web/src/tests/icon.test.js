import { mount } from "@vue/test-utils";
import { describe, it, expect, vi } from "vitest";
import Icon from "../components/Icon.vue";

const DOCUMENTED_GLYPHS = [
  "plus", "chat", "home", "projects", "artifacts", "customize", "code",
  "design", "star", "search", "sidebar", "connections", "files", "schedules",
  "inbox", "admin", "network", "audit", "send", "settings", "drive", "trash",
  "download", "upload", "pause", "play", "check", "close", "chevron-down",
  "spinner", "stop",
  // Admin-category glyphs
  "users", "tool", "book", "gauge", "server", "bot", "cpu", "play-circle", "scale",
];

describe("Icon.vue", () => {
  it.each(DOCUMENTED_GLYPHS)("renders non-empty svg for glyph '%s'", (name) => {
    const wrapper = mount(Icon, { props: { name } });
    const svg = wrapper.find("svg");
    expect(svg.exists(), `expected svg for ${name}`).toBe(true);
    // must have actual path/shape content inside the svg element
    expect(wrapper.find("svg").element.innerHTML.length).toBeGreaterThan(0);
  });

  it("renders nothing for unknown glyph", () => {
    const warnSpy = vi.spyOn(console, "warn").mockImplementation(() => {});
    const wrapper = mount(Icon, { props: { name: "does-not-exist-xyz" } });
    expect(wrapper.find("svg").exists()).toBe(false);
    expect(warnSpy).toHaveBeenCalledWith(expect.stringContaining("does-not-exist-xyz"));
    warnSpy.mockRestore();
  });

  it("accepts a size prop and applies it", () => {
    const wrapper = mount(Icon, { props: { name: "home", size: 32 } });
    const svg = wrapper.find("svg");
    expect(svg.attributes("width")).toBe("32");
    expect(svg.attributes("height")).toBe("32");
  });
});
