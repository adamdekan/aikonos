import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { usePromptStore } from "../store/prompt.js";
import { useChatStore } from "../store/chat.js";
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

let mockRunAgui;
vi.mock("../api/agui.js", () => ({
  runAgui: (...args) => mockRunAgui(...args),
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

describe("Chat.vue — history sent with submit", () => {
  let router;

  beforeEach(() => {
    localStorage.clear();
    setActivePinia(createPinia());
    router = makeRouter();
    mockRunAgui = vi.fn().mockResolvedValue("thread-abc");
  });

  afterEach(() => {
    localStorage.clear();
    vi.restoreAllMocks();
  });

  async function mountChat() {
    await router.push("/chat");
    const w = mount(Chat, { global: { plugins: [router] } });
    await flushPromises();
    return w;
  }

  it("first submit sends empty history (no prior turns)", async () => {
    const capturedOpts = [];
    mockRunAgui = vi.fn().mockImplementation((opts, _handlers) => {
      capturedOpts.push({ ...opts });
      return Promise.resolve("thread-abc");
    });

    const w = await mountChat();
    const textarea = w.find("textarea");
    await textarea.setValue("Hello");
    await w.find("form").trigger("submit");
    await flushPromises();

    expect(capturedOpts).toHaveLength(1);
    // First message: no prior turns, so Chat builds an empty history array. The []→omitted-from-body wire behavior is covered in agui-history.test.js.
    expect(capturedOpts[0].history).toHaveLength(0);
  });

  it("second submit carries first user+assistant turn as history, excluding the new prompt", async () => {
    const capturedOpts = [];
    mockRunAgui = vi.fn().mockImplementation((opts, handlers) => {
      capturedOpts.push({ ...opts, history: opts.history ? [...opts.history] : opts.history });
      // Simulate a minimal assistant response so the buffer gets an assistant turn.
      handlers.onRunStarted?.();
      handlers.onTextStart?.();
      handlers.onText?.("I got it");
      handlers.onTextEnd?.();
      handlers.onFinished?.();
      return Promise.resolve("thread-abc");
    });

    const w = await mountChat();

    // First message.
    const textarea = w.find("textarea");
    await textarea.setValue("first question");
    await w.find("form").trigger("submit");
    await flushPromises();

    // Second message.
    await textarea.setValue("follow-up");
    await w.find("form").trigger("submit");
    await flushPromises();

    expect(capturedOpts).toHaveLength(2);

    // First call: no prior turns.
    expect(capturedOpts[0].history).toEqual([]);

    // Second call: history = [user "first question", assistant "I got it"].
    // Must NOT include "follow-up" (the new prompt).
    const h = capturedOpts[1].history;
    expect(h).toHaveLength(2);
    expect(h[0]).toEqual({ role: "user", content: "first question" });
    expect(h[1]).toEqual({ role: "assistant", content: "I got it" });
  });

  it("filters out buffer entries without text (tools-only assistant turns)", async () => {
    const capturedOpts = [];
    mockRunAgui = vi.fn().mockImplementation((opts, handlers) => {
      capturedOpts.push({ ...opts, history: opts.history ? [...opts.history] : opts.history });
      // Simulate an assistant turn with no text (tool only — text stays "").
      handlers.onRunStarted?.();
      handlers.onToolCall?.({ id: "tc1", name: "web.fetch" });
      handlers.onToolResult?.({ id: "tc1", content: "result", isError: false });
      handlers.onFinished?.();
      return Promise.resolve("thread-abc");
    });

    const w = await mountChat();

    await w.find("textarea").setValue("do a fetch");
    await w.find("form").trigger("submit");
    await flushPromises();

    await w.find("textarea").setValue("now tell me more");
    await w.find("form").trigger("submit");
    await flushPromises();

    expect(capturedOpts).toHaveLength(2);
    const h = capturedOpts[1].history;
    // The tools-only assistant turn (empty text) must be filtered out.
    // Only the user "do a fetch" turn has text.
    expect(h.every((m) => m.content && m.content.length > 0)).toBe(true);
    expect(h.find((m) => m.role === "assistant")).toBeUndefined();
    expect(h.find((m) => m.role === "user" && m.content === "do a fetch")).toBeDefined();
  });
});
