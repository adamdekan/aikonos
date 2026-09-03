// Per-message actions: assistant copy/reply, user edit-and-resend.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
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
vi.mock("../api/agui.js", () => ({ runAgui: (...args) => mockRunAgui(...args) }));

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

function simulateStream(handlers, events) {
  for (const [name, ...args] of events) {
    if (handlers[name]) handlers[name](...args);
  }
  return Promise.resolve("thread-abc");
}

// A full one-turn exchange producing an assistant reply.
function streamReply(text) {
  return (_opts, handlers) =>
    simulateStream(handlers, [
      ["onRunStarted"],
      ["onTextStart"],
      ["onText", text],
      ["onTextEnd"],
      ["onFinished"],
    ]);
}

describe("Chat.vue — per-message actions", () => {
  let router;

  beforeEach(() => {
    localStorage.clear();
    setActivePinia(createPinia());
    router = makeRouter();
    mockRunAgui = vi.fn().mockImplementation((_opts, _h) => Promise.resolve("thread-abc"));
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

  it("copy action writes assistant text to the clipboard", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", {
      value: { writeText },
      configurable: true,
    });
    mockRunAgui = vi.fn().mockImplementation(streamReply("copy me"));

    const w = await mountChat("hi");
    await flushPromises();

    await w.find("[data-testid='msg-copy']").trigger("click");
    expect(writeText).toHaveBeenCalledWith("copy me");
  });

  it("reply action seeds the composer with a quoted follow-up", async () => {
    mockRunAgui = vi.fn().mockImplementation(streamReply("first line\nsecond line"));

    const w = await mountChat("hi");
    await flushPromises();

    await w.find("[data-testid='msg-reply']").trigger("click");
    await flushPromises();

    const ta = w.find("textarea.composer-input");
    expect(ta.element.value).toBe("> first line\n\n");
  });

  it("editing a user message re-runs the conversation from that point", async () => {
    mockRunAgui = vi.fn().mockImplementation(streamReply("resp one"));
    const w = await mountChat("original");
    await flushPromises();
    expect(mockRunAgui).toHaveBeenCalledTimes(1);

    // Enter edit mode on the user bubble, change the text, save.
    await w.find("[data-testid='msg-edit']").trigger("click");
    const editBox = w.find("[data-testid='edit-input']");
    await editBox.setValue("revised prompt");
    await w.find("[data-testid='edit-save']").trigger("click");
    await flushPromises();

    expect(mockRunAgui).toHaveBeenCalledTimes(2);
    const secondCall = mockRunAgui.mock.calls[1][0];
    expect(secondCall.prompt).toBe("revised prompt");
    // The prior turn was discarded, so history is empty on the re-run.
    expect(secondCall.history).toEqual([]);
    // Only one user bubble remains, carrying the revised text.
    expect(w.text()).toContain("revised prompt");
    expect(w.text()).not.toContain("original");
  });
});
