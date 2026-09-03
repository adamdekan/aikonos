// Auto-recall chip:
// aikonos.memory.recalled → onMemoryRecalled → {role:"memory"} chat message →
// MemoryTimeline. Mirrors skills-timeline.test.js, whose dedup+splice contract
// this reproduces for memory entries.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import Chat from "../views/Chat.vue";
import MessageList from "../components/MessageList.vue";
import MemoryTimeline from "../components/MemoryTimeline.vue";

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

const groupConcept = {
  id: "deploy-runbook",
  scope: "group",
  groupId: "security-team",
  title: "Deploy runbook",
  status: "stable",
  trustTier: "human-reviewed",
  stale: false,
};
const staleUserConcept = {
  id: "old-note",
  scope: "user",
  title: "Old note",
  status: "draft",
  trustTier: "unverified",
  stale: true,
};

describe("Chat.vue — aikonos.memory.recalled timeline", () => {
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

  it("renders a memory timeline after a recalling turn", async () => {
    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [
        ["onRunStarted"],
        ["onMemoryRecalled", { concepts: [groupConcept] }],
        ["onTextStart"],
        ["onText", "hi"],
        ["onFinished"],
      ])
    );
    const w = await mountChat();
    await send(w, "how do we deploy");

    expect(w.findComponent(MemoryTimeline).exists()).toBe(true);
    expect(w.text()).toContain("Deploy runbook");
    expect(w.text()).toContain("security-team");
  });

  it("positions the memory message after the user bubble and before the assistant text", async () => {
    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [
        ["onRunStarted"],
        ["onMemoryRecalled", { concepts: [groupConcept] }],
        ["onTextStart"],
        ["onText", "assistant reply text"],
        ["onFinished"],
      ])
    );
    const w = await mountChat();
    await send(w, "how do we deploy");

    const rows = w.findAll(".message-list-inner > *");
    const userIdx = rows.findIndex(r => r.classes().includes("msg-row--user"));
    const memoryIdx = rows.findIndex(r => r.find(".memory-timeline").exists());
    const assistantIdx = rows.findIndex(r => r.classes().includes("msg-row--assistant"));

    expect(userIdx).toBeGreaterThanOrEqual(0);
    expect(memoryIdx).toBeGreaterThan(userIdx);
    expect(assistantIdx).toBeGreaterThan(memoryIdx);
  });

  it("does not re-announce the same (id, scope) concept on a second recalling turn", async () => {
    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [
        ["onRunStarted"],
        ["onMemoryRecalled", { concepts: [groupConcept] }],
        ["onFinished"],
      ])
    );
    const w = await mountChat();
    await send(w, "how do we deploy");
    expect(w.findAllComponents(MemoryTimeline).length).toBe(1);

    await send(w, "deploy again");
    expect(w.findAllComponents(MemoryTimeline).length).toBe(1);
  });

  it("splices only the not-yet-announced concepts", async () => {
    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [
        ["onRunStarted"],
        ["onMemoryRecalled", { concepts: [groupConcept] }],
        ["onFinished"],
      ])
    );
    const w = await mountChat();
    await send(w, "how do we deploy");

    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [
        ["onRunStarted"],
        ["onMemoryRecalled", { concepts: [groupConcept, staleUserConcept] }],
        ["onFinished"],
      ])
    );
    await send(w, "deploy and the old note");

    const timelines = w.findAllComponents(MemoryTimeline);
    expect(timelines.length).toBe(2);
    // The second timeline carries only the new concept — the group one was deduped.
    expect(timelines[1].props("concepts").map(c => c.id)).toEqual(["old-note"]);
  });

  it("pushes nothing for an empty concepts array", async () => {
    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [
        ["onRunStarted"],
        ["onMemoryRecalled", { concepts: [] }],
        ["onFinished"],
      ])
    );
    const w = await mountChat();
    await send(w, "nothing matches");

    expect(w.findComponent(MemoryTimeline).exists()).toBe(false);
  });
});

describe("MemoryTimeline.vue", () => {
  it("renders scope, title and id for a group concept", () => {
    const w = mount(MemoryTimeline, { props: { concepts: [groupConcept] } });
    expect(w.text()).toContain("Deploy runbook");
    expect(w.text()).toContain("deploy-runbook");
    expect(w.text()).toContain("security-team");
    expect(w.find(".memory-timeline-item--stale").exists()).toBe(false);
  });

  it("marks a stale concept and shows its status", () => {
    const w = mount(MemoryTimeline, { props: { concepts: [staleUserConcept] } });
    expect(w.text()).toContain("Old note");
    expect(w.text()).toContain("stale");
    expect(w.text()).toContain("draft");
    expect(w.find(".memory-timeline-item--stale").exists()).toBe(true);
  });

  it("falls back to the id when the concept has no title", () => {
    const w = mount(MemoryTimeline, { props: { concepts: [{ ...staleUserConcept, title: "" }] } });
    expect(w.text()).toContain("old-note");
  });
});

describe("MessageList.vue — role:'memory'", () => {
  it("renders MemoryTimeline for a role:'memory' message", () => {
    const w = mount(MessageList, {
      props: { messages: [{ role: "memory", concepts: [groupConcept] }] },
    });
    expect(w.findComponent(MemoryTimeline).exists()).toBe(true);
    expect(w.text()).toContain("Deploy runbook");
  });
});
