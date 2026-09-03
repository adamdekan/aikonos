import { describe, it, expect, beforeEach, afterEach } from "vitest";
import { createPinia, setActivePinia } from "pinia";
import { useThemeStore } from "../store/theme.js";

const THEME_KEY = "aikonos.theme";

describe("store/theme.js", () => {
  beforeEach(() => {
    localStorage.clear();
    document.documentElement.removeAttribute("data-theme");
    setActivePinia(createPinia());
  });

  afterEach(() => {
    localStorage.clear();
    document.documentElement.removeAttribute("data-theme");
  });

  it("defaults to dark when no stored pref and no light media match", () => {
    // intent: no stored pref and OS preference not light → dark
    const store = useThemeStore();
    expect(store.mode).toBe("dark");
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
  });

  it("resolves to light when localStorage has 'light' before store init", () => {
    // The store reads localStorage at construction time, so the key must be set
    // before the store is instantiated. Create a fresh pinia here (not reusing
    // the one from beforeEach) to ensure construction happens after the write.
    localStorage.setItem(THEME_KEY, "light");
    setActivePinia(createPinia());
    const store = useThemeStore();
    expect(store.mode).toBe("light");
  });

  it("toggle() flips mode, updates data-theme attribute, and writes localStorage", () => {
    const store = useThemeStore();
    // starts dark
    expect(store.mode).toBe("dark");

    store.toggle();

    expect(store.mode).toBe("light");
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
    expect(localStorage.getItem(THEME_KEY)).toBe("light");

    store.toggle();

    expect(store.mode).toBe("dark");
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
    expect(localStorage.getItem(THEME_KEY)).toBe("dark");
  });

  it("setMode('system') applies OS preference to data-theme", () => {
    const matchMediaMock = (query) => ({
      matches: query.includes("light"),
      media: query,
      addEventListener: () => {},
      removeEventListener: () => {},
    });
    const original = window.matchMedia;
    window.matchMedia = matchMediaMock;

    const store = useThemeStore();
    store.setMode("system");

    expect(store.mode).toBe("system");
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
    expect(localStorage.getItem(THEME_KEY)).toBe("system");

    window.matchMedia = original;
  });

  it("explicit modes (dark/light) behave unchanged when set", () => {
    const store = useThemeStore();
    store.setMode("light");
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
    store.setMode("dark");
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
  });

  it("stored 'system' restores on store construction", () => {
    localStorage.setItem(THEME_KEY, "system");
    window.matchMedia = (query) => ({
      matches: query.includes("light"),
      media: query,
      addEventListener: () => {},
      removeEventListener: () => {},
    });
    setActivePinia(createPinia());
    const store = useThemeStore();
    expect(store.mode).toBe("system");
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
  });

  it("re-applies on media-query change while in system mode", () => {
    let changeHandler = null;
    let matches = true; // starts light
    window.matchMedia = (query) => ({
      get matches() {
        return query.includes("light") ? matches : !matches;
      },
      media: query,
      addEventListener: (event, handler) => {
        if (event === "change") changeHandler = handler;
      },
      removeEventListener: () => {},
    });

    const store = useThemeStore();
    store.setMode("system");
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");

    // Simulate OS switching to dark.
    matches = false;
    changeHandler({ matches: false });

    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
  });

  it("toggle() from system goes to the opposite of the currently applied theme", () => {
    window.matchMedia = (query) => ({
      matches: query.includes("light"),
      media: query,
      addEventListener: () => {},
      removeEventListener: () => {},
    });

    const store = useThemeStore();
    store.setMode("system"); // applies to "light"

    store.toggle();

    expect(store.mode).toBe("dark");
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
  });
});
