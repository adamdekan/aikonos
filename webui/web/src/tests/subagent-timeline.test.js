// Subagent fan-out timeline:
// aikonos.subagent.spawned / aikonos.subagent.completed -> onSubagentSpawned /
// onSubagentCompleted -> {role:"subagents"} chat message -> SubagentTimeline.
// Mirrors memory-timeline.test.js's structure (Chat.vue integration +
// isolated component + MessageList branch), but this feature is two-phase
// (spawned creates a row, completed mutates that same row) rather than
// one-shot, so the correlation/scoping behavior gets its own tests below.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import Chat from "../views/Chat.vue";
import MessageList from "../components/MessageList.vue";
import SubagentTimeline from "../components/SubagentTimeline.vue";
import { useChatStore, CHAT_STORAGE_KEY } from "../store/chat.js";

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

function simulateStream(handlers, events) {
  for (const [name, ...args] of events) {
    if (handlers[name]) handlers[name](...args);
  }
  return Promise.resolve("thread-abc");
}

describe("Chat.vue — aikonos.subagent.* timeline", () => {
  let router;

  beforeEach(() => {
    localStorage.clear();
    setActivePinia(createPinia());
    router = makeRouter();
    mockRunAgui = vi.fn().mockImplementation(() => Promise.resolve("thread-abc"));
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

  async function send(w, text) {
    await w.find("textarea").setValue(text);
    await w.find("form").trigger("submit");
    await flushPromises();
  }

  it("renders a row naming the subtask on spawn", async () => {
    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [
        ["onRunStarted"],
        ["onSubagentSpawned", { index: 0, task: "summarize the doc", role: null }],
        ["onTextStart"],
        ["onText", "working"],
        ["onFinished"],
      ])
    );
    const w = await mountChat();
    await send(w, "fan out please");

    expect(w.findComponent(SubagentTimeline).exists()).toBe(true);
    expect(w.text()).toContain("summarize the doc");
  });

  it("updates the same row in place on completion — no duplicate row", async () => {
    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [
        ["onRunStarted"],
        ["onSubagentSpawned", { index: 0, task: "summarize the doc", role: null }],
        ["onSubagentCompleted", { index: 0, task: "summarize the doc", role: null, ok: true, cost: 0.0123 }],
        ["onFinished"],
      ])
    );
    const w = await mountChat();
    await send(w, "fan out please");

    const timeline = w.findComponent(SubagentTimeline);
    expect(timeline.props("branches").length).toBe(1);
    expect(timeline.props("branches")[0].status).toBe("ok");
  });

  it("renders a realistic small per-branch cost without collapsing to zero", async () => {
    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [
        ["onRunStarted"],
        ["onSubagentSpawned", { index: 0, task: "task a", role: null }],
        ["onSubagentCompleted", { index: 0, task: "task a", role: null, ok: true, cost: 0.0123 }],
        ["onFinished"],
      ])
    );
    const w = await mountChat();
    await send(w, "fan out please");

    const text = w.getComponent(SubagentTimeline).text();
    expect(text).not.toMatch(/\b0\.00\b/);
    expect(text).not.toMatch(/\$?0\b(?!\.\d*[1-9])/); // no bare "0"
    expect(text).toMatch(/0\.012/);
  });

  it("renders all four failure kinds distinguishably", async () => {
    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [
        ["onRunStarted"],
        ["onSubagentSpawned", { index: 0, task: "a", role: null }],
        ["onSubagentSpawned", { index: 1, task: "b", role: null }],
        ["onSubagentSpawned", { index: 2, task: "c", role: null }],
        ["onSubagentSpawned", { index: 3, task: "d", role: null }],
        ["onSubagentCompleted", { index: 0, task: "a", role: null, ok: false, failure: "error", cost: 0.001 }],
        ["onSubagentCompleted", { index: 1, task: "b", role: null, ok: false, failure: "timeout", cost: 0.001 }],
        ["onSubagentCompleted", { index: 2, task: "c", role: null, ok: false, failure: "denied", cost: 0 }],
        ["onSubagentCompleted", { index: 3, task: "d", role: null, ok: false, failure: "systemic", cost: 0.001 }],
        ["onFinished"],
      ])
    );
    const w = await mountChat();
    await send(w, "fan out please");

    const timeline = w.getComponent(SubagentTimeline);
    const branches = timeline.props("branches");
    const labels = branches.map((b) => b.failure);
    expect(new Set(labels).size).toBe(4);

    // A user must be able to tell "denied" (run it directly) apart from "error" (broke).
    const text = timeline.text();
    expect(text).toContain("needs to run directly in chat");
    expect(text).toContain("failed");
    expect(text).toContain("timed out");
    expect(text).toContain("system error");
  });

  it("shows the aggregate total only once all known branches resolve, summing branch costs", async () => {
    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [
        ["onRunStarted"],
        ["onSubagentSpawned", { index: 0, task: "a", role: null }],
        ["onSubagentSpawned", { index: 1, task: "b", role: null }],
        ["onSubagentCompleted", { index: 0, task: "a", role: null, ok: true, cost: 0.0123 }],
        // Not finished yet — branch 1 still running. No total should show.
      ])
    );
    const w = await mountChat();
    await send(w, "fan out please");

    let timeline = w.getComponent(SubagentTimeline);
    expect(timeline.find('[data-testid="subagent-timeline-total"]').exists()).toBe(false);

    // Now complete branch 1 via a fresh stream on the same buffer.
    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [
        ["onRunStarted"],
        ["onSubagentCompleted", { index: 1, task: "b", role: null, ok: true, cost: 0.005 }],
        ["onFinished"],
      ])
    );
    await send(w, "continue");

    timeline = w.getComponent(SubagentTimeline);
    const total = timeline.find('[data-testid="subagent-timeline-total"]');
    expect(total.exists()).toBe(true);
    expect(total.text()).toMatch(/0\.017/); // 0.0123 + 0.005 = 0.0173
  });

  it("renders a completed event with no prior spawned as a standalone row", async () => {
    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [
        ["onRunStarted"],
        ["onSubagentCompleted", { index: 0, task: "reconnect-missed task", role: null, ok: true, cost: 0.002 }],
        ["onFinished"],
      ])
    );
    const w = await mountChat();
    await send(w, "fan out please");

    const timeline = w.getComponent(SubagentTimeline);
    expect(timeline.props("branches").length).toBe(1);
    expect(timeline.text()).toContain("reconnect-missed task");
  });

  it("keeps two fan-outs in one session from overwriting each other's rows", async () => {
    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [
        ["onRunStarted"],
        ["onSubagentSpawned", { index: 0, task: "first fan-out task", role: null }],
        ["onSubagentCompleted", { index: 0, task: "first fan-out task", role: null, ok: true, cost: 0.01 }],
        ["onFinished"],
      ])
    );
    const w = await mountChat();
    await send(w, "first fan out");

    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [
        ["onRunStarted"],
        // Index restarts at 0 for the second fan-out.
        ["onSubagentSpawned", { index: 0, task: "second fan-out task", role: null }],
        ["onFinished"],
      ])
    );
    await send(w, "second fan out");

    const timelines = w.findAllComponents(SubagentTimeline);
    expect(timelines.length).toBe(2);
    expect(timelines[0].props("branches")[0].task).toBe("first fan-out task");
    expect(timelines[0].props("branches")[0].status).toBe("ok");
    expect(timelines[1].props("branches")[0].task).toBe("second fan-out task");
    expect(timelines[1].props("branches")[0].status).toBe("running");
  });

  it("persists and reloads the subagent message via the chat store", async () => {
    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [
        ["onRunStarted"],
        ["onSubagentSpawned", { index: 0, task: "persisted task", role: null }],
        ["onSubagentCompleted", { index: 0, task: "persisted task", role: null, ok: true, cost: 0.0123 }],
        ["onFinished"],
      ])
    );
    const w = await mountChat();
    await send(w, "fan out please");

    const raw = localStorage.getItem(CHAT_STORAGE_KEY);
    expect(raw).toBeTruthy();
    const parsed = JSON.parse(raw);
    const key = Object.keys(parsed)[0];
    const buf = parsed[key];
    const subagentMsg = buf.messages.find((m) => m.role === "subagents");
    expect(subagentMsg).toBeTruthy();
    expect(subagentMsg.branches[0].task).toBe("persisted task");
    expect(subagentMsg.branches[0].cost).toBeCloseTo(0.0123);

    // Round-trip through the store's own load-from-storage path (a fresh Pinia
    // instance simulates a page reload), not just re-parsing the JSON we wrote.
    w.unmount();
    setActivePinia(createPinia());
    const freshStore = useChatStore();
    freshStore.setActiveSession(key);
    const reloaded = freshStore.currentBuffer();
    const reloadedMsg = reloaded.messages.find((m) => m.role === "subagents");
    expect(reloadedMsg).toBeTruthy();
    expect(reloadedMsg.branches[0].task).toBe("persisted task");
    expect(reloadedMsg.branches[0].status).toBe("ok");
  });
});

describe("SubagentTimeline.vue", () => {
  it("renders task text and running status for an unresolved branch", () => {
    const w = mount(SubagentTimeline, {
      props: { branches: [{ index: 0, task: "do the thing", role: null, status: "running", cost: 0 }] },
    });
    expect(w.text()).toContain("do the thing");
    expect(w.find('[data-testid="subagent-timeline-total"]').exists()).toBe(false);
  });

  it("renders per-branch cost for a resolved branch without collapsing to zero", () => {
    const w = mount(SubagentTimeline, {
      props: {
        branches: [{ index: 0, task: "do the thing", role: null, status: "ok", cost: 0.0123 }],
      },
    });
    expect(w.text()).toMatch(/0\.012/);
  });

  it("renders the aggregate total as the sum of branch costs once all resolve", () => {
    const w = mount(SubagentTimeline, {
      props: {
        branches: [
          { index: 0, task: "a", role: null, status: "ok", cost: 0.0123 },
          { index: 1, task: "b", role: null, status: "ok", cost: 0.005 },
        ],
      },
    });
    const total = w.find('[data-testid="subagent-timeline-total"]');
    expect(total.exists()).toBe(true);
    expect(total.text()).toMatch(/0\.017/);
  });
});

describe("MessageList.vue — role:'subagents'", () => {
  it("renders SubagentTimeline for a role:'subagents' message", () => {
    const w = mount(MessageList, {
      props: {
        messages: [
          { role: "subagents", branches: [{ index: 0, task: "do the thing", role: null, status: "running", cost: 0 }] },
        ],
      },
    });
    expect(w.findComponent(SubagentTimeline).exists()).toBe(true);
    expect(w.text()).toContain("do the thing");
  });
});
