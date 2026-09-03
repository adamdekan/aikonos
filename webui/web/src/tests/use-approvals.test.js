import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { nextTick } from "vue";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { usePromptStore } from "../store/prompt.js";
import Chat from "../views/Chat.vue";
import * as clientMod from "../api/client.js";
import { useToast } from "../components/ui/useToast.js";

// F37 — conditional, queue-aware approval polling. These tests drive the real
// Chat.vue wiring (useApprovals + useAguiRun) the same way chat.test.js's HITL
// dedup suite does, rather than instantiating the composables in isolation,
// since useApprovals' onUnmounted hook needs a live component instance.

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

const info1 = {
  toolCallId: "tc-q1",
  toolId: "web.fetch",
  toolName: "web.fetch",
  reason: "requires approval",
  args: { url: "https://example.com" },
  stepUp: false,
};

const info2 = {
  toolCallId: "tc-q2",
  toolId: "doc.write",
  toolName: "doc.write",
  reason: "requires approval",
  args: { path: "notes.md" },
  stepUp: false,
};

describe("F37 — conditional, queue-aware approval polling", () => {
  let router;

  beforeEach(() => {
    localStorage.clear();
    setActivePinia(createPinia());
    router = makeRouter();
    clientMod.get.mockReset();
    clientMod.get.mockResolvedValue({ approvals: [] });
    // Drain any stray toasts left by a previous test (shared module singleton).
    const { toasts } = useToast();
    toasts.splice(0, toasts.length);
  });

  afterEach(() => {
    localStorage.clear();
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  async function mountChat(prompt = "need approval") {
    const ps = usePromptStore();
    ps.set(prompt);
    await router.push("/chat");
    const w = mount(Chat, { global: { plugins: [router] } });
    await flushPromises();
    return w;
  }

  it("does not start continuous polling when a run produces no approval event and none is pending server-side (unconditional-start flipped)", async () => {
    let resolveStream;
    mockRunAgui = vi.fn().mockImplementation(async (_opts, handlers) => {
      handlers.onRunStarted();
      await new Promise((res) => { resolveStream = res; });
      handlers.onFinished();
      return "thread-abc";
    });
    // /approvals stays empty for the whole run — the one-shot safety net (F37
    // follow-up) fires exactly once at RUN_STARTED+2s but finds nothing, so it
    // must not escalate into continuous polling.

    vi.useFakeTimers();
    const w = await mountChat();

    vi.advanceTimersByTime(6000);
    await flushPromises();

    // Exactly one /approvals GET (the one-shot safety check) — no recurring poll.
    const approvalsCalls = clientMod.get.mock.calls.filter(([url]) => url === "/approvals");
    expect(approvalsCalls.length).toBe(1);
    expect(w.findAll(".approval-modal").length).toBe(0);

    vi.useRealTimers();
    resolveStream();
    await flushPromises();
  });

  it("starts polling once a aikonos.approval.request SSE event arrives", async () => {
    let resolveStream;
    mockRunAgui = vi.fn().mockImplementation(async (_opts, handlers) => {
      handlers.onRunStarted();
      handlers.onApprovalRequest(info1);
      await new Promise((res) => { resolveStream = res; });
      handlers.onFinished();
      return "thread-abc";
    });

    vi.useFakeTimers();
    const w = await mountChat();
    expect(w.findAll(".approval-modal").length).toBe(1);

    vi.advanceTimersByTime(2000);
    await flushPromises();

    expect(clientMod.get).toHaveBeenCalledWith("/approvals");

    vi.useRealTimers();
    resolveStream();
    await flushPromises();
  });

  it("two distinct approvals enqueue FIFO — one modal at a time, no thrash", async () => {
    let resolveStream;
    mockRunAgui = vi.fn().mockImplementation(async (_opts, handlers) => {
      handlers.onRunStarted();
      handlers.onApprovalRequest(info1);
      await new Promise((res) => { resolveStream = res; });
      handlers.onFinished();
      return "thread-abc";
    });
    // The poll fallback discovers the second, distinct approval.
    clientMod.get.mockResolvedValue({ approvals: [info1, info2] });

    vi.useFakeTimers();
    const w = await mountChat();
    expect(w.findAll(".approval-modal").length).toBe(1);
    expect(w.text()).toContain(info1.toolId);

    vi.advanceTimersByTime(2000);
    await flushPromises();
    await nextTick();

    // Still exactly one modal visible — info1 remains current; info2 queued behind it.
    expect(w.findAll(".approval-modal").length).toBe(1);
    expect(w.text()).toContain(info1.toolId);
    expect(w.text()).toContain("1 of 2");

    // Resolve the current approval — the queue advances to info2.
    await w.find("[data-action='deny']").trigger("click");
    await flushPromises();
    await nextTick();

    expect(w.findAll(".approval-modal").length).toBe(1);
    expect(w.text()).toContain(info2.toolId);

    vi.useRealTimers();
    resolveStream();
    await flushPromises();
  });

  it("handled-set dedup: same toolCallId from CUSTOM event and poll enqueues once", async () => {
    let resolveStream;
    mockRunAgui = vi.fn().mockImplementation(async (_opts, handlers) => {
      handlers.onRunStarted();
      handlers.onApprovalRequest(info1);
      await new Promise((res) => { resolveStream = res; });
      handlers.onFinished();
      return "thread-abc";
    });
    clientMod.get.mockResolvedValue({ approvals: [info1] });

    vi.useFakeTimers();
    const w = await mountChat();

    vi.advanceTimersByTime(4000);
    await flushPromises();
    await nextTick();

    expect(w.findAll(".approval-modal").length).toBe(1);
    // No queue badge — only one distinct entry was ever enqueued.
    expect(w.text()).not.toContain("1 of");

    vi.useRealTimers();
    resolveStream();
    await flushPromises();
  });

  it("toasts once per polling session on poll failure, then stays quiet", async () => {
    let resolveStream;
    mockRunAgui = vi.fn().mockImplementation(async (_opts, handlers) => {
      handlers.onRunStarted();
      handlers.onApprovalRequest(info1);
      await new Promise((res) => { resolveStream = res; });
      handlers.onFinished();
      return "thread-abc";
    });
    clientMod.get.mockRejectedValue(new Error("network down"));

    vi.useFakeTimers();
    const w = await mountChat();
    const { toasts } = useToast();

    vi.advanceTimersByTime(2000);
    await flushPromises();
    expect(toasts.length).toBe(1);

    vi.advanceTimersByTime(2000);
    await flushPromises();
    // Second failure stays quiet — no additional toast.
    expect(toasts.length).toBe(1);

    vi.useRealTimers();
    resolveStream();
    await flushPromises();
  });

  it("one-shot safety net surfaces a pending approval when the SSE frame is dropped", async () => {
    // RUN_STARTED fires, but no aikonos.approval.request CUSTOM event ever
    // arrives — simulates a dropped SSE frame while the connection stays live.
    let resolveStream;
    mockRunAgui = vi.fn().mockImplementation(async (_opts, handlers) => {
      handlers.onRunStarted();
      await new Promise((res) => { resolveStream = res; });
      handlers.onFinished();
      return "thread-abc";
    });
    // The approval exists server-side — only the SSE notification was lost.
    clientMod.get.mockResolvedValue({ approvals: [info1] });

    vi.useFakeTimers();
    const w = await mountChat();

    // Before the one-shot fires, no approval is visible yet.
    expect(w.findAll(".approval-modal").length).toBe(0);

    // One poll interval later, the safety-net GET fires and surfaces it.
    vi.advanceTimersByTime(2000);
    await flushPromises();
    await nextTick();

    expect(w.findAll(".approval-modal").length).toBe(1);
    expect(w.text()).toContain(info1.toolId);

    // Continuous polling should now be running too (escalated by the one-shot).
    vi.advanceTimersByTime(2000);
    await flushPromises();
    expect(clientMod.get).toHaveBeenCalledWith("/approvals");

    vi.useRealTimers();
    resolveStream();
    await flushPromises();
  });

  it("one-shot timer is cleared when the run finishes before the interval elapses", async () => {
    // Run starts and finishes synchronously (fast run) — well within 2s.
    mockRunAgui = vi.fn().mockImplementation(async (_opts, handlers) => {
      handlers.onRunStarted();
      handlers.onFinished();
      return "thread-abc";
    });
    // If the one-shot were not cleared, this would surface a phantom approval.
    clientMod.get.mockResolvedValue({ approvals: [info1] });

    vi.useFakeTimers();
    const w = await mountChat();

    vi.advanceTimersByTime(6000);
    await flushPromises();
    await nextTick();

    // The one-shot must have been cancelled on run end — no /approvals fetch,
    // no modal ever appears.
    expect(clientMod.get).not.toHaveBeenCalledWith("/approvals");
    expect(w.findAll(".approval-modal").length).toBe(0);

    vi.useRealTimers();
  });
});
