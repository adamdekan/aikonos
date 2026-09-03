import { describe, it, expect, beforeEach, afterEach } from "vitest";
import { createPinia, setActivePinia } from "pinia";
import { usePrefsStore } from "../store/prefs.js";

const PREFS_KEY = "aikonos.prefs";

describe("store/prefs.js", () => {
  beforeEach(() => {
    localStorage.clear();
    setActivePinia(createPinia());
  });

  afterEach(() => {
    localStorage.clear();
  });

  it("defaults when storage empty", () => {
    const store = usePrefsStore();
    expect(store.chatPersistEnabled).toBe(true);
    expect(store.chatPersistTurns).toBe(30);
    expect(store.chatInstructions).toBe("");
  });

  it("round-trips setChatPersistEnabled through localStorage", () => {
    const store = usePrefsStore();
    store.setChatPersistEnabled(false);
    expect(store.chatPersistEnabled).toBe(false);

    setActivePinia(createPinia());
    const reloaded = usePrefsStore();
    expect(reloaded.chatPersistEnabled).toBe(false);
  });

  it("round-trips setChatPersistTurns through localStorage", () => {
    const store = usePrefsStore();
    store.setChatPersistTurns(50);
    expect(store.chatPersistTurns).toBe(50);

    setActivePinia(createPinia());
    const reloaded = usePrefsStore();
    expect(reloaded.chatPersistTurns).toBe(50);
  });

  it("clamps setChatPersistTurns below minimum to 5", () => {
    const store = usePrefsStore();
    store.setChatPersistTurns(4);
    expect(store.chatPersistTurns).toBe(5);
  });

  it("clamps setChatPersistTurns above maximum to 200", () => {
    const store = usePrefsStore();
    store.setChatPersistTurns(999);
    expect(store.chatPersistTurns).toBe(200);
  });

  it("clamps non-numeric setChatPersistTurns to the default", () => {
    const store = usePrefsStore();
    store.setChatPersistTurns("abc");
    expect(store.chatPersistTurns).toBe(30);
  });

  it("clamps a stored out-of-range turns value on load", () => {
    localStorage.setItem(PREFS_KEY, JSON.stringify({ chatPersistTurns: 1 }));
    const store = usePrefsStore();
    expect(store.chatPersistTurns).toBe(5);
  });

  it("round-trips setChatInstructions through localStorage", () => {
    const store = usePrefsStore();
    store.setChatInstructions("answer in German");
    expect(store.chatInstructions).toBe("answer in German");

    setActivePinia(createPinia());
    const reloaded = usePrefsStore();
    expect(reloaded.chatInstructions).toBe("answer in German");
  });

  it("caps setChatInstructions at 2000 characters", () => {
    const store = usePrefsStore();
    store.setChatInstructions("x".repeat(2500));
    expect(store.chatInstructions.length).toBe(2000);
  });

  it("coerces a non-string stored chatInstructions to empty on load", () => {
    localStorage.setItem(PREFS_KEY, JSON.stringify({ chatInstructions: 42 }));
    const store = usePrefsStore();
    expect(store.chatInstructions).toBe("");
  });

  it("caps an oversized stored chatInstructions on load", () => {
    localStorage.setItem(PREFS_KEY, JSON.stringify({ chatInstructions: "y".repeat(3000) }));
    const store = usePrefsStore();
    expect(store.chatInstructions.length).toBe(2000);
  });

  it("falls back to defaults on corrupt JSON in storage without throwing", () => {
    localStorage.setItem(PREFS_KEY, "{not valid json");
    expect(() => usePrefsStore()).not.toThrow();
    const store = usePrefsStore();
    expect(store.chatPersistEnabled).toBe(true);
    expect(store.chatPersistTurns).toBe(30);
  });
});
