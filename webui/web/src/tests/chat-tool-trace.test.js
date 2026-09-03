// Live agent-action visibility — tool calls render as grey trace lines in the
// assistant message, with animated dots while a call (or the run) is pending.
// Drives the runAgui handlers through a mounted Chat, mirroring chat-loss-signal.
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

describe("Chat.vue — live tool trace", () => {
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

  it("renders one grey line per tool, labelled by the friendly phrase, with a settled result", async () => {
    mockRunAgui = vi.fn().mockImplementation(async (_opts, handlers) => {
      handlers.onRunStarted();
      handlers.onToolCall({ id: "tc-1", name: "web_fetch", description: "Fetch a public web page over HTTPS." });
      handlers.onToolResult({ id: "tc-1", content: "done", isError: false });
      handlers.onFinished();
      return "thread-abc";
    });

    const w = await mountChat("go");
    await flushPromises();

    const trace = w.find("[data-testid='tool-trace']");
    expect(trace.exists()).toBe(true);
    expect(trace.text()).toContain("Reading a web page");
  });

  it("shows standalone dots on RUN_STARTED before any text or tool event (empty placeholder)", async () => {
    // Invariant: the empty assistant placeholder created on RUN_STARTED renders
    // the transcript progress dots immediately — no footer indicator, no wait
    // for the first tool event. Pause the stream right after RUN_STARTED.
    let release;
    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) => {
      handlers.onRunStarted();
      return new Promise((res) => { release = res; });
    });

    const w = await mountChat("go");
    await flushPromises();
    await nextTick();

    expect(w.find("[data-testid='tool-trace-standalone-dots']").exists()).toBe(true);

    await w.findComponent({ name: "Composer" }).vm.$emit("stop");
    release("thread-abc");
    await flushPromises();
    expect(w.find("[data-testid='tool-trace-standalone-dots']").exists()).toBe(false);
  });

  it("shows standalone dots while the run is active and no tool is running (no text yet)", async () => {
    let release;
    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) => {
      handlers.onRunStarted();
      // A tool completed; now waiting on the model's next step — dots must show.
      handlers.onToolCall({ id: "tc-1", name: "web_fetch", description: "Fetch a page" });
      handlers.onToolResult({ id: "tc-1", content: "ok", isError: false });
      return new Promise((res) => { release = res; });
    });

    const w = await mountChat("go");
    await flushPromises();
    await nextTick();

    expect(w.find("[data-testid='tool-trace-standalone-dots']").exists()).toBe(true);

    // Finish the run — dots disappear.
    await w.findComponent({ name: "Composer" }).vm.$emit("stop");
    release("thread-abc");
    await flushPromises();
    expect(w.find("[data-testid='tool-trace-standalone-dots']").exists()).toBe(false);
  });

  it("labels load_skill as 'Loading skill: ' + first 5 words of the bundle description", async () => {
    mockRunAgui = vi.fn().mockImplementation(async (_opts, handlers) => {
      handlers.onRunStarted();
      handlers.onToolCall({
        id: "tc-1",
        name: "load_skill",
        description: "Process supplier invoices end to end automatically for the finance team",
      });
      handlers.onToolResult({ id: "tc-1", content: "activated", isError: false });
      handlers.onFinished();
      return "thread-abc";
    });

    const w = await mountChat("go");
    await flushPromises();

    const trace = w.find("[data-testid='tool-trace']");
    expect(trace.text()).toContain("Loading skill: Process supplier invoices end to");
  });
});
