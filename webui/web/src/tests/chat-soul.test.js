// Tests for Chat.vue CP4 — owner personality editor for named agents.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import Chat from "../views/Chat.vue";

// Stub vue-stream-markdown (shiki/WASM not available under jsdom).
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

// Stub runAgui (no real SSE).
vi.mock("../api/agui.js", () => ({
  runAgui: vi.fn().mockResolvedValue("thread-x"),
}));

// Stub client (approval poll).
vi.mock("../api/client.js", () => ({
  get: vi.fn().mockResolvedValue({ approvals: [] }),
  post: vi.fn().mockResolvedValue({}),
  request: vi.fn(),
  del: vi.fn(),
  patch: vi.fn(),
}));

// Stub sessions API so writeSession doesn't blow up.
vi.mock("../api/sessions.js", () => ({
  listSessions: vi.fn().mockResolvedValue({ sessions: [] }),
  getSession:   vi.fn().mockResolvedValue(null),
  writeSession: vi.fn().mockResolvedValue({}),
}));

vi.mock("../api/agents.js", () => ({
  listMyAgents: vi.fn().mockResolvedValue({ agents: [] }),
  getAgentSoul: vi.fn(),
  setAgentSoul: vi.fn(),
}));

import * as agentsApi from "../api/agents.js";

function makeRouter(agentId = "") {
  const path = agentId ? `/chat?agent=${agentId}` : "/chat";
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [{ path: "/chat", component: Chat }, { path: "/", component: { template: "<div/>" } }],
  });
  router.push(path);
  return router;
}

describe("Chat.vue — soul/personality editor", () => {
  beforeEach(() => {
    localStorage.clear();
    setActivePinia(createPinia());
    vi.clearAllMocks();
  });

  afterEach(() => {
    localStorage.clear();
    vi.restoreAllMocks();
  });

  it("Personality button NOT rendered when no agentId in route", async () => {
    agentsApi.getAgentSoul.mockResolvedValue({ soul: "x" });
    const router = makeRouter("");
    await router.isReady();
    const w = mount(Chat, { global: { plugins: [router] } });
    await flushPromises();
    expect(w.find("[data-testid='personality-btn']").exists()).toBe(false);
    w.unmount();
  });

  it("Personality button NOT rendered when getAgentSoul returns forbidden", async () => {
    agentsApi.getAgentSoul.mockResolvedValue({ forbidden: true });
    const router = makeRouter("agent-99");
    await router.isReady();
    const w = mount(Chat, { global: { plugins: [router], attachTo: document.body } });
    await flushPromises();
    expect(w.find("[data-testid='personality-btn']").exists()).toBe(false);
    w.unmount();
  });

  it("Personality button rendered when getAgentSoul succeeds", async () => {
    agentsApi.getAgentSoul.mockResolvedValue({ soul: "Be helpful." });
    const router = makeRouter("agent-42");
    await router.isReady();
    const w = mount(Chat, { global: { plugins: [router], attachTo: document.body } });
    await flushPromises();
    expect(w.find("[data-testid='personality-btn']").exists()).toBe(true);
    w.unmount();
  });

  it("Personality button opens modal with existing soul pre-filled", async () => {
    agentsApi.getAgentSoul.mockResolvedValue({ soul: "Be helpful." });
    const router = makeRouter("agent-42");
    await router.isReady();
    const w = mount(Chat, { global: { plugins: [router], attachTo: document.body } });
    await flushPromises();

    await w.find("[data-testid='personality-btn']").trigger("click");
    await flushPromises();

    const ta = document.querySelector("[data-testid='soul-textarea']");
    expect(ta).not.toBeNull();
    expect(ta.value).toBe("Be helpful.");
    w.unmount();
  });

  it("Save calls setAgentSoul with the textarea value", async () => {
    agentsApi.getAgentSoul.mockResolvedValue({ soul: "" });
    agentsApi.setAgentSoul.mockResolvedValue({ soul: "Think step by step." });
    const router = makeRouter("agent-42");
    await router.isReady();
    const w = mount(Chat, { global: { plugins: [router], attachTo: document.body } });
    await flushPromises();

    await w.find("[data-testid='personality-btn']").trigger("click");
    await flushPromises();

    const ta = document.querySelector("[data-testid='soul-textarea']");
    ta.value = "Think step by step.";
    ta.dispatchEvent(new Event("input"));
    await flushPromises();

    document.querySelector("[data-testid='soul-save-btn']").click();
    await flushPromises();

    expect(agentsApi.setAgentSoul).toHaveBeenCalledWith("agent-42", "Think step by step.");
    w.unmount();
  });

  it("Personality button hidden when getAgentSoul throws", async () => {
    agentsApi.getAgentSoul.mockRejectedValue(new Error("network error"));
    const router = makeRouter("agent-42");
    await router.isReady();
    const w = mount(Chat, { global: { plugins: [router], attachTo: document.body } });
    await flushPromises();
    expect(w.find("[data-testid='personality-btn']").exists()).toBe(false);
    w.unmount();
  });
});
