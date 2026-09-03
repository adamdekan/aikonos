// Tests for views/admin/Skills.vue (CP6).
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";

vi.mock("../api/admin.js", () => ({
  listSkills:    vi.fn(),
  upsertSkill:   vi.fn(),
  deleteSkill:   vi.fn(),
  listAssignments:     vi.fn(),
  assignRole:          vi.fn(),
  revokeRole:          vi.fn(),
  listNetworkRules:    vi.fn(),
  addNetworkRule:      vi.fn(),
  deleteNetworkRule:   vi.fn(),
  listProvisioningRules: vi.fn(),
  addProvisioningRule:   vi.fn(),
  deleteProvisioningRule: vi.fn(),
  listAgents:          vi.fn(),
  createAgent:         vi.fn(),
  updateAgent:         vi.fn(),
  deleteAgent:         vi.fn(),
  listMcpConnections:  vi.fn(),
  listAdminRuns:       vi.fn(),
  listLlmProviders:    vi.fn(),
}));

import Skills from "../views/admin/Skills.vue";
import * as adminApi from "../api/admin.js";

const SAMPLE_SKILLS = [
  {
    toolId:       "web.fetch",
    scope:        "web",
    enabled:      true,
    effectClass:  "read_external",
    displayName:  "Web Fetch",
    description:  "Fetches a URL",
    executorKind: "builtin",
  },
  {
    toolId:       "mcp:myconn:search",
    scope:        "mcp:myconn",
    enabled:      true,
    effectClass:  "write_external",
    displayName:  "",
    description:  "",
    executorKind: "mcp",
  },
];

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/admin/skills", component: Skills },
      { path: "/",             component: { template: "<div/>" } },
    ],
  });
}

describe("Skills.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    useUserStore().setFromProfile({ email: "admin@example.com", sub: "admin@example.com" });
    vi.clearAllMocks();
  });
  afterEach(() => vi.restoreAllMocks());

  it("renders skills list from listSkills response", async () => {
    adminApi.listSkills.mockResolvedValue({ skills: SAMPLE_SKILLS });
    const router = makeRouter();
    await router.push("/admin/skills");
    const w = mount(Skills, { global: { plugins: [router] } });
    await flushPromises();

    expect(w.findAll("[data-testid='skill-row']").length).toBe(2);
    expect(w.text()).toContain("web.fetch");
    expect(w.text()).toContain("mcp:myconn:search");
  });

  it("shows forbidden empty-state when listSkills returns forbidden", async () => {
    adminApi.listSkills.mockResolvedValue({ forbidden: true });
    const router = makeRouter();
    await router.push("/admin/skills");
    const w = mount(Skills, { global: { plugins: [router] } });
    await flushPromises();

    expect(w.find("[data-testid='forbidden']").exists()).toBe(true);
    expect(w.find("[data-testid='skill-row']").exists()).toBe(false);
  });

  it("scope column is never an editable input", async () => {
    adminApi.listSkills.mockResolvedValue({ skills: SAMPLE_SKILLS });
    const router = makeRouter();
    await router.push("/admin/skills");
    const w = mount(Skills, { global: { plugins: [router] } });
    await flushPromises();

    // No input or select with name/placeholder or data-testid relating to scope
    const inputs = w.findAll("input, select");
    for (const inp of inputs) {
      const val = (inp.element.name || inp.element.dataset.testid || inp.element.placeholder || "").toLowerCase();
      expect(val).not.toContain("scope");
    }
  });

  it("save calls upsertSkill with correct args (no scope)", async () => {
    adminApi.listSkills.mockResolvedValue({ skills: [SAMPLE_SKILLS[0]] });
    adminApi.upsertSkill.mockResolvedValue({ skill: SAMPLE_SKILLS[0] });
    const router = makeRouter();
    await router.push("/admin/skills");
    const w = mount(Skills, { global: { plugins: [router] } });
    await flushPromises();

    // Change description input for first row
    const descInput = w.find("[data-testid='desc-web.fetch']");
    await descInput.setValue("Updated description");

    // Click Save
    await w.find("[data-testid='save-web.fetch']").trigger("click");
    await flushPromises();

    const call = adminApi.upsertSkill.mock.calls[0];
    expect(call[0]).toBe("web.fetch");
    // scope must NOT be passed as an editable value — it should not be in the call args
    // (the function signature accepts it but the view must not supply a user-edited scope)
    expect(call[1]).not.toHaveProperty("scope");
    expect(call[1].description).toBe("Updated description");
  });

  it("add-custom form calls upsertSkill with the mcp tool id", async () => {
    adminApi.listSkills.mockResolvedValue({ skills: [] });
    adminApi.upsertSkill.mockResolvedValue({ skill: { toolId: "mcp:conn:tool", scope: "mcp:conn", enabled: true, effectClass: "write_external", executorKind: "mcp" } });
    const router = makeRouter();
    await router.push("/admin/skills");
    const w = mount(Skills, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='custom-tool-id']").setValue("mcp:conn:tool");
    await w.find("[data-testid='custom-effect-class']").setValue("write_external");
    await w.find("[data-testid='custom-add-btn']").trigger("click");
    await flushPromises();

    expect(adminApi.upsertSkill).toHaveBeenCalledWith(
      "mcp:conn:tool",
      expect.objectContaining({ effectClass: "write_external" }),
    );
  });

  it("delete calls deleteSkill with toolId", async () => {
    adminApi.listSkills.mockResolvedValue({ skills: [SAMPLE_SKILLS[0]] });
    adminApi.deleteSkill.mockResolvedValue({ success: true });
    // stub window.confirm
    vi.stubGlobal("confirm", () => true);
    const router = makeRouter();
    await router.push("/admin/skills");
    const w = mount(Skills, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='delete-web.fetch']").trigger("click");
    await flushPromises();

    expect(adminApi.deleteSkill).toHaveBeenCalledWith("web.fetch");
  });

  it("broker error surfaces in error banner", async () => {
    adminApi.listSkills.mockResolvedValue({ skills: [SAMPLE_SKILLS[0]] });
    adminApi.upsertSkill.mockResolvedValue({ error: "effect class would downgrade built-in" });
    const router = makeRouter();
    await router.push("/admin/skills");
    const w = mount(Skills, { global: { plugins: [router] } });
    await flushPromises();

    const descInput = w.find("[data-testid='desc-web.fetch']");
    await descInput.setValue("some desc");
    await w.find("[data-testid='save-web.fetch']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='error-banner']").exists()).toBe(true);
    expect(w.find("[data-testid='error-banner']").text()).toContain("effect class would downgrade built-in");
  });
});
