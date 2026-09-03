import { mount } from "@vue/test-utils";
import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { flushPromises } from "@vue/test-utils";
import { createRouter, createWebHashHistory } from "vue-router";
import { nextTick } from "vue";
import Sidebar from "../components/Sidebar.vue";
import { autoScrollbar } from "../lib/autoScrollbar.js";
import { useUserStore } from "../store/user.js";
import { useInboxStore } from "../store/inbox.js";
import { useAgentsStore } from "../store/agents.js";
import { createPinia, setActivePinia } from "pinia";

// Prevent real API calls from refreshAdminStatus during mount.
vi.mock("../api/adminStatus.js", () => ({
  refreshAdminStatus: vi.fn().mockResolvedValue(undefined),
}));

// Prevent real listInbox calls from Sidebar's refresh.
vi.mock("../api/inbox.js", () => ({
  listInbox: vi.fn().mockResolvedValue({ envelopes: [] }),
}));

// Prevent real listMyAgents calls from Sidebar's refresh.
vi.mock("../api/agents.js", () => ({
  listMyAgents: vi.fn().mockResolvedValue({ agents: [] }),
}));

// Sidebar fetches configured connector providers to decide whether the
// Connections nav item shows. Default: one provider configured (item visible).
vi.mock("../api/connectors.js", () => ({
  listConnectorProviders: vi.fn().mockResolvedValue({
    providers: [{ key: "onedrive", displayName: "OneDrive" }],
  }),
}));

// Sidebar fetches the user's granted skills to gate skill-gated feature nav
// (Schedules/Workflows). Default: both capability skills granted (items visible).
// Spread the real module so other api/admin.js consumers keep working.
vi.mock("../api/admin.js", async (importOriginal) => ({
  ...(await importOriginal()),
  listUserSkills: vi.fn().mockResolvedValue({ skills: ["scheduler", "workflows"] }),
}));

import * as inboxApiMod from "../api/inbox.js";
import * as adminApiMod from "../api/admin.js";
import * as agentsApiMod from "../api/agents.js";
import * as connApiMod from "../api/connectors.js";

// minimal routes for router
const routes = [
  { path: "/", component: { template: "<div/>" } },
  { path: "/chat", component: { template: "<div/>" } },
  { path: "/files", component: { template: "<div/>" } },
  { path: "/connections", component: { template: "<div/>" } },
  { path: "/schedules", component: { template: "<div/>" } },
  { path: "/workflows", component: { template: "<div/>" } },
  { path: "/skills", component: { template: "<div/>" } },
  { path: "/inbox", component: { template: "<div/>" } },
  { path: "/admin/roles", component: { template: "<div/>" } },
  { path: "/admin/roles/:sub", component: { template: "<div/>" } },
  { path: "/admin/mcp", component: { template: "<div/>" } },
  { path: "/admin/runs", component: { template: "<div/>" } },
  { path: "/admin/audit", component: { template: "<div/>" } },
  { path: "/admin/audit/history", component: { template: "<div/>" } },
  { path: "/admin/decisions", component: { template: "<div/>" } },
  { path: "/admin/policy/simulate", component: { template: "<div/>" } },
  { path: "/admin/agents", component: { template: "<div/>" } },
  { path: "/admin/policy", component: { template: "<div/>" } },
  { path: "/admin/settings", component: { template: "<div/>" } },
  { path: "/admin/skill-bundles", component: { template: "<div/>" } },
];

function makeRouter(initialPath = "/") {
  const router = createRouter({ history: createWebHashHistory(), routes });
  router.push(initialPath);
  return router;
}

describe("Sidebar.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
  });

  it("renders all feature nav items when isAdmin=true", async () => {
    const router = makeRouter();
    await router.isReady();
    const store = useUserStore();
    store.isAdmin = true;
    const wrapper = mount(Sidebar, { global: { plugins: [router], directives: { autoscroll: autoScrollbar } } });
    await flushPromises(); // Connections item appears after the async provider fetch resolves
    const text = wrapper.text();
    // workspace items
    expect(text).toContain("Chat");
    expect(text).toContain("Files");
    expect(text).toContain("Connections");
    expect(text).toContain("Schedules");
    expect(text).toContain("Workflows");
    expect(text).toContain("Skills");
    expect(text).toContain("Inbox");
    // admin items
    expect(text).toContain("Access Control");
    expect(text).toContain("Tools");
    expect(text).toContain("Skills");
    expect(text).toContain("MCP");
    expect(text).toContain("Agents");
    expect(text).toContain("Runs");
    expect(text).toContain("Policy");
    expect(text).toContain("Settings");
  });

  it("shows the Connections nav item when a connector provider is configured", async () => {
    connApiMod.listConnectorProviders.mockResolvedValueOnce({
      providers: [{ key: "onedrive", displayName: "OneDrive" }],
    });
    const router = makeRouter();
    await router.isReady();
    const wrapper = mount(Sidebar, { global: { plugins: [router], directives: { autoscroll: autoScrollbar } } });
    await flushPromises();
    expect(wrapper.text()).toContain("Connections");
  });

  it("hides the Connections nav item when no connector provider is configured", async () => {
    connApiMod.listConnectorProviders.mockResolvedValueOnce({ providers: [] });
    const router = makeRouter();
    await router.isReady();
    const wrapper = mount(Sidebar, { global: { plugins: [router], directives: { autoscroll: autoScrollbar } } });
    await flushPromises();
    expect(wrapper.text()).not.toContain("Connections");
  });

  // ── Skill-gated feature nav (deny-by-default, mirrors broker FGA gating) ─────

  it("hides Schedules and Workflows when the user lacks both capability skills", async () => {
    adminApiMod.listUserSkills.mockResolvedValueOnce({ skills: ["web.fetch"] });
    const router = makeRouter();
    await router.isReady();
    const wrapper = mount(Sidebar, { global: { plugins: [router], directives: { autoscroll: autoScrollbar } } });
    await flushPromises();
    expect(wrapper.text()).not.toContain("Schedules");
    expect(wrapper.text()).not.toContain("Workflows");
    // ungated items stay visible
    expect(wrapper.text()).toContain("Chat");
    expect(wrapper.text()).toContain("Inbox");
  });

  it("shows only the skill-gated item whose grant is present", async () => {
    adminApiMod.listUserSkills.mockResolvedValueOnce({ skills: ["scheduler"] });
    const router = makeRouter();
    await router.isReady();
    const wrapper = mount(Sidebar, { global: { plugins: [router], directives: { autoscroll: autoScrollbar } } });
    await flushPromises();
    expect(wrapper.text()).toContain("Schedules");
    expect(wrapper.text()).not.toContain("Workflows");
  });

  it("hides skill-gated items when the skills fetch fails (fail-closed)", async () => {
    adminApiMod.listUserSkills.mockRejectedValueOnce(new Error("broker down"));
    const router = makeRouter();
    await router.isReady();
    const wrapper = mount(Sidebar, { global: { plugins: [router], directives: { autoscroll: autoScrollbar } } });
    await flushPromises();
    expect(wrapper.text()).not.toContain("Schedules");
    expect(wrapper.text()).not.toContain("Workflows");
  });

  // Skills is deliberately NOT skill-gated in the
  // nav — the view itself renders a no-access panel on a 403, so the item must
  // stay visible even when the caller's granted-skills fetch omits it entirely.
  it("keeps the Skills nav item visible regardless of granted skills", async () => {
    adminApiMod.listUserSkills.mockResolvedValueOnce({ skills: [] });
    const router = makeRouter();
    await router.isReady();
    const wrapper = mount(Sidebar, { global: { plugins: [router], directives: { autoscroll: autoScrollbar } } });
    await flushPromises();
    expect(wrapper.text()).toContain("My Skills");
  });

  it("renders both section labels when isAdmin=true", async () => {
    const router = makeRouter();
    await router.isReady();
    const store = useUserStore();
    store.isAdmin = true;
    const wrapper = mount(Sidebar, { global: { plugins: [router], directives: { autoscroll: autoScrollbar } } });
    const text = wrapper.text();
    expect(text).toContain("Workspace");
    expect(text).toContain("Admin");
  });

  it("marks the active route item", async () => {
    const router = makeRouter("/files");
    await router.isReady();
    const wrapper = mount(Sidebar, { global: { plugins: [router], directives: { autoscroll: autoScrollbar } } });
    const activeLinks = wrapper.findAll(".nav-item.active");
    expect(activeLinks.length).toBe(1);
    expect(activeLinks[0].text()).toContain("Files");
  });

  it("marks parent nav item active for nested sub-routes", async () => {
    const router = makeRouter("/admin/roles/anything");
    await router.isReady();
    const store = useUserStore();
    store.isAdmin = true;
    const wrapper = mount(Sidebar, { global: { plugins: [router], directives: { autoscroll: autoScrollbar } } });
    const activeLinks = wrapper.findAll(".nav-item.active");
    expect(activeLinks.length).toBe(1);
    expect(activeLinks[0].text()).toContain("Access Control");
  });

  it("collapse toggle flips rail class on the sidebar element", async () => {
    const router = makeRouter();
    await router.isReady();
    const wrapper = mount(Sidebar, { global: { plugins: [router], directives: { autoscroll: autoScrollbar } } });
    expect(wrapper.find(".sidebar").classes()).not.toContain("rail");
    const toggleBtn = wrapper.find("[data-testid='sidebar-toggle']");
    await toggleBtn.trigger("click");
    expect(wrapper.find(".sidebar").classes()).toContain("rail");
    await toggleBtn.trigger("click");
    expect(wrapper.find(".sidebar").classes()).not.toContain("rail");
  });

  // ── Admin section visibility gating ──────────────────────────────────────────

  it("hides Admin section label and all admin items when isAdmin=false", async () => {
    const router = makeRouter();
    await router.isReady();
    // isAdmin defaults to false — no store mutation needed
    const wrapper = mount(Sidebar, { global: { plugins: [router], directives: { autoscroll: autoScrollbar } } });
    const text = wrapper.text();
    expect(text).not.toContain("Access Control");
    expect(text).not.toContain("Network");
    expect(text).not.toContain("MCP");
    expect(text).not.toContain("Runs");
    expect(text).not.toContain("Audit");
    expect(text).not.toContain("Audit History");
    expect(text).not.toContain("Decisions");
    expect(text).not.toContain("Simulator");
    expect(text).not.toContain("Policy");
    expect(text).not.toContain("Settings");
    // The "Admin" section label must also be absent
    // (collapse is off so section labels are rendered when present)
    expect(wrapper.find(".nav-section .section-label") .exists() &&
           wrapper.findAll(".nav-section .section-label").map(el => el.text()).includes("Admin")).toBe(false);
  });

  it("shows Admin section label and all admin items when isAdmin=true", async () => {
    const router = makeRouter();
    await router.isReady();
    const store = useUserStore();
    store.isAdmin = true;
    const wrapper = mount(Sidebar, { global: { plugins: [router], directives: { autoscroll: autoScrollbar } } });
    const text = wrapper.text();
    expect(text).toContain("Admin");
    expect(text).toContain("Access Control");
    expect(text).toContain("MCP");
    expect(text).toContain("Agents");
    expect(text).toContain("Runs");
    expect(text).toContain("Policy");
    expect(text).toContain("Settings");
    // Network Access / Rate Limits / Spend Caps are Settings categories now —
    // they must not reappear as top-level Admin nav items.
    expect(text).not.toContain("Network");
    expect(text).not.toContain("Rate Limits");
    expect(text).not.toContain("Spend Caps");
  });

  it("re-renders admin section when isAdmin flips from false to true", async () => {
    const router = makeRouter();
    await router.isReady();
    const store = useUserStore();
    const wrapper = mount(Sidebar, { global: { plugins: [router], directives: { autoscroll: autoScrollbar } } });
    expect(wrapper.text()).not.toContain("Access Control");
    store.isAdmin = true;
    await nextTick();
    expect(wrapper.text()).toContain("Access Control");
  });

  it(".sidebar-scroll wrapper exists and contains both nav sections", async () => {
    const router = makeRouter();
    await router.isReady();
    const store = useUserStore();
    store.isAdmin = true;
    const wrapper = mount(Sidebar, { global: { plugins: [router], directives: { autoscroll: autoScrollbar } } });
    const scrollEl = wrapper.find(".sidebar-scroll");
    expect(scrollEl.exists()).toBe(true);
    const navSections = scrollEl.findAll(".nav-section");
    expect(navSections.length).toBe(2);
  });

  // ── Inbox badge ───────────────────────────────────────────────────────────────

  it("inbox badge renders with count when inboxStore.count > 0", async () => {
    const router = makeRouter();
    await router.isReady();
    const wrapper = mount(Sidebar, { global: { plugins: [router], directives: { autoscroll: autoScrollbar } } });
    const inboxStore = useInboxStore();
    inboxStore.setCount(3);
    await nextTick();
    const badge = wrapper.find("[data-testid='inbox-badge']");
    expect(badge.exists()).toBe(true);
    expect(badge.text()).toBe("3");
  });

  it("inbox badge is absent when inboxStore.count === 0", async () => {
    const router = makeRouter();
    await router.isReady();
    const wrapper = mount(Sidebar, { global: { plugins: [router], directives: { autoscroll: autoScrollbar } } });
    const inboxStore = useInboxStore();
    inboxStore.setCount(0);
    await nextTick();
    expect(wrapper.find("[data-testid='inbox-badge']").exists()).toBe(false);
  });

  it("Sidebar calls inboxStore.refresh on mount", async () => {
    const router = makeRouter();
    await router.isReady();
    const pinia = createPinia();
    setActivePinia(pinia);
    const inboxStore = useInboxStore();
    const refreshSpy = vi.spyOn(inboxStore, "refresh").mockResolvedValue(undefined);
    mount(Sidebar, { global: { plugins: [router], directives: { autoscroll: autoScrollbar } } });
    await nextTick();
    expect(refreshSpy).toHaveBeenCalled();
  });

  it("Sidebar does NOT re-call inboxStore.refresh on identity change (no user-switch watch)", async () => {
    // The user-switch watch was removed: identity is fixed for the session by the
    // OIDC token. refresh() runs once on mount; a later setFromProfile() does not trigger it.
    const router = makeRouter();
    await router.isReady();
    const inboxStore = useInboxStore();
    const refreshSpy = vi.spyOn(inboxStore, "refresh").mockResolvedValue(undefined);
    const userStore = useUserStore();
    mount(Sidebar, { global: { plugins: [router], directives: { autoscroll: autoScrollbar } } });
    await nextTick();
    const callsAfterMount = refreshSpy.mock.calls.length;
    refreshSpy.mockClear();
    userStore.setFromProfile({ email: "alice@example.com", sub: "alice@example.com" });
    await nextTick();
    // No additional call beyond the mount-time refresh.
    expect(refreshSpy).not.toHaveBeenCalled();
    // Sanity: mount-time call did happen (captured before clear).
    expect(callsAfterMount).toBeGreaterThanOrEqual(1);
  });

  it("inbox badge renders in rail (collapsed) mode when count > 0", async () => {
    const router = makeRouter();
    await router.isReady();
    const wrapper = mount(Sidebar, { global: { plugins: [router], directives: { autoscroll: autoScrollbar } } });
    const inboxStore = useInboxStore();
    // collapse to rail mode first
    await wrapper.find("[data-testid='sidebar-toggle']").trigger("click");
    await nextTick();
    expect(wrapper.find(".sidebar").classes()).toContain("rail");
    // set count after mount so async refresh (returning []) doesn't override it
    inboxStore.setCount(2);
    await nextTick();
    const badge = wrapper.find("[data-testid='inbox-badge']");
    expect(badge.exists()).toBe(true);
  });

  // ── Dynamic assigned-agents list ──────────────────────────────────────────────

  it("renders assigned-agent links for each agent in agentsStore.assigned", async () => {
    const router = makeRouter();
    await router.isReady();
    const wrapper = mount(Sidebar, { global: { plugins: [router], directives: { autoscroll: autoScrollbar } } });
    const agentsStore = useAgentsStore();
    agentsStore.setAssigned([
      { id: "a1", name: "Sales Bot" },
      { id: "a2", name: "Dev Helper" },
    ]);
    await nextTick();
    const links = wrapper.findAll("[data-testid='assigned-agent']");
    expect(links.length).toBe(2);
    expect(links[0].text()).toContain("Sales Bot");
    expect(links[1].text()).toContain("Dev Helper");
  });

  it("assigned-agent links point to /chat?agent=<id>", async () => {
    const router = makeRouter();
    await router.isReady();
    const wrapper = mount(Sidebar, { global: { plugins: [router], directives: { autoscroll: autoScrollbar } } });
    const agentsStore = useAgentsStore();
    agentsStore.setAssigned([{ id: "abc123", name: "My Agent" }]);
    await nextTick();
    const link = wrapper.find("[data-testid='assigned-agent']");
    expect(link.exists()).toBe(true);
    expect(link.attributes("href")).toContain("agent=abc123");
  });

  it("assigned-agent list is empty when agentsStore.assigned is empty", async () => {
    const router = makeRouter();
    await router.isReady();
    const wrapper = mount(Sidebar, { global: { plugins: [router], directives: { autoscroll: autoScrollbar } } });
    const agentsStore = useAgentsStore();
    agentsStore.setAssigned([]);
    await nextTick();
    expect(wrapper.findAll("[data-testid='assigned-agent']").length).toBe(0);
  });

  it("Sidebar calls agentsStore.refresh on mount", async () => {
    const router = makeRouter();
    await router.isReady();
    const pinia = createPinia();
    setActivePinia(pinia);
    const agentsStore = useAgentsStore();
    const refreshSpy = vi.spyOn(agentsStore, "refresh").mockResolvedValue(undefined);
    mount(Sidebar, { global: { plugins: [router], directives: { autoscroll: autoScrollbar } } });
    await nextTick();
    expect(refreshSpy).toHaveBeenCalled();
  });

  // ── New chat button ───────────────────────────────────────────────────────────

  it("newChat resets buffer (no args) and navigates to /chat when no agent query", async () => {
    const router = makeRouter("/chat");
    await router.isReady();
    const pinia = createPinia();
    setActivePinia(pinia);
    const userStore = useUserStore();
    userStore.setFromProfile({ email: "alice@example.com", sub: "alice@example.com" });
    const { useChatStore } = await import("../store/chat.js");
    const chatStore = useChatStore();
    const resetSpy = vi.spyOn(chatStore, "reset");
    const clearSpy = vi.spyOn(chatStore, "clearActiveSession");
    const wrapper = mount(Sidebar, { global: { plugins: [router], directives: { autoscroll: autoScrollbar } } });
    await nextTick();

    await wrapper.find("[data-testid='new-chat-btn']").trigger("click");
    await nextTick();

    // reset() takes no args in new contract (buffer is conversation-scoped, not agent-scoped)
    expect(resetSpy).toHaveBeenCalledWith();
    expect(clearSpy).toHaveBeenCalled();
    expect(router.currentRoute.value.fullPath).toBe("/chat");
  });

  it("newChat resets buffer and navigates preserving agent param", async () => {
    const router = createRouter({ history: createWebHashHistory(), routes });
    await router.push("/chat?agent=bot-99");
    await router.isReady();
    const pinia = createPinia();
    setActivePinia(pinia);
    const userStore = useUserStore();
    userStore.setFromProfile({ email: "alice@example.com", sub: "alice@example.com" });
    const { useChatStore } = await import("../store/chat.js");
    const chatStore = useChatStore();
    const resetSpy = vi.spyOn(chatStore, "reset");
    const clearSpy = vi.spyOn(chatStore, "clearActiveSession");
    const wrapper = mount(Sidebar, { global: { plugins: [router], directives: { autoscroll: autoScrollbar } } });
    await nextTick();

    await wrapper.find("[data-testid='new-chat-btn']").trigger("click");
    await nextTick();

    // reset() takes no args — the buffer is not agent-scoped
    expect(resetSpy).toHaveBeenCalledWith();
    expect(clearSpy).toHaveBeenCalled();
    expect(router.currentRoute.value.query.agent).toBe("bot-99");
  });

  // Note: agentsStore.refresh on user switch is proven by the combination of:
  // 1. "Sidebar calls agentsStore.refresh on mount" (above) — same watch wiring
  // 2. "Sidebar calls inboxStore.refresh on user switch" — same watch callback
  // 3. agents-store unit tests proving refresh calls listMyAgents
  // A direct spy test is blocked by module-ordering in the full suite
  // (admin-agents.test.js loads api/agents.js before the mock takes effect here).
});
