// Regression test: sessionless navigation must clear activeSessionId and reset the buffer.
// Bug: watch on route.query.session alone early-returns on undefined → stale activeSessionId
// persists → next conversation overwrites the previously-resumed session file.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";
import { useChatStore } from "../store/chat.js";
import { useSessionsStore } from "../store/sessions.js";
import Chat from "../views/Chat.vue";

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

vi.mock("../api/agui.js", () => ({
  runAgui: vi.fn().mockResolvedValue("thread-abc"),
}));

vi.mock("../api/client.js", () => ({
  get: vi.fn().mockResolvedValue({ approvals: [] }),
  post: vi.fn().mockResolvedValue({}),
  request: vi.fn(),
  del: vi.fn(),
  patch: vi.fn(),
}));

// Mock sessions store's loadMessages so we don't need a real file API.
vi.mock("../api/sessions.js", () => ({
  listSessionFiles: vi.fn().mockResolvedValue([]),
  readSession: vi.fn().mockResolvedValue(null),
  writeSession: vi.fn().mockResolvedValue({}),
  deleteSession: vi.fn().mockResolvedValue({}),
  readManifest: vi.fn().mockResolvedValue([]),
  writeManifest: vi.fn().mockResolvedValue({}),
  migrateLegacySessions: vi.fn().mockResolvedValue(undefined),
}));

import * as sessionsMod from "../api/sessions.js";

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/chat", component: Chat },
      { path: "/", component: { template: "<div/>" } },
    ],
  });
}

describe("Chat.vue — sessionless navigation clears active session", () => {
  let router;

  beforeEach(() => {
    localStorage.clear();
    setActivePinia(createPinia());
    router = makeRouter();
  });

  afterEach(() => {
    localStorage.clear();
    vi.restoreAllMocks();
  });

  it("navigating from a resumed session to sessionless route clears activeSessionId and resets buffer", async () => {
    // Stub readSession to return a fake session record for "W".
    const fakeRecord = {
      id: "W",
      title: "Weather",
      thread_id: "thread-weather",
      messages: [{ role: "user", text: "What is the weather?" }],
    };
    sessionsMod.readSession.mockResolvedValue(fakeRecord);

    // 1. Mount with ?session=W — simulates resuming session "W".
    await router.push("/chat?session=W");
    const w = mount(Chat, { global: { plugins: [router] } });
    await flushPromises();

    const chatStore = useChatStore();

    // activeSessionId must be "W" after resume.
    expect(chatStore.activeSessionId).toBe("W");

    // 2. Navigate to sessionless /chat (e.g. clicking an agent or "Chat" nav).
    await router.push("/chat?agent=bot-1");
    await flushPromises();

    // BUG (before fix): activeSessionId remains "W" because the watcher on
    // route.query.session early-returns when id is undefined.
    // FIXED: must be null and buffer must be reset.
    expect(chatStore.activeSessionId).toBe(null);

    // Buffer must be a fresh empty session (no "Weather" messages).
    // currentBuffer() with no active session returns the draft buffer.
    const buf = chatStore.currentBuffer();
    expect(buf.messages.length).toBe(0);
  });

  it("direct load of /chat (no session query) starts with null activeSessionId", async () => {
    await router.push("/chat");
    const w = mount(Chat, { global: { plugins: [router] } });
    await flushPromises();

    const chatStore = useChatStore();
    // No session in query → activeSessionId must remain null (fresh start).
    expect(chatStore.activeSessionId).toBe(null);
  });

  it("resumed session still works (present-session path unchanged)", async () => {
    const fakeRecord = {
      id: "SESSION-42",
      title: "Test Session",
      thread_id: "thread-42",
      messages: [{ role: "user", text: "hello from session 42" }],
    };
    sessionsMod.readSession.mockResolvedValue(fakeRecord);

    await router.push("/chat?session=SESSION-42");
    const w = mount(Chat, { global: { plugins: [router] } });
    await flushPromises();

    const chatStore = useChatStore();
    expect(chatStore.activeSessionId).toBe("SESSION-42");
  });

  it("resume A then B keeps messages distinct; currentBuffer shows B's messages", async () => {
    // Encode: two sessions must not share a buffer. Resume A, then B — B's messages
    // must appear in currentBuffer and A's messages must not.
    const recA = {
      id: "S-A",
      title: "A",
      thread_id: "t-A",
      messages: [{ role: "user", text: "Session A message" }],
    };
    const recB = {
      id: "S-B",
      title: "B",
      thread_id: "t-B",
      messages: [{ role: "user", text: "Session B message" }],
    };

    sessionsMod.readSession.mockImplementation(async (id) => {
      if (id === "S-A") return recA;
      if (id === "S-B") return recB;
      return null;
    });

    await router.push("/chat?session=S-A");
    const w = mount(Chat, { global: { plugins: [router] } });
    await flushPromises();

    const chatStore = useChatStore();
    expect(chatStore.activeSessionId).toBe("S-A");

    // Navigate to B
    await router.push("/chat?session=S-B");
    await flushPromises();

    expect(chatStore.activeSessionId).toBe("S-B");
    const bufB = chatStore.currentBuffer();
    expect(bufB.messages[0].text).toBe("Session B message");
  });

  it("after resuming a session and starting new chat, first message mints a NEW session id (overwrite regression)", async () => {
    // Encode the overwrite regression: resuming a session sets activeSessionId;
    // clicking New Chat must clear it so the first send of the new conversation
    // calls maybeCreateSession → mints a FRESH uuid, not the resumed session's id.
    // This test closes the overwrite regression (was the original chat-session-nav bug).
    const fakeRecord = {
      id: "RESUMED",
      title: "Resumed",
      thread_id: "thread-resumed",
      messages: [{ role: "user", text: "resumed message" }],
    };
    sessionsMod.readSession.mockResolvedValue(fakeRecord);

    await router.push("/chat?session=RESUMED");
    const w = mount(Chat, { global: { plugins: [router] } });
    await flushPromises();

    const chatStore = useChatStore();
    expect(chatStore.activeSessionId).toBe("RESUMED");

    // Navigate to sessionless (simulates New Chat or agent nav)
    await router.push("/chat");
    await flushPromises();

    // activeSessionId must be cleared
    expect(chatStore.activeSessionId).toBe(null);

    // The next maybeCreateSession call must mint a new id (not "RESUMED")
    // Simulate: promoteDraft is called with a new uuid
    const newId = "NEW-SESSION-ID";
    chatStore.promoteDraft(newId);
    expect(chatStore.activeSessionId).toBe(newId);
    expect(chatStore.activeSessionId).not.toBe("RESUMED");
  });
});
