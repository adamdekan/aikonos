// CP2 (F34): Chat-mount discovery banner — visible on store error, Retry
// calls discoveryStore.refresh(), clears once refresh succeeds.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { nextTick } from "vue";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import Chat from "../views/Chat.vue";
import * as clientMod from "../api/client.js";

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

vi.mock("../api/agui.js", () => ({
  runAgui: vi.fn().mockImplementation((_opts, _handlers) => Promise.resolve("thread-abc")),
}));

vi.mock("../api/inbox.js", () => ({
  listInbox: vi.fn().mockResolvedValue({ items: [] }),
  delegate: vi.fn().mockResolvedValue({ ok: true }),
}));

vi.mock("../api/client.js", () => ({
  get: vi.fn(),
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

describe("Chat.vue — discovery failure banner (CP2/F34)", () => {
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

  async function mountChat() {
    await router.push("/chat");
    const w = mount(Chat, { global: { plugins: [router] } });
    await flushPromises();
    return w;
  }

  it("shows no banner when discovery datasets load healthily", async () => {
    clientMod.get.mockResolvedValue({ approvals: [] });
    const w = await mountChat();
    expect(w.find("[data-testid='error-banner']").exists()).toBe(false);
  });

  it("shows the banner when a discovery dataset fails, with a Retry action", async () => {
    clientMod.get.mockImplementation((path) => {
      if (path === "/user/skill-bundles") return Promise.reject(new Error("boom"));
      return Promise.resolve({ approvals: [] });
    });
    const w = await mountChat();

    const banner = w.find("[data-testid='error-banner']");
    expect(banner.exists()).toBe(true);
    expect(banner.text()).toContain("Mention and tool palettes unavailable");
    expect(w.find("[data-testid='discovery-retry-btn']").exists()).toBe(true);
  });

  it("Retry calls discoveryStore.refresh() and the banner clears once healthy", async () => {
    clientMod.get.mockImplementation((path) => {
      if (path === "/user/skill-bundles") return Promise.reject(new Error("boom"));
      return Promise.resolve({ approvals: [] });
    });
    const w = await mountChat();
    expect(w.find("[data-testid='error-banner']").exists()).toBe(true);

    // Recover the failing endpoint before retrying.
    clientMod.get.mockResolvedValue({ approvals: [] });
    await w.find("[data-testid='discovery-retry-btn']").trigger("click");
    await flushPromises();
    await nextTick();

    expect(w.find("[data-testid='error-banner']").exists()).toBe(false);
  });
});
