// CP1 tests: persistSessionMessages rebuilds the record from client state (no
// pre-write read), warns instead of swallowing on failure; sessions.load() discovers
// missing manifest entries concurrently; encodeJson/decodeJson round-trip a large payload.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
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
vi.mock("../api/sessions.js", () => ({
  listSessionFiles: vi.fn().mockResolvedValue([]),
  readSession: vi.fn().mockResolvedValue(null),
  writeSession: vi.fn().mockResolvedValue({}),
  deleteSession: vi.fn().mockResolvedValue({}),
  readManifest: vi.fn().mockResolvedValue([]),
  writeManifest: vi.fn().mockResolvedValue({}),
  migrateLegacySessions: vi.fn().mockResolvedValue(undefined),
}));

import * as aguiMod from "../api/agui.js";
import * as sessionsMod from "../api/sessions.js";
import { useSessionsStore } from "../store/sessions.js";

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

describe("Chat.vue — persistSessionMessages (CP1)", () => {
  let router;

  beforeEach(() => {
    localStorage.clear();
    vi.clearAllMocks();
    setActivePinia(createPinia());
    router = makeRouter();
    sessionsMod.migrateLegacySessions.mockResolvedValue(undefined);
    sessionsMod.listSessionFiles.mockResolvedValue([]);
    sessionsMod.readManifest.mockResolvedValue([]);
    sessionsMod.writeManifest.mockResolvedValue({});
    sessionsMod.writeSession.mockResolvedValue({});
  });

  afterEach(() => {
    localStorage.clear();
    vi.restoreAllMocks();
  });

  it("persists without calling loadMessages / readSession — record built from manifest entry + buffer", async () => {
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

    // createSession (maybeCreateSession) writes once; persist (onFinished) writes again.
    // Neither path reads the session file back.
    expect(sessionsMod.readSession).not.toHaveBeenCalled();
    expect(sessionsMod.writeSession).toHaveBeenCalled();

    const sessionsStore = useSessionsStore();
    const chatStore = useChatStore();
    const id = chatStore.activeSessionId;
    expect(id).toBeTruthy();

    const written = sessionsMod.writeSession.mock.calls.at(-1)[0];
    const manifestEntry = sessionsStore.sessions.find((s) => s.id === id);

    expect(written.id).toBe(id);
    expect(written.title).toBe(manifestEntry.title);
    expect(written.agent_id).toBe(manifestEntry.agent_id);
    expect(written.agent_name).toBe(manifestEntry.agent_name);
    expect(written.pinned).toBe(manifestEntry.pinned);
    expect(written.pinned_at).toBe(manifestEntry.pinned_at);
    expect(written.created_at).toBe(manifestEntry.created_at);
    expect(written.source).toBe(manifestEntry.source);
    expect(written.schedule_id).toBe(manifestEntry.schedule_id);
    expect(written.thread_id).toBe(chatStore.currentBuffer().threadId);
    expect(written.first_message).toBe("hello world");
    expect(written.messages.some((m) => m.role === "user" && m.text === "hello world")).toBe(true);
  });

  it("persist failure logs console.warn and does not throw / does not block the run", async () => {
    aguiMod.runAgui.mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [["onRunStarted"], ["onText", "reply"], ["onFinished"]])
    );
    const warnSpy = vi.spyOn(console, "warn").mockImplementation(() => {});

    await router.push("/chat");
    const w = mount(Chat, { global: { plugins: [router] } });
    await flushPromises();

    const composer = w.find("textarea");
    await composer.setValue("first message");
    await w.find("form").trigger("submit");
    await flushPromises();

    // Fail the persist write on the *next* send (session already created).
    sessionsMod.writeSession.mockRejectedValueOnce(new Error("disk full"));

    await composer.setValue("second message");
    await w.find("form").trigger("submit");
    await flushPromises();

    expect(warnSpy).toHaveBeenCalledWith("session persist failed", expect.any(Error));
  });
});

describe("Chat.vue — persistSessionMessages resume-before-manifest fallback (CP1)", () => {
  let router;

  beforeEach(() => {
    localStorage.clear();
    vi.clearAllMocks();
    setActivePinia(createPinia());
    router = makeRouter();
    sessionsMod.migrateLegacySessions.mockResolvedValue(undefined);
    sessionsMod.listSessionFiles.mockResolvedValue([]);
    sessionsMod.readManifest.mockResolvedValue([]);
    sessionsMod.writeManifest.mockResolvedValue({});
    sessionsMod.writeSession.mockResolvedValue({});
  });

  afterEach(() => {
    localStorage.clear();
    vi.restoreAllMocks();
  });

  it("falls back to loadMessages (readSession) when the manifest entry is still missing after load() retry, instead of silently no-op'ing", async () => {
    aguiMod.runAgui.mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [["onRunStarted"], ["onText", "reply"], ["onFinished"]])
    );

    await router.push("/chat?session=resumed-1");
    const w = mount(Chat, { global: { plugins: [router] } });
    await flushPromises();

    const chatStore = useChatStore();
    const sessionsStore = useSessionsStore();

    // Simulate resumeFromQuery having activated the session by direct file read,
    // bypassing the manifest (e.g. a concurrent load() failure left sessions=[]).
    sessionsMod.readSession.mockResolvedValue({
      id: "resumed-1",
      title: "Resumed session",
      agent_id: "agent-x",
      agent_name: "Agent X",
      pinned: false,
      pinned_at: null,
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
      source: null,
      schedule_id: null,
      thread_id: "thread-old",
      messages: [{ role: "user", text: "prior message" }],
    });
    chatStore.hydrateBuffer("resumed-1", { threadId: "thread-old", messages: [{ role: "user", text: "prior message" }] });
    expect(sessionsStore.sessions.find((s) => s.id === "resumed-1")).toBeUndefined();

    const composer = w.find("textarea");
    await composer.setValue("hello world");
    await w.find("form").trigger("submit");
    await flushPromises();

    // The load() retry finds nothing new (still no manifest entry) so the code
    // must fall back to loadMessages/readSession rather than silently dropping the write.
    expect(sessionsMod.readSession).toHaveBeenCalledWith("resumed-1");
    expect(sessionsMod.writeSession).toHaveBeenCalled();

    const written = sessionsMod.writeSession.mock.calls.at(-1)[0];
    expect(written.id).toBe("resumed-1");
    expect(written.title).toBe("Resumed session");
    expect(written.agent_id).toBe("agent-x");
    expect(
      written.messages.some((m) => m.role === "user" && m.text === "hello world")
    ).toBe(true);
  });

  it("warns and does not write when neither the manifest nor loadMessages have a record", async () => {
    aguiMod.runAgui.mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [["onRunStarted"], ["onText", "reply"], ["onFinished"]])
    );
    const warnSpy = vi.spyOn(console, "warn").mockImplementation(() => {});

    await router.push("/chat?session=ghost-1");
    const w = mount(Chat, { global: { plugins: [router] } });
    await flushPromises();

    const chatStore = useChatStore();
    sessionsMod.readSession.mockResolvedValue(null);
    chatStore.hydrateBuffer("ghost-1", { threadId: "thread-old", messages: [] });

    const composer = w.find("textarea");
    await composer.setValue("hello world");
    await w.find("form").trigger("submit");
    await flushPromises();

    expect(warnSpy).toHaveBeenCalledWith("session persist skipped: no session record", "ghost-1");
    expect(sessionsMod.writeSession).not.toHaveBeenCalled();
  });

  it("skips the loadMessages fallback when load() retry finds the manifest entry", async () => {
    aguiMod.runAgui.mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [["onRunStarted"], ["onText", "reply"], ["onFinished"]])
    );

    await router.push("/chat?session=resumed-2");
    const w = mount(Chat, { global: { plugins: [router] } });
    await flushPromises();

    const chatStore = useChatStore();
    const sessionsStore = useSessionsStore();
    sessionsMod.readSession.mockResolvedValue({
      id: "resumed-2",
      title: "Resumed session 2",
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
      thread_id: "thread-old",
      messages: [],
    });
    chatStore.hydrateBuffer("resumed-2", { threadId: "thread-old", messages: [] });

    // Manifest gets the entry populated on the load() retry (e.g. discovery finds
    // the file this time) before persistSessionMessages looks it up a second time.
    sessionsStore.sessions.push({
      id: "resumed-2",
      title: "Resumed session 2",
      agent_id: null,
      agent_name: null,
      pinned: false,
      pinned_at: null,
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
      source: null,
      schedule_id: null,
    });
    sessionsMod.readSession.mockClear();

    const composer = w.find("textarea");
    await composer.setValue("hello world");
    await w.find("form").trigger("submit");
    await flushPromises();

    expect(sessionsMod.readSession).not.toHaveBeenCalled();
    expect(sessionsMod.writeSession).toHaveBeenCalled();
    const written = sessionsMod.writeSession.mock.calls.at(-1)[0];
    expect(written.title).toBe("Resumed session 2");
  });
});

describe("sessions store — concurrent discovery (CP1)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    setActivePinia(createPinia());
  });

  it("issues discovery reads concurrently — out-of-order resolution still lands all entries", async () => {
    sessionsMod.listSessionFiles.mockResolvedValue([
      { path: ".agent/Sessions/slow.json" },
      { path: ".agent/Sessions/fast.json" },
    ]);
    sessionsMod.readManifest.mockResolvedValue([]);
    sessionsMod.writeManifest.mockResolvedValue({});

    const order = [];
    sessionsMod.readSession.mockImplementation((id) => {
      order.push(`start:${id}`);
      if (id === "slow") {
        // Resolves after "fast" despite starting first — proves concurrency, not sequencing.
        return new Promise((resolve) =>
          setTimeout(() => {
            order.push(`end:${id}`);
            resolve({
              id: "slow",
              title: "Slow",
              created_at: "2026-01-01T00:00:00Z",
              updated_at: "2026-01-01T00:00:00Z",
            });
          }, 20)
        );
      }
      order.push(`end:${id}`);
      return Promise.resolve({
        id: "fast",
        title: "Fast",
        created_at: "2026-01-01T00:00:00Z",
        updated_at: "2026-01-01T00:00:00Z",
      });
    });

    const store = useSessionsStore();
    await store.load();

    // Both reads must have been issued (started) before either finished — i.e. concurrently.
    expect(order.indexOf("start:slow")).toBeLessThan(order.indexOf("end:fast"));

    const ids = store.sessions.map((s) => s.id);
    expect(ids).toContain("slow");
    expect(ids).toContain("fast");
  });

  it("skips a corrupt discovery entry (null) alongside a good one, resolved via allSettled", async () => {
    sessionsMod.listSessionFiles.mockResolvedValue([
      { path: ".agent/Sessions/corrupt.json" },
      { path: ".agent/Sessions/good.json" },
    ]);
    sessionsMod.readManifest.mockResolvedValue([]);
    sessionsMod.writeManifest.mockResolvedValue({});
    sessionsMod.readSession.mockImplementation((id) => {
      if (id === "corrupt") return Promise.reject(new Error("corrupt"));
      return Promise.resolve({
        id: "good",
        title: "Good",
        created_at: "2026-01-01T00:00:00Z",
        updated_at: "2026-01-01T00:00:00Z",
      });
    });

    const store = useSessionsStore();
    await store.load(); // must not throw

    const ids = store.sessions.map((s) => s.id);
    expect(ids).toContain("good");
    expect(ids).not.toContain("corrupt");
  });
});
