import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";
import { useChatStore, CHAT_STORAGE_KEY } from "../store/chat.js";
import Chat from "../views/Chat.vue";

vi.mock("../api/agui.js", () => ({
  runAgui: vi.fn().mockResolvedValue("thread-abc"),
}));
vi.mock("vue-stream-markdown", () => ({
  Markdown: {
    props: ["content"],
    template: "<span>{{ content }}</span>",
  },
}));
vi.mock("vue-stream-markdown/index.css", () => ({}));
// Chat.vue mounts Composer, whose onMounted unconditionally loads the workspace
// store — mock it here so this suite
// doesn't leak a real (token-less) client.js call.
vi.mock("../api/workspace.js", () => ({
  getWorkspaceBackend: () => Promise.resolve({
    pref: { backend: "local", onedriveFolderPath: "" },
    onedriveAvailable: false,
    onedriveStatus: "",
  }),
  setWorkspaceBackend: () => Promise.resolve({}),
  listOneDriveFolders: () => Promise.resolve({}),
}));
vi.mock("../api/client.js", () => ({
  get: vi.fn().mockResolvedValue({ approvals: [] }),
  post: vi.fn().mockResolvedValue({}),
  request: vi.fn(),
  del: vi.fn(),
  patch: vi.fn(),
}));

import * as aguiMod from "../api/agui.js";

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [{ path: "/chat", component: Chat }, { path: "/", component: { template: "<div/>" } }],
  });
}

function simulateStream(handlers, events) {
  for (const [name, ...args] of events) {
    if (handlers[name]) handlers[name](...args);
  }
  return Promise.resolve("thread-abc");
}

describe("Chat.vue — per-session buffer persistence", () => {
  let router;

  beforeEach(() => {
    localStorage.clear();
    setActivePinia(createPinia());
    router = makeRouter();
    aguiMod.runAgui.mockImplementation((_opts, _handlers) => Promise.resolve("thread-abc"));
  });

  afterEach(() => {
    localStorage.clear();
    vi.restoreAllMocks();
  });

  it("sending a message stores it in the current session buffer in localStorage (no '::' key)", async () => {
    aguiMod.runAgui.mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [["onRunStarted"], ["onText", "reply"], ["onFinished"]])
    );

    await router.push("/chat");
    const w = mount(Chat, { global: { plugins: [router] } });
    await flushPromises();

    const composer = w.find("textarea");
    await composer.setValue("hello world");
    await w.find("form").trigger("submit");
    await flushPromises();

    // Check via store — currentBuffer() holds the message
    const chatStore = useChatStore();
    const buf = chatStore.currentBuffer();
    expect(buf.messages.some(m => m.role === "user" && m.text === "hello world")).toBe(true);

    // localStorage must be persisted with no '::' keys (new schema only)
    const raw = localStorage.getItem(CHAT_STORAGE_KEY);
    expect(raw).toBeTruthy();
    const parsed = JSON.parse(raw);
    const hasColonColon = Object.keys(parsed).some(k => k.includes("::"));
    expect(hasColonColon).toBe(false);
  });

  it("mounting Chat at bare /chat (no ?session=) always starts a fresh buffer", async () => {
    // Correct contract: sessionless nav resets the buffer so a previously-resumed
    // session is never accidentally overwritten. Resume requires ?session=<id>.
    const chatStore = useChatStore();

    // Pre-seed the draft buffer with leftover state from a prior conversation.
    const draft = chatStore.currentBuffer();
    draft.messages.push({ role: "user", text: "prior user message" });
    chatStore.persist();

    await router.push("/chat");
    const w = mount(Chat, { global: { plugins: [router] } });
    await flushPromises();

    // Buffer must be cleared on sessionless mount — prior messages must NOT appear.
    expect(w.text()).not.toContain("prior user message");
    // The store's live buffer is now empty (reset).
    expect(chatStore.currentBuffer().messages.length).toBe(0);
  });

  it("uses the same threadId across multiple sends (threadId stable from currentBuffer)", () => {
    // Encode: threadId is created once on first currentBuffer() call and never
    // changed mid-conversation. Two sends to the same session share one threadId.
    return (async () => {
      const capturedOpts = [];
      aguiMod.runAgui.mockImplementation((opts, handlers) => {
        capturedOpts.push({ ...opts });
        return simulateStream(handlers, [["onText", "ok"], ["onFinished"]]);
      });

      await router.push("/chat");
      const w = mount(Chat, { global: { plugins: [router] } });
      await flushPromises();

      const composer = w.find("textarea");
      await composer.setValue("first");
      await w.find("form").trigger("submit");
      await flushPromises();

      await composer.setValue("second");
      await w.find("form").trigger("submit");
      await flushPromises();

      expect(capturedOpts.length).toBe(2);
      expect(capturedOpts[0].threadId).toBe(capturedOpts[1].threadId);

      // threadId must match what the store has (currentBuffer gives the canonical value)
      const chatStore = useChatStore();
      expect(capturedOpts[0].threadId).toBe(chatStore.currentBuffer().threadId);
    })();
  });

  it("sends the active session id from the FIRST send (read after maybeCreateSession promotes the draft)", async () => {
    // Encode: spend attribution must cover
    // the first turn too. activeSessionId is null until maybeCreateSession
    // promotes the draft buffer, so reading it before that await would silently
    // ship an unattributed first turn on every new conversation.
    const capturedOpts = [];
    aguiMod.runAgui.mockImplementation((opts, handlers) => {
      capturedOpts.push({ ...opts });
      return simulateStream(handlers, [["onText", "ok"], ["onFinished"]]);
    });

    await router.push("/chat");
    const w = mount(Chat, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("textarea").setValue("first");
    await w.find("form").trigger("submit");
    await flushPromises();

    const chatStore = useChatStore();
    expect(chatStore.activeSessionId).toBeTruthy();
    expect(capturedOpts.length).toBe(1);
    expect(capturedOpts[0].sessionId).toBe(chatStore.activeSessionId);
  });
});
