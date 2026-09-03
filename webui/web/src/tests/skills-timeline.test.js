import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import Chat from "../views/Chat.vue";
import MessageList from "../components/MessageList.vue";
import SkillTimeline from "../components/SkillTimeline.vue";

// Stub vue-stream-markdown so shiki/WASM doesn't run under jsdom (matches chat.test.js).
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

// Fire a simulated AG-UI stream synchronously via mockRunAgui's handlers.
function simulateStream(handlers, events) {
  for (const [name, ...args] of events) {
    if (handlers[name]) handlers[name](...args);
  }
  return Promise.resolve("thread-abc");
}

const loadedSkill = { name: "billing", description: "Billing helper", status: "loaded" };
const suppressedSkill = { name: "hr-tools", description: "HR helper", status: "suppressed", reason: "cap-overflow" };

describe("Chat.vue — aikonos.skills.loaded timeline", () => {
  let router;

  beforeEach(() => {
    localStorage.clear();
    setActivePinia(createPinia());
    router = makeRouter();
    mockRunAgui = vi.fn().mockImplementation((_opts, _handlers) => Promise.resolve("thread-abc"));
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

  it("renders a skills timeline after a matching turn", async () => {
    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [
        ["onRunStarted"],
        ["onUser", { user: "alice" }],
        ["onSkillsLoaded", { skills: [loadedSkill] }],
        ["onTextStart"],
        ["onText", "hi"],
        ["onFinished"],
      ])
    );
    const w = await mountChat();
    const composer = w.find("textarea");
    await composer.setValue("bill me please");
    await w.find("form").trigger("submit");
    await flushPromises();

    expect(w.findComponent(SkillTimeline).exists()).toBe(true);
    expect(w.text()).toContain("billing");
    expect(w.text()).toContain("Billing helper");
  });

  it("positions the skills message after the user bubble and before the assistant text", async () => {
    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [
        ["onRunStarted"],
        ["onSkillsLoaded", { skills: [loadedSkill] }],
        ["onTextStart"],
        ["onText", "assistant reply text"],
        ["onFinished"],
      ])
    );
    const w = await mountChat();
    await w.find("textarea").setValue("bill me please");
    await w.find("form").trigger("submit");
    await flushPromises();

    const rows = w.findAll(".message-list-inner > *");
    const userIdx = rows.findIndex(r => r.classes().includes("msg-row--user"));
    const skillsIdx = rows.findIndex(r => r.findComponent(SkillTimeline).exists?.() || r.find(".skill-timeline").exists());
    const assistantIdx = rows.findIndex(r => r.classes().includes("msg-row--assistant"));

    expect(userIdx).toBeGreaterThanOrEqual(0);
    expect(skillsIdx).toBeGreaterThan(userIdx);
    expect(assistantIdx).toBeGreaterThan(skillsIdx);
  });

  it("does not re-announce the same (name, status) skill on a second matching turn", async () => {
    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [
        ["onRunStarted"],
        ["onSkillsLoaded", { skills: [loadedSkill] }],
        ["onFinished"],
      ])
    );
    const w = await mountChat();
    await w.find("textarea").setValue("bill me please");
    await w.find("form").trigger("submit");
    await flushPromises();
    expect(w.findAllComponents(SkillTimeline).length).toBe(1);

    // Second turn — same skill, same status.
    await w.find("textarea").setValue("bill me again");
    await w.find("form").trigger("submit");
    await flushPromises();

    expect(w.findAllComponents(SkillTimeline).length).toBe(1);
  });

  it("allows the same skill name to re-announce when its status changes", async () => {
    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [
        ["onRunStarted"],
        ["onSkillsLoaded", { skills: [loadedSkill] }],
        ["onFinished"],
      ])
    );
    const w = await mountChat();
    await w.find("textarea").setValue("bill me please");
    await w.find("form").trigger("submit");
    await flushPromises();

    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [
        ["onRunStarted"],
        ["onSkillsLoaded", { skills: [{ ...loadedSkill, status: "suppressed", reason: "flag-blocked" }] }],
        ["onFinished"],
      ])
    );
    await w.find("textarea").setValue("bill me please again");
    await w.find("form").trigger("submit");
    await flushPromises();

    expect(w.findAllComponents(SkillTimeline).length).toBe(2);
  });

  it("pushes nothing when all entries in the event have already been announced", async () => {
    mockRunAgui = vi.fn().mockImplementation((_opts, handlers) =>
      simulateStream(handlers, [
        ["onRunStarted"],
        ["onSkillsLoaded", { skills: [loadedSkill, suppressedSkill] }],
        ["onFinished"],
      ])
    );
    const w = await mountChat();
    await w.find("textarea").setValue("first turn");
    await w.find("form").trigger("submit");
    await flushPromises();
    expect(w.findAllComponents(SkillTimeline).length).toBe(1);

    await w.find("textarea").setValue("repeat turn");
    await w.find("form").trigger("submit");
    await flushPromises();

    // No new timeline message — dedup dropped both entries.
    expect(w.findAllComponents(SkillTimeline).length).toBe(1);
  });
});

describe("SkillTimeline.vue", () => {
  it("renders a knowledge icon + name + description for a loaded entry", () => {
    const w = mount(SkillTimeline, { props: { skills: [loadedSkill] } });
    expect(w.text()).toContain("billing");
    expect(w.text()).toContain("Billing helper");
    expect(w.find(".skill-timeline-item--suppressed").exists()).toBe(false);
  });

  it("renders an error icon + reason for a suppressed entry, not the description", () => {
    const w = mount(SkillTimeline, { props: { skills: [suppressedSkill] } });
    expect(w.text()).toContain("hr-tools");
    expect(w.text()).toContain("cap-overflow");
    expect(w.find(".skill-timeline-item--suppressed").exists()).toBe(true);
  });
});

describe("MessageList.vue — role:'skills'", () => {
  it("renders SkillTimeline for a role:'skills' message", () => {
    const w = mount(MessageList, {
      props: { messages: [{ role: "skills", skills: [loadedSkill] }] },
    });
    expect(w.findComponent(SkillTimeline).exists()).toBe(true);
    expect(w.text()).toContain("billing");
  });
});
