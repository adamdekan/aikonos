import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { nextTick } from "vue";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";
import { usePromptStore } from "../store/prompt.js";
import { usePrefsStore } from "../store/prefs.js";
import Chat from "../views/Chat.vue";
import ToolCard from "../components/ToolCard.vue";
import * as clientMod from "../api/client.js";

// Stub vue-stream-markdown so shiki/WASM doesn't run under jsdom.
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

// Mock the agui module before importing Chat.
let mockRunAgui;
vi.mock("../api/agui.js", () => ({
  runAgui: (...args) => mockRunAgui(...args),
}));

// Mock inbox delegate.
let mockDelegate;
vi.mock("../api/inbox.js", () => ({
  listInbox: vi.fn().mockResolvedValue({ items: [] }),
  delegate: (...args) => mockDelegate(...args),
}));

// Mock the api client so ApprovalModal's poll doesn't need a real server.
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

// Helper: fire a full simulated AG-UI stream via mockRunAgui.
// handlers is the object passed from Chat.vue to runAgui.
function simulateStream(handlers, events) {
  // Call handlers synchronously to simulate a resolved stream.
  for (const [name, ...args] of events) {
    if (handlers[name]) handlers[name](...args);
  }
  return Promise.resolve("thread-abc");
}

describe("Chat.vue — transcript rendering", () => {
  let router;

  beforeEach(() => {
    localStorage.clear();
    setActivePinia(createPinia());
    router = makeRouter();
    // Default: no-op stream.
    mockRunAgui = vi.fn().mockImplementation((_opts, _handlers) => Promise.resolve("thread-abc"));
    mockDelegate = vi.fn().mockResolvedValue({ ok: true });
  });

  afterEach(() => {
    localStorage.clear();
    vi.restoreAllMocks();
  });

  async function mountChat(prompt = "") {
    if (prompt) {
      const ps = usePromptStore();
      ps.set(prompt);
    }
    await router.push("/chat");
    const w = mount(Chat, { global: { plugins: [router] } });
    await flushPromises();
    return w;
  }

  it("mounts without error in stub state", async () => {
    const w = await mountChat();
    expect(w.exists()).toBe(true);
  });

  it("consumes promptStore.pending on mount and clears it", async () => {
    const ps = usePromptStore();
    ps.set("auto prompt");
    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [
        ["onRunStarted"],
        ["onTextStart"],
        ["onText", "Hello"],
        ["onTextEnd"],
        ["onFinished"],
      ])
    );
    const w = await mountChat();
    await flushPromises();
    expect(ps.pending).toBe("");
    expect(mockRunAgui).toHaveBeenCalled();
  });

  it("renders user bubble for submitted prompt", async () => {
    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [["onFinished"]])
    );
    const w = await mountChat();
    const composer = w.find("textarea");
    await composer.setValue("Hello agent");
    const form = w.find("form");
    await form.trigger("submit");
    await flushPromises();
    expect(w.text()).toContain("Hello agent");
  });

  it("streams assistant text into an assistant bubble", async () => {
    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [
        ["onRunStarted"],
        ["onTextStart"],
        ["onText", "Part one "],
        ["onText", "part two"],
        ["onTextEnd"],
        ["onFinished"],
      ])
    );
    const w = await mountChat("greet me");
    await flushPromises();
    expect(w.text()).toContain("Part one part two");
  });

  it("assistant text renders via MarkdownMessage (.markdown-message wrapper); user text stays plain", async () => {
    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [
        ["onRunStarted"],
        ["onTextStart"],
        ["onText", "**hello markdown**"],
        ["onTextEnd"],
        ["onFinished"],
      ])
    );
    const w = await mountChat("test markdown path");
    await flushPromises();

    // Assistant text must appear inside .markdown-message (MarkdownMessage stub).
    const mdWrapper = w.find(".markdown-message");
    expect(mdWrapper.exists()).toBe(true);
    expect(mdWrapper.text()).toContain("**hello markdown**");

    // User bubble must NOT be wrapped in .markdown-message.
    const userBubble = w.find(".bubble--user");
    expect(userBubble.exists()).toBe(true);
    expect(userBubble.find(".markdown-message").exists()).toBe(false);
  });

  it("renders a ToolCard on tool call + args + result", async () => {
    usePrefsStore().setDebugBroker(true); // broker chips are opt-in (default off)
    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [
        ["onRunStarted"],
        ["onToolCall", { id: "tc1", name: "web.fetch" }],
        ["onToolArgs", { id: "tc1", argsJson: JSON.stringify({ url: "https://example.com" }) }],
        ["onToolEnd", "tc1"],
        ["onToolResult", { id: "tc1", content: "page content", isError: false }],
        ["onFinished"],
      ])
    );
    const w = await mountChat("fetch something");
    await flushPromises();
    // Tool name should appear somewhere in the rendered output.
    expect(w.text()).toContain("web.fetch");
  });

  it("shows error state on RUN_ERROR", async () => {
    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [
        ["onRunStarted"],
        ["onError", "the agent crashed"],
      ])
    );
    const w = await mountChat("break things");
    await flushPromises();
    expect(w.text()).toContain("the agent crashed");
  });

  it("shows transcript progress dots after RUN_STARTED before any text/tool event", async () => {
    // Invariant: from RUN_STARTED until the first text/tool event, the empty
    // assistant placeholder must render ToolTrace's standalone dots in the
    // transcript (there is no separate footer indicator). We capture the wrapper
    // mid-stream by delaying finish.
    let resolveStream;
    mockRunAgui = vi.fn().mockImplementation(async (_opts, handlers) => {
      handlers.onRunStarted();
      // Pause here — progress dots should be visible in the transcript.
      await new Promise(res => { resolveStream = res; });
      handlers.onFinished();
      return "thread-abc";
    });

    const ps = usePromptStore();
    ps.set("slow");
    await router.push("/chat");
    const w = mount(Chat, { global: { plugins: [router] } });
    // Allow onMounted to start running and reach the await in runAgui.
    await flushPromises();
    await nextTick();

    expect(w.find("[data-testid='tool-trace-standalone-dots']").exists()).toBe(true);

    resolveStream();
    await flushPromises();
  });

  it("renders streaming text incrementally BEFORE run finishes (reactivity fix)", async () => {
    // This test is the lock for the addAssistant() reactivity fix.
    // It fails against the old `return msg` (raw object) code because mutations
    // on the raw object bypass Vue's reactive proxy — the DOM never updates until
    // the component re-renders for an unrelated reason.
    let captureHandlers;
    mockRunAgui = vi.fn().mockImplementation(async (_opts, handlers) => {
      captureHandlers = handlers;
      // Do NOT call onFinished — we want to assert DOM state mid-stream.
      return new Promise(() => {}); // never resolves
    });

    const ps = usePromptStore();
    ps.set("stream test");
    await router.push("/chat");
    const w = mount(Chat, { global: { plugins: [router] } });

    // Let onMounted run and reach the pending promise inside runAgui.
    await flushPromises();
    await nextTick();

    // Simulate the stream arriving mid-run (no RUN_FINISHED yet).
    captureHandlers.onRunStarted();
    captureHandlers.onTextStart();
    captureHandlers.onText("Hello ");
    captureHandlers.onText("world");

    // A single nextTick is all Vue needs to flush reactive updates.
    await nextTick();

    // The partial text must be in the DOM NOW, before RUN_FINISHED.
    expect(w.text()).toContain("Hello world");
  });

  it("keeps threadId across multiple sends (multi-turn)", async () => {
    const capturedOpts = [];
    mockRunAgui = vi.fn().mockImplementation((opts, handlers) => {
      capturedOpts.push({ ...opts });
      return simulateStream(handlers, [["onTextStart"], ["onText", "ok"], ["onTextEnd"], ["onFinished"]]);
    });
    const w = await mountChat("first");
    await flushPromises();

    const composer = w.find("textarea");
    await composer.setValue("second");
    await w.find("form").trigger("submit");
    await flushPromises();

    expect(capturedOpts.length).toBe(2);
    // Both calls must use the same threadId.
    expect(capturedOpts[0].threadId).toBeTruthy();
    expect(capturedOpts[0].threadId).toBe(capturedOpts[1].threadId);
  });
});

describe("ToolCard — compact chip + hover popover", () => {
  it("renders chip with tool name; no visible pre args by default (popover exists but hidden)", () => {
    const w = mount(ToolCard, {
      props: {
        id: "tc1",
        name: "web.fetch",
        argsJson: JSON.stringify({ url: "https://example.com" }),
        result: null,
        isError: false,
        done: false,
      },
    });
    // Chip must exist with data-testid and contain the tool name.
    const chip = w.find("[data-testid='tool-chip']");
    expect(chip.exists()).toBe(true);
    expect(chip.text()).toContain("web.fetch");

    // Popover exists in DOM (CSS-hidden) — not rendered conditionally.
    const popover = w.find("[data-testid='tool-popover']");
    expect(popover.exists()).toBe(true);
    // Chip itself must NOT directly render a <pre> with args (args live in popover only).
    expect(chip.find("pre").exists()).toBe(false);
  });

  it("popover contains pretty-printed args and pretty-printed JSON result", () => {
    const w = mount(ToolCard, {
      props: {
        id: "tc2",
        name: "web.fetch",
        argsJson: JSON.stringify({ url: "https://example.com" }),
        result: JSON.stringify({ a: 1 }),
        isError: false,
        done: true,
      },
    });
    const popover = w.find("[data-testid='tool-popover']");
    // Pretty-printed args: indented JSON.
    expect(popover.text()).toContain('"url"');
    expect(popover.text()).toContain("https://example.com");
    // Pretty-printed result: indented JSON.
    expect(popover.text()).toContain('"a"');
    expect(popover.text()).toContain("1");
    // Indented — the result pre must contain a newline (pretty format).
    const pres = popover.findAll("pre");
    expect(pres.some(p => p.text().includes("\n"))).toBe(true);
  });

  it("non-JSON result stays raw in popover", () => {
    const w = mount(ToolCard, {
      props: {
        id: "tc3",
        name: "doc.write",
        argsJson: "{}",
        result: "plain text result",
        isError: false,
        done: true,
      },
    });
    const popover = w.find("[data-testid='tool-popover']");
    expect(popover.text()).toContain("plain text result");
  });

  it("error tool: chip carries the error class", () => {
    const w = mount(ToolCard, {
      props: {
        id: "tc4",
        name: "web.fetch",
        argsJson: "{}",
        result: "network error",
        isError: true,
        done: true,
      },
    });
    const chip = w.find("[data-testid='tool-chip']");
    expect(chip.classes().some(c => c.includes("error"))).toBe(true);
  });
});

describe("Chat.vue — HITL approval dedup (cross-source)", () => {
  let router;

  beforeEach(() => {
    localStorage.clear();
    setActivePinia(createPinia());
    router = makeRouter();
    mockRunAgui = vi.fn().mockImplementation((_opts, _handlers) => Promise.resolve("thread-abc"));
    mockDelegate = vi.fn().mockResolvedValue({ ok: true });
    clientMod.get.mockResolvedValue({ approvals: [] });
  });

  afterEach(() => {
    localStorage.clear();
    vi.restoreAllMocks();
  });

  async function mountChat(prompt = "") {
    if (prompt) {
      const ps = usePromptStore();
      ps.set(prompt);
    }
    await router.push("/chat");
    const w = mount(Chat, { global: { plugins: [router] } });
    await flushPromises();
    return w;
  }

  it("CUSTOM approval then identical poll entry renders exactly one modal, approvalId stable", async () => {
    const info = {
      toolCallId: "tc-dedup-42",
      toolId: "web.fetch",
      toolName: "web.fetch",
      effectClass: 1,
      reason: "requires approval",
      args: { url: "https://example.com" },
      stepUp: false,
    };

    // Pause the stream mid-run so the poll has time to fire.
    let resolveStream;
    mockRunAgui = vi.fn().mockImplementation(async (_opts, handlers) => {
      handlers.onRunStarted();
      // Fire the CUSTOM approval event from the SSE stream.
      handlers.onApprovalRequest(info);
      // Hold the stream open so polling can occur.
      await new Promise(res => { resolveStream = res; });
      handlers.onFinished();
      return "thread-abc";
    });

    // Poll will return the same approval (same toolCallId) as the CUSTOM event.
    clientMod.get.mockResolvedValue({ approvals: [info] });

    const ps = usePromptStore();
    ps.set("need approval");
    await router.push("/chat");
    const w = mount(Chat, { global: { plugins: [router] } });
    await flushPromises();
    await nextTick();

    // One modal visible.
    expect(w.findAll(".approval-modal").length).toBe(1);

    // Advance time to let at least one poll interval fire.
    vi.useFakeTimers();
    vi.advanceTimersByTime(4000);
    await flushPromises();
    await nextTick();

    // Still exactly one modal — poll did not re-render a duplicate.
    expect(w.findAll(".approval-modal").length).toBe(1);

    // The modal's displayed toolId matches the original info.
    expect(w.text()).toContain(info.toolId);

    vi.useRealTimers();
    resolveStream();
    await flushPromises();
  });
});

describe("Chat.vue — delegation confirm modal (CP2)", () => {
  let router;
  let wrapper;

  beforeEach(() => {
    localStorage.clear();
    setActivePinia(createPinia());
    router = makeRouter();
    mockRunAgui = vi.fn().mockImplementation((_opts, _handlers) => Promise.resolve("thread-abc"));
    mockDelegate = vi.fn().mockResolvedValue({ ok: true });
    clientMod.get.mockResolvedValue({ approvals: [] });
  });

  afterEach(() => {
    // Unmount explicitly so Teleport content is removed from document.body.
    if (wrapper) { wrapper.unmount(); wrapper = null; }
    localStorage.clear();
    vi.restoreAllMocks();
  });

  async function mountChat() {
    await router.push("/chat");
    wrapper = mount(Chat, { global: { plugins: [router] }, attachTo: document.body });
    await flushPromises();
    return wrapper;
  }

  // Helper: trigger submit with a delegateTo payload by calling the Composer's submit emit.
  async function submitWithDelegate(w, text, target) {
    // Set draft text on the textarea so the component has a value for the text computation.
    const textarea = w.find("textarea");
    await textarea.setValue(text);
    // Emit the submit event from Composer with a delegateTo payload.
    await w.findComponent({ name: "Composer" }).vm.$emit("submit", { delegateTo: target });
    await nextTick();
    await flushPromises();
  }

  // Modal is teleported to document.body — query there, not via w.find().
  function bodyText() { return document.body.textContent ?? ""; }
  function bodyFind(sel) { return document.body.querySelector(sel); }

  it("submit with delegateTo opens the confirm modal without calling delegate or runAgui", async () => {
    const w = await mountChat();
    const target = { userId: "user:alice", displayName: "Alice" };

    await submitWithDelegate(w, "@Alice do the thing", target);

    // Modal must be visible (teleported to body).
    expect(bodyText()).toContain("Delegate task");
    // Neither delegate nor runAgui called yet.
    expect(mockDelegate).not.toHaveBeenCalled();
    expect(mockRunAgui).not.toHaveBeenCalled();
  });

  it("clicking Confirm calls delegate with correct args and appends assistant message, no runAgui", async () => {
    const w = await mountChat();
    const target = { userId: "user:alice", displayName: "Alice" };
    const text = "@Alice do the thing";

    await submitWithDelegate(w, text, target);

    // Click the confirm button (teleported to body).
    const confirmBtn = bodyFind("[data-testid='delegate-confirm-btn']");
    expect(confirmBtn).not.toBeNull();
    confirmBtn.click();
    await flushPromises();
    await nextTick();

    // delegate called once with expected args; intent has @Alice stripped.
    expect(mockDelegate).toHaveBeenCalledOnce();
    expect(mockDelegate).toHaveBeenCalledWith({
      to: "user:alice",
      intent: "do the thing",
      scopes: [],
      maxCost: 50,
    });

    // runAgui must NOT be called.
    expect(mockRunAgui).not.toHaveBeenCalled();

    // An assistant message with the delegated text must appear in the component.
    expect(w.text()).toContain("✓ Task delegated to Alice");
  });

  it("clicking Cancel restores draft and calls neither delegate nor runAgui", async () => {
    const w = await mountChat();
    const target = { userId: "user:alice", displayName: "Alice" };
    const text = "@Alice do the thing";

    await submitWithDelegate(w, text, target);

    // Click cancel (teleported to body).
    const cancelBtn = bodyFind("[data-testid='delegate-cancel-btn']");
    expect(cancelBtn).not.toBeNull();
    cancelBtn.click();
    await flushPromises();

    expect(mockDelegate).not.toHaveBeenCalled();
    expect(mockRunAgui).not.toHaveBeenCalled();
    // Draft must be restored to the original text.
    expect(w.find("textarea").element.value).toBe(text);
  });

  it("rejected delegate (ok:false) shows error toast path, restores draft, no runAgui", async () => {
    mockDelegate = vi.fn().mockResolvedValue({ ok: false, error: "permission denied" });
    const w = await mountChat();
    const target = { userId: "user:alice", displayName: "Alice" };
    const text = "@Alice do the thing";

    await submitWithDelegate(w, text, target);

    const confirmBtn = bodyFind("[data-testid='delegate-confirm-btn']");
    expect(confirmBtn).not.toBeNull();
    confirmBtn.click();
    await flushPromises();

    // delegate was called.
    expect(mockDelegate).toHaveBeenCalledOnce();
    // runAgui must not be called.
    expect(mockRunAgui).not.toHaveBeenCalled();
    // No assistant delegation message in the component.
    expect(w.text()).not.toContain("✓ Task delegated");
    // Modal closed (confirm button gone from body).
    expect(bodyFind("[data-testid='delegate-confirm-btn']")).toBeNull();
    // Draft restored to the original text.
    expect(w.find("textarea").element.value).toBe(text);
  });

  it("thrown delegate error shows error path and restores draft, no runAgui", async () => {
    mockDelegate = vi.fn().mockRejectedValue(new Error("network failure"));
    const w = await mountChat();
    const target = { userId: "user:alice", displayName: "Alice" };
    const text = "@Alice do the thing";

    await submitWithDelegate(w, text, target);

    const confirmBtn = bodyFind("[data-testid='delegate-confirm-btn']");
    expect(confirmBtn).not.toBeNull();
    confirmBtn.click();
    await flushPromises();

    expect(mockDelegate).toHaveBeenCalledOnce();
    expect(mockRunAgui).not.toHaveBeenCalled();
    expect(w.text()).not.toContain("✓ Task delegated");
    expect(bodyFind("[data-testid='delegate-confirm-btn']")).toBeNull();
    // Draft restored to the original text.
    expect(w.find("textarea").element.value).toBe(text);
  });
});

describe("Chat.vue — group delegation (CP4)", () => {
  let router;
  let wrapper;

  beforeEach(() => {
    localStorage.clear();
    setActivePinia(createPinia());
    router = makeRouter();
    mockRunAgui = vi.fn().mockImplementation((_opts, _handlers) => Promise.resolve("thread-abc"));
    mockDelegate = vi.fn().mockResolvedValue({ ok: true });
    clientMod.get.mockResolvedValue({ approvals: [] });
  });

  afterEach(() => {
    if (wrapper) { wrapper.unmount(); wrapper = null; }
    localStorage.clear();
    vi.restoreAllMocks();
  });

  async function mountChat() {
    await router.push("/chat");
    wrapper = mount(Chat, { global: { plugins: [router] }, attachTo: document.body });
    await flushPromises();
    return wrapper;
  }

  async function submitWithDelegate(w, text, target) {
    const textarea = w.find("textarea");
    await textarea.setValue(text);
    await w.findComponent({ name: "Composer" }).vm.$emit("submit", { delegateTo: target });
    await nextTick();
    await flushPromises();
  }

  function bodyText() { return document.body.textContent ?? ""; }
  function bodyFind(sel) { return document.body.querySelector(sel); }

  it("submit with group delegateTo opens confirm modal without calling delegate or runAgui", async () => {
    const w = await mountChat();
    const target = { groupId: "group:eng", displayName: "Engineering", memberCount: 5 };

    await submitWithDelegate(w, "@Engineering do the thing", target);

    // Modal must be visible with group-specific copy
    expect(bodyText()).toContain("Delegate task");
    expect(bodyText()).toContain("Engineering");
    expect(bodyText()).toContain("5");
    expect(mockDelegate).not.toHaveBeenCalled();
    expect(mockRunAgui).not.toHaveBeenCalled();
  });

  it("group confirm modal copy shows '(N people)' with memberCount", async () => {
    const w = await mountChat();
    const target = { groupId: "group:eng", displayName: "Engineering", memberCount: 5 };

    await submitWithDelegate(w, "@Engineering do the thing", target);

    // Must show the member count
    expect(bodyText()).toContain("5 people");
  });

  it("Confirm on group delegateTo calls delegate({ group, intent, scopes:[], maxCost:50 })", async () => {
    const w = await mountChat();
    const target = { groupId: "group:eng", displayName: "Engineering", memberCount: 5 };
    const text = "@Engineering do the thing";

    await submitWithDelegate(w, text, target);

    const confirmBtn = bodyFind("[data-testid='delegate-confirm-btn']");
    expect(confirmBtn).not.toBeNull();
    confirmBtn.click();
    await flushPromises();
    await nextTick();

    expect(mockDelegate).toHaveBeenCalledOnce();
    // intent has @Engineering stripped.
    expect(mockDelegate).toHaveBeenCalledWith({
      group: "group:eng",
      intent: "do the thing",
      scopes: [],
      maxCost: 50,
    });
    expect(mockRunAgui).not.toHaveBeenCalled();

    // Success message includes group name and member count
    expect(w.text()).toContain("✓ Task delegated to group Engineering (5 people)");
  });

  it("Cancel on group delegate restores draft and calls nothing", async () => {
    const w = await mountChat();
    const target = { groupId: "group:eng", displayName: "Engineering", memberCount: 5 };
    const text = "@Engineering do the thing";

    await submitWithDelegate(w, text, target);

    const cancelBtn = bodyFind("[data-testid='delegate-cancel-btn']");
    expect(cancelBtn).not.toBeNull();
    cancelBtn.click();
    await flushPromises();

    expect(mockDelegate).not.toHaveBeenCalled();
    expect(mockRunAgui).not.toHaveBeenCalled();
    expect(w.find("textarea").element.value).toBe(text);
  });

  it("per-user delegate path still calls delegate({ to, intent, scopes:[], maxCost:50 })", async () => {
    const w = await mountChat();
    const target = { userId: "user:alice", displayName: "Alice" };
    const text = "@Alice do the thing";

    await submitWithDelegate(w, text, target);

    const confirmBtn = bodyFind("[data-testid='delegate-confirm-btn']");
    expect(confirmBtn).not.toBeNull();
    confirmBtn.click();
    await flushPromises();

    // intent has @Alice stripped.
    expect(mockDelegate).toHaveBeenCalledWith({
      to: "user:alice",
      intent: "do the thing",
      scopes: [],
      maxCost: 50,
    });
    expect(mockRunAgui).not.toHaveBeenCalled();
  });
});

describe("Chat.vue — Enter confirms delegate modal (CP2)", () => {
  let router;
  let wrapper;

  beforeEach(() => {
    localStorage.clear();
    setActivePinia(createPinia());
    router = makeRouter();
    mockRunAgui = vi.fn().mockImplementation((_opts, _handlers) => Promise.resolve("thread-abc"));
    mockDelegate = vi.fn().mockResolvedValue({ ok: true });
    clientMod.get.mockResolvedValue({ approvals: [] });
  });

  afterEach(() => {
    if (wrapper) { wrapper.unmount(); wrapper = null; }
    localStorage.clear();
    vi.restoreAllMocks();
  });

  async function mountChat() {
    await router.push("/chat");
    wrapper = mount(Chat, { global: { plugins: [router] }, attachTo: document.body });
    await flushPromises();
    return wrapper;
  }

  async function submitWithDelegate(w, text, target) {
    const textarea = w.find("textarea");
    await textarea.setValue(text);
    await w.findComponent({ name: "Composer" }).vm.$emit("submit", { delegateTo: target });
    await nextTick();
    await flushPromises();
  }

  it("Enter while delegate modal is open calls delegate() with stripped intent", async () => {
    const w = await mountChat();
    const target = { userId: "user:alice", displayName: "Alice" };
    const text = "@Alice do the thing";

    await submitWithDelegate(w, text, target);

    // Dispatch Enter keydown on document — modal is open.
    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    await flushPromises();
    await nextTick();

    expect(mockDelegate).toHaveBeenCalledOnce();
    expect(mockDelegate).toHaveBeenCalledWith({
      to: "user:alice",
      intent: "do the thing",
      scopes: [],
      maxCost: 50,
    });
  });

  it("Enter while delegate modal is closed does NOT call delegate()", async () => {
    await mountChat();

    // No delegation submitted — modal is closed.
    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    await flushPromises();

    expect(mockDelegate).not.toHaveBeenCalled();
  });
});
