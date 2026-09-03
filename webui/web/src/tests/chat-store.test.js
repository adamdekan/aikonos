import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { createPinia, setActivePinia } from "pinia";
import { useChatStore, CHAT_STORAGE_KEY } from "../store/chat.js";
import { useUserStore } from "../store/user.js";
import { usePrefsStore } from "../store/prefs.js";

describe("store/chat.js", () => {
  beforeEach(() => {
    localStorage.clear();
    setActivePinia(createPinia());
  });

  afterEach(() => {
    localStorage.clear();
    vi.restoreAllMocks();
  });

  it("CHAT_STORAGE_KEY is exported and is the expected string", () => {
    expect(CHAT_STORAGE_KEY).toBe("aikonos.chatSessions");
  });

  // ── currentBuffer ─────────────────────────────────────────────────────────────

  it("currentBuffer() returns a buffer with non-empty threadId and empty messages when no session is active (draft key)", () => {
    const store = useChatStore();
    const buf = store.currentBuffer();
    expect(buf.threadId).toBeTruthy();
    expect(typeof buf.threadId).toBe("string");
    expect(buf.messages).toEqual([]);
  });

  it("currentBuffer() returns the same object on repeated calls when activeSessionId is null (draft buffer is stable)", () => {
    const store = useChatStore();
    const b1 = store.currentBuffer();
    const b2 = store.currentBuffer();
    expect(b1).toBe(b2);
    expect(b1.threadId).toBe(b2.threadId);
  });

  it("currentBuffer() uses 'draft' key when activeSessionId is null — persisted localStorage has no '::' key", () => {
    const store = useChatStore();
    store.currentBuffer();
    store.persist();
    const raw = localStorage.getItem(CHAT_STORAGE_KEY);
    expect(raw).toBeTruthy();
    const parsed = JSON.parse(raw);
    // No old user::agentId keys must exist
    const hasColonColon = Object.keys(parsed).some(k => k.includes("::"));
    expect(hasColonColon).toBe(false);
    // "draft" key must be present
    expect(parsed["draft"]).toBeDefined();
    expect(parsed["draft"].threadId).toBeTruthy();
  });

  it("currentBuffer() keyed by activeSessionId when a session is active", () => {
    const store = useChatStore();
    store.setActiveSession("session-abc");
    const buf = store.currentBuffer();
    store.persist();
    const parsed = JSON.parse(localStorage.getItem(CHAT_STORAGE_KEY));
    expect(parsed["session-abc"]).toBeDefined();
    expect(parsed["session-abc"].threadId).toBe(buf.threadId);
  });

  it("currentBuffer() returns different buffers for different active session ids", () => {
    const store = useChatStore();

    store.setActiveSession("session-A");
    const bufA = store.currentBuffer();
    bufA.messages.push({ role: "user", text: "A message" });

    store.hydrateBuffer("session-B", { threadId: "tid-B", messages: [{ role: "user", text: "B message" }] });

    // now active is B
    expect(store.activeSessionId).toBe("session-B");
    const bufB = store.currentBuffer();
    expect(bufB.messages[0].text).toBe("B message");

    // switch back to A to verify A's content persisted
    store.hydrateBuffer("session-A", { threadId: bufA.threadId, messages: bufA.messages });
    expect(store.currentBuffer().messages[0].text).toBe("A message");
  });

  // ── reset ─────────────────────────────────────────────────────────────────────

  it("reset() replaces draft buffer with fresh threadId and empty messages", () => {
    const store = useChatStore();
    const before = store.currentBuffer();
    const oldThreadId = before.threadId;
    before.messages.push({ role: "user", text: "old" });

    store.reset();

    const after = store.currentBuffer();
    expect(after.threadId).not.toBe(oldThreadId);
    expect(after.messages).toEqual([]);
  });

  it("reset() persists the new draft buffer to localStorage", () => {
    const store = useChatStore();
    store.currentBuffer();
    store.reset();

    const raw = localStorage.getItem(CHAT_STORAGE_KEY);
    const parsed = JSON.parse(raw);
    expect(parsed["draft"]).toBeDefined();
    expect(parsed["draft"].messages).toEqual([]);
    // No :: keys
    expect(Object.keys(parsed).some(k => k.includes("::"))).toBe(false);
  });

  it("reset() prunes buffers other than active and draft", () => {
    const store = useChatStore();
    // Hydrate two session buffers
    store.hydrateBuffer("S1", { threadId: "t1", messages: [] });
    store.hydrateBuffer("S2", { threadId: "t2", messages: [] });
    // Active is S2; now reset (goes to draft, clears active)
    store.clearActiveSession();
    store.reset();
    store.persist();
    const parsed = JSON.parse(localStorage.getItem(CHAT_STORAGE_KEY));
    // Only "draft" should remain (active is null, so no active buffer to keep)
    expect(Object.keys(parsed)).toEqual(["draft"]);
  });

  // ── hydrateBuffer ─────────────────────────────────────────────────────────────

  it("hydrateBuffer(sessionId, ...) sets buffers[sessionId] and sets activeSessionId", () => {
    const store = useChatStore();
    store.hydrateBuffer("sid-42", { threadId: "thread-42", messages: [{ role: "user", text: "hi" }] });
    expect(store.activeSessionId).toBe("sid-42");
    const buf = store.currentBuffer();
    expect(buf.threadId).toBe("thread-42");
    expect(buf.messages).toEqual([{ role: "user", text: "hi" }]);
  });

  it("hydrateBuffer persists keyed by sessionId (no :: in key)", () => {
    const store = useChatStore();
    store.hydrateBuffer("sid-99", { threadId: "t99", messages: [] });
    store.persist();
    const parsed = JSON.parse(localStorage.getItem(CHAT_STORAGE_KEY));
    expect(parsed["sid-99"]).toBeDefined();
    expect(Object.keys(parsed).some(k => k.includes("::"))).toBe(false);
  });

  it("hydrateBuffer prunes previously-active buffer (only active + draft retained)", () => {
    const store = useChatStore();
    // Set up a prior active session
    store.hydrateBuffer("S-old", { threadId: "t-old", messages: [{ role: "user", text: "old" }] });
    // Now hydrate a new session
    store.hydrateBuffer("S-new", { threadId: "t-new", messages: [] });
    store.persist();
    const parsed = JSON.parse(localStorage.getItem(CHAT_STORAGE_KEY));
    // S-old should be pruned; S-new and "draft" may be present
    expect(parsed["S-old"]).toBeUndefined();
    expect(parsed["S-new"]).toBeDefined();
  });

  it("hydrateBuffer uses fallback threadId when not provided", () => {
    const store = useChatStore();
    store.hydrateBuffer("sid-x", { messages: [{ role: "user", text: "hello" }] });
    const buf = store.currentBuffer();
    expect(buf.threadId).toBeTruthy();
    expect(buf.messages).toEqual([{ role: "user", text: "hello" }]);
  });

  // ── promoteDraft ──────────────────────────────────────────────────────────────

  it("promoteDraft(sessionId) moves the draft buffer to buffers[sessionId], preserving threadId and messages", () => {
    const store = useChatStore();
    const draft = store.currentBuffer(); // creates draft
    const draftThreadId = draft.threadId;
    draft.messages.push({ role: "user", text: "first" });

    store.promoteDraft("new-session-id");

    expect(store.activeSessionId).toBe("new-session-id");
    const buf = store.currentBuffer();
    // Same object identity (threadId and messages preserved)
    expect(buf.threadId).toBe(draftThreadId);
    expect(buf.messages).toEqual([{ role: "user", text: "first" }]);
  });

  it("promoteDraft removes the draft key and adds the session key", () => {
    const store = useChatStore();
    store.currentBuffer(); // create draft
    store.promoteDraft("S-real");
    store.persist();
    const parsed = JSON.parse(localStorage.getItem(CHAT_STORAGE_KEY));
    expect(parsed["S-real"]).toBeDefined();
    // draft buffer should be removed after promote (only active is retained)
    expect(parsed["draft"]).toBeUndefined();
  });

  it("threadId is stable across draft→id promotion (first message send does not change threadId)", () => {
    const store = useChatStore();
    const draft = store.currentBuffer();
    const originalThreadId = draft.threadId;
    draft.messages.push({ role: "user", text: "hello" });

    // simulate maybeCreateSession: promoteDraft then build record from currentBuffer
    store.promoteDraft("real-session");
    const buf = store.currentBuffer();

    // threadId must be the same (no new uuid minted)
    expect(buf.threadId).toBe(originalThreadId);
  });

  // ── pruning invariant ─────────────────────────────────────────────────────────

  it("persisted map size is ≤ 2 after switching sessions (no per-agent leak)", () => {
    const store = useChatStore();

    store.hydrateBuffer("S1", { threadId: "t1", messages: [] });
    store.hydrateBuffer("S2", { threadId: "t2", messages: [] });
    store.hydrateBuffer("S3", { threadId: "t3", messages: [] });
    store.persist();

    const parsed = JSON.parse(localStorage.getItem(CHAT_STORAGE_KEY));
    expect(Object.keys(parsed).length).toBeLessThanOrEqual(2);
  });

  it("draft buffer is retained alongside active buffer (≤ 2 total)", () => {
    const store = useChatStore();
    // Create draft
    store.currentBuffer();
    // Hydrate an active session
    store.hydrateBuffer("S1", { threadId: "t1", messages: [] });
    store.persist();
    const parsed = JSON.parse(localStorage.getItem(CHAT_STORAGE_KEY));
    // active "S1" stays; "draft" may also stay — total ≤ 2
    expect(Object.keys(parsed).length).toBeLessThanOrEqual(2);
  });

  // ── old-schema migration ──────────────────────────────────────────────────────

  it("discards old user::agentId schema on load and starts empty", () => {
    const old = {
      "alice@example.com::": { threadId: "old-tid", messages: [{ role: "user", text: "hi" }] },
      "alice@example.com::agent-abc": { threadId: "old-tid2", messages: [] },
    };
    localStorage.setItem(CHAT_STORAGE_KEY, JSON.stringify(old));
    setActivePinia(createPinia());
    const store = useChatStore();
    // currentBuffer creates a fresh draft (old content discarded)
    const buf = store.currentBuffer();
    expect(buf.threadId).not.toBe("old-tid");
    expect(buf.messages).toEqual([]);
  });

  it("does NOT discard new-schema (session id / 'draft') keys on load", () => {
    const newSchema = {
      "session-xyz": { threadId: "existing-tid", messages: [{ role: "user", text: "restored" }] },
    };
    localStorage.setItem(CHAT_STORAGE_KEY, JSON.stringify(newSchema));
    setActivePinia(createPinia());
    const store = useChatStore();
    store.setActiveSession("session-xyz");
    const buf = store.currentBuffer();
    expect(buf.threadId).toBe("existing-tid");
    expect(buf.messages).toEqual([{ role: "user", text: "restored" }]);
  });

  // ── persist / error resilience ────────────────────────────────────────────────

  it("persist writes the buffer map to localStorage", () => {
    const store = useChatStore();
    const buf = store.currentBuffer();
    buf.messages.push({ role: "user", text: "hello" });
    store.persist();

    const raw = localStorage.getItem(CHAT_STORAGE_KEY);
    const parsed = JSON.parse(raw);
    expect(parsed["draft"].messages).toEqual([{ role: "user", text: "hello" }]);
  });

  it("store construction does not throw when localStorage.getItem throws — uses empty map", () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("SecurityError: access denied");
    });
    setActivePinia(createPinia());
    let store;
    expect(() => {
      store = useChatStore();
    }).not.toThrow();
    vi.restoreAllMocks();
    expect(() => store.currentBuffer()).not.toThrow();
  });

  it("persist does not throw when localStorage.setItem throws", () => {
    const store = useChatStore();
    store.currentBuffer();
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("QuotaExceededError");
    });
    expect(() => store.persist()).not.toThrow();
  });

  // ── F41: truncation + quota toast ───────────────────────────────────────────────

  it("persist() truncates a large tool result in the serialized copy but keeps it in memory", () => {
    const store = useChatStore();
    const buf = store.currentBuffer();
    const bigResult = "x".repeat(3000); // > 2 KiB when JSON-stringified
    buf.messages.push({
      role: "assistant",
      text: "done",
      tools: [{ id: "t1", name: "web.fetch", result: bigResult, isError: false, done: true }],
    });
    store.persist();

    // In-memory buffer keeps the full payload.
    expect(buf.messages[0].tools[0].result).toBe(bigResult);

    // Serialized copy carries the stub, not the full payload.
    const parsed = JSON.parse(localStorage.getItem(CHAT_STORAGE_KEY));
    const storedResult = parsed["draft"].messages[0].tools[0].result;
    expect(storedResult).not.toBe(bigResult);
    expect(JSON.parse(storedResult)).toEqual({
      truncated: true,
      note: "tool result truncated for local storage",
    });
  });

  it("persist() leaves small tool results untouched in the serialized copy", () => {
    const store = useChatStore();
    const buf = store.currentBuffer();
    buf.messages.push({
      role: "assistant",
      text: "done",
      tools: [{ id: "t1", name: "web.fetch", result: "small result", isError: false, done: true }],
    });
    store.persist();

    const parsed = JSON.parse(localStorage.getItem(CHAT_STORAGE_KEY));
    expect(parsed["draft"].messages[0].tools[0].result).toBe("small result");
  });

  it("persist() keeps only the last 30 messages in the serialized copy, full history in memory", () => {
    const store = useChatStore();
    const buf = store.currentBuffer();
    for (let i = 0; i < 35; i++) {
      buf.messages.push({ role: "user", text: `msg-${i}` });
    }
    store.persist();

    expect(buf.messages.length).toBe(35);

    const parsed = JSON.parse(localStorage.getItem(CHAT_STORAGE_KEY));
    const storedMessages = parsed["draft"].messages;
    expect(storedMessages.length).toBe(30);
    expect(storedMessages[0].text).toBe("msg-5");
    expect(storedMessages[29].text).toBe("msg-34");
  });

  it("persist() toasts once per session on storage failure, then stays silent on subsequent failures", async () => {
    const { useToast } = await import("../components/ui/useToast.js");
    const { toasts } = useToast();
    toasts.splice(0, toasts.length);

    const store = useChatStore();
    store.currentBuffer();
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("QuotaExceededError");
    });

    store.persist();
    store.persist();

    const quotaToasts = toasts.filter((t) => t.msg.includes("too large for local storage"));
    expect(quotaToasts.length).toBe(1);
  });

  // ── CP3: chat persist honors prefs ──────────────────────────────────────────────

  it("persist() truncates to chatPersistTurns instead of the hardcoded 30", () => {
    const store = useChatStore();
    usePrefsStore().setChatPersistTurns(10);
    const buf = store.currentBuffer();
    for (let i = 0; i < 15; i++) {
      buf.messages.push({ role: "user", text: `msg-${i}` });
    }
    store.persist();

    expect(buf.messages.length).toBe(15);
    const parsed = JSON.parse(localStorage.getItem(CHAT_STORAGE_KEY));
    const storedMessages = parsed["draft"].messages;
    expect(storedMessages.length).toBe(10);
    expect(storedMessages[0].text).toBe("msg-5");
    expect(storedMessages[9].text).toBe("msg-14");
  });

  it("persist() removes the stored key and does not write when chatPersistEnabled is false", () => {
    const store = useChatStore();
    const buf = store.currentBuffer();
    buf.messages.push({ role: "user", text: "hello" });
    store.persist();
    expect(localStorage.getItem(CHAT_STORAGE_KEY)).not.toBeNull();

    usePrefsStore().setChatPersistEnabled(false);
    store.persist();

    expect(localStorage.getItem(CHAT_STORAGE_KEY)).toBeNull();
    // In-memory buffer is unaffected.
    expect(buf.messages.length).toBe(1);
  });

  // ── setActiveSession / clearActiveSession ─────────────────────────────────────

  it("setActiveSession / clearActiveSession update activeSessionId", () => {
    const store = useChatStore();
    expect(store.activeSessionId).toBe(null);
    store.setActiveSession("sess-1");
    expect(store.activeSessionId).toBe("sess-1");
    store.clearActiveSession();
    expect(store.activeSessionId).toBe(null);
  });

  it("clearActiveSession causes currentBuffer() to return the draft buffer", () => {
    const store = useChatStore();
    store.setActiveSession("sess-1");
    store.clearActiveSession();
    const buf = store.currentBuffer();
    store.persist();
    const parsed = JSON.parse(localStorage.getItem(CHAT_STORAGE_KEY));
    expect(parsed["draft"]).toBeDefined();
  });

  // ── skills timeline persistence (auto-skill-loading CP4) ───────────────────────

  it("persist() round-trips a role:'skills' message intact", () => {
    const store = useChatStore();
    const buf = store.currentBuffer();
    buf.messages.push({ role: "user", text: "bill me please" });
    buf.messages.push({
      role: "skills",
      skills: [
        { name: "billing", description: "Billing helper", status: "loaded" },
        { name: "hr-tools", description: "HR helper", status: "suppressed", reason: "cap-overflow" },
      ],
    });
    buf.messages.push({ role: "assistant", text: "done", tools: [], error: null });
    store.persist();

    const parsed = JSON.parse(localStorage.getItem(CHAT_STORAGE_KEY));
    const stored = parsed["draft"].messages;
    const skillsMsg = stored.find((m) => m.role === "skills");
    expect(skillsMsg).toEqual(buf.messages[1]);
  });

  // ── clearAll ──────────────────────────────────────────────────────────────────

  it("clearAll() removes all buffers, clears activeSessionId, and removes localStorage key", () => {
    const store = useChatStore();
    store.hydrateBuffer("S1", { threadId: "t1", messages: [{ role: "user", text: "secret" }] });
    store.persist();
    // Confirm setup: key exists
    expect(localStorage.getItem(CHAT_STORAGE_KEY)).not.toBeNull();

    store.clearAll();

    expect(localStorage.getItem(CHAT_STORAGE_KEY)).toBeNull();
    expect(store.activeSessionId).toBe(null);
    expect(store.currentBuffer().messages.length).toBe(0);
  });

  // ── cross-user draft bleed (privacy regression) ───────────────────────────────
  // User A leaves a draft, logs out (userStore.clear()), user B logs in.
  // B must NOT see A's draft.

  it("userStore.clear() wipes the chat buffer — cross-user draft bleed is prevented", () => {
    // Seed localStorage as if user A left a draft (already in storage before store init).
    const userAData = {
      draft: { threadId: "tid-user-a", messages: [{ role: "user", text: "user A secret" }] },
    };
    localStorage.setItem(CHAT_STORAGE_KEY, JSON.stringify(userAData));

    // Re-init stores so chat store loads user A's draft from storage.
    setActivePinia(createPinia());
    const chatStore = useChatStore();
    const userStore = useUserStore();

    // Confirm user A's draft is present before logout.
    chatStore.setActiveSession(null); // ensure draft key
    const preClear = chatStore.currentBuffer();
    expect(preClear.messages[0].text).toBe("user A secret");

    // User A logs out.
    userStore.clear();

    // Storage must be gone.
    expect(localStorage.getItem(CHAT_STORAGE_KEY)).toBeNull();
    // currentBuffer() for user B must be a fresh empty buffer.
    expect(chatStore.currentBuffer().messages.length).toBe(0);
    expect(chatStore.activeSessionId).toBe(null);
  });
});
