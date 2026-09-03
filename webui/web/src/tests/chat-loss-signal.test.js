// F36 — connection-lost marking for verdict-less chat streams (CP2).
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { nextTick } from "vue";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { usePromptStore } from "../store/prompt.js";
import Chat from "../views/Chat.vue";

vi.mock("vue-stream-markdown", () => ({
  Markdown: { props: ["content"], template: "<span>{{ content }}</span>" },
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

let mockRunAgui;
vi.mock("../api/agui.js", () => ({
  runAgui: (...args) => mockRunAgui(...args),
}));

vi.mock("../api/inbox.js", () => ({
  listInbox: vi.fn().mockResolvedValue({ items: [] }),
  delegate: vi.fn().mockResolvedValue({ ok: true }),
}));

vi.mock("../api/client.js", () => ({
  get: vi.fn().mockResolvedValue({ approvals: [] }),
  post: vi.fn().mockResolvedValue({}),
  request: vi.fn(),
  del: vi.fn(),
  patch: vi.fn(),
}));

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [{ path: "/chat", component: Chat }, { path: "/", component: { template: "<div/>" } }],
  });
}

describe("Chat.vue — connection-lost marking (F36 CP2)", () => {
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

  async function mountChat(prompt = "") {
    if (prompt) usePromptStore().set(prompt);
    await router.push("/chat");
    const w = mount(Chat, { global: { plugins: [router] } });
    await flushPromises();
    return w;
  }

  it("stream resolving without a verdict marks the assistant message with a connection-lost error", async () => {
    mockRunAgui = vi.fn().mockImplementation(async (_opts, handlers) => {
      handlers.onRunStarted();
      handlers.onTextStart();
      handlers.onText("partial reply");
      // Resolve without ever calling onFinished/onError — simulates a dropped stream.
      return "thread-abc";
    });

    const w = await mountChat("hello");
    await flushPromises();

    expect(w.text()).toContain("Connection lost");
    expect(w.text()).toContain("partial reply");
  });

  it("user abort keeps quiet behavior — no connection-lost error", async () => {
    let resolveStream;
    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) => {
      handlers.onRunStarted();
      return new Promise((res) => { resolveStream = res; });
    });

    const w = await mountChat("hello");
    await flushPromises();
    await nextTick();

    // Simulate the user clicking Stop, which aborts and resolves runAgui
    // the way a real abort eventually would. Trigger stop via the Composer's stop emit.
    await w.findComponent({ name: "Composer" }).vm.$emit("stop");
    resolveStream("thread-abc");
    await flushPromises();

    expect(w.text()).not.toContain("Connection lost");
  });

  it("normal RUN_FINISHED stream is unaffected", async () => {
    mockRunAgui = vi.fn().mockImplementation(async (_opts, handlers) => {
      handlers.onRunStarted();
      handlers.onTextStart();
      handlers.onText("all good");
      handlers.onTextEnd();
      handlers.onFinished();
      return "thread-abc";
    });

    const w = await mountChat("hello");
    await flushPromises();

    expect(w.text()).toContain("all good");
    expect(w.text()).not.toContain("Connection lost");
  });
});
