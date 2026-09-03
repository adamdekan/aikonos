// Tests for Agents.vue CP4 — soul/personality field in edit modal.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";

vi.mock("../api/admin.js", () => ({
  listAssignments:      vi.fn(),
  assignRole:           vi.fn(),
  revokeRole:           vi.fn(),
  listNetworkRules:     vi.fn(),
  addNetworkRule:       vi.fn(),
  deleteNetworkRule:    vi.fn(),
  listAdminRuns:        vi.fn(),
  listMcpConnections:   vi.fn(),
  addMcpConnection:     vi.fn(),
  updateMcpConnection:  vi.fn(),
  deleteMcpConnection:  vi.fn(),
  listAgents:           vi.fn(),
  createAgent:          vi.fn(),
  updateAgent:          vi.fn(),
  deleteAgent:          vi.fn(),
  mintAgentApiKey:      vi.fn(),
  listAgentApiKeys:     vi.fn(),
  revokeAgentApiKey:    vi.fn(),
  listLlmProviders:     vi.fn(),
  listSkills:           vi.fn(),
}));

vi.mock("../api/agents.js", () => ({
  listMyAgents:  vi.fn(),
  getAgentSoul:  vi.fn(),
  setAgentSoul:  vi.fn(),
}));

import Agents from "../views/admin/Agents.vue";
import * as adminApi from "../api/admin.js";
import * as agentsApi from "../api/agents.js";

const SAMPLE_AGENT = {
  id: "a1",
  name: "my-agent",
  llm_model: "gpt-4o",
  approval_mode: "needs_approval",
  skills: ["web.fetch"],
  mcp_servers: [],
  usable_by: [],
};

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/admin/agents", component: Agents },
      { path: "/", component: { template: "<div/>" } },
    ],
  });
}

describe("Agents.vue — soul/personality field", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    useUserStore().setFromProfile({ email: "admin@example.com", sub: "admin@example.com" });
    vi.clearAllMocks();
    adminApi.listMcpConnections.mockResolvedValue({ connections: [] });
    adminApi.listLlmProviders.mockResolvedValue({ providers: [] });
    adminApi.listAgentApiKeys.mockResolvedValue({ keys: [] });
    adminApi.listSkills.mockResolvedValue({ skills: [] });
    adminApi.listAgents.mockResolvedValue({ agents: [SAMPLE_AGENT] });
  });
  afterEach(() => vi.restoreAllMocks());

  it("openEdit loads soul from getAgentSoul and populates textarea", async () => {
    agentsApi.getAgentSoul.mockResolvedValue({ soul: "Be concise." });
    const router = makeRouter();
    await router.push("/admin/agents");
    const w = mount(Agents, { global: { plugins: [router], attachTo: document.body } });
    await flushPromises();

    await w.find("[data-testid='agent-edit-a1']").trigger("click");
    await flushPromises();

    expect(agentsApi.getAgentSoul).toHaveBeenCalledWith("a1");
    const ta = document.querySelector("[data-testid='agent-soul']");
    expect(ta).not.toBeNull();
    expect(ta.value).toBe("Be concise.");
    w.unmount();
  });

  it("openEdit with forbidden getAgentSoul leaves soul empty but still opens modal", async () => {
    agentsApi.getAgentSoul.mockResolvedValue({ forbidden: true });
    const router = makeRouter();
    await router.push("/admin/agents");
    const w = mount(Agents, { global: { plugins: [router], attachTo: document.body } });
    await flushPromises();

    await w.find("[data-testid='agent-edit-a1']").trigger("click");
    await flushPromises();

    const ta = document.querySelector("[data-testid='agent-soul']");
    expect(ta).not.toBeNull();
    expect(ta.value).toBe("");
    w.unmount();
  });

  it("submit (edit) calls setAgentSoul with the textarea value after updateAgent", async () => {
    agentsApi.getAgentSoul.mockResolvedValue({ soul: "" });
    agentsApi.setAgentSoul.mockResolvedValue({ soul: "You are helpful." });
    adminApi.updateAgent.mockResolvedValue({ agent: SAMPLE_AGENT });
    const router = makeRouter();
    await router.push("/admin/agents");
    const w = mount(Agents, { global: { plugins: [router], attachTo: document.body } });
    await flushPromises();

    // Open edit modal
    await w.find("[data-testid='agent-edit-a1']").trigger("click");
    await flushPromises();

    // Set textarea value
    const ta = document.querySelector("[data-testid='agent-soul']");
    ta.value = "You are helpful.";
    ta.dispatchEvent(new Event("input"));
    await flushPromises();

    // Save
    document.querySelector("[data-testid='agent-save-btn']").click();
    await flushPromises();

    expect(adminApi.updateAgent).toHaveBeenCalledWith("a1", expect.any(Object));
    expect(agentsApi.setAgentSoul).toHaveBeenCalledWith("a1", "You are helpful.");
    w.unmount();
  });

  it("save button is disabled and submit blocked when soul exceeds 4096 bytes", async () => {
    agentsApi.getAgentSoul.mockResolvedValue({ soul: "" });
    agentsApi.setAgentSoul.mockResolvedValue({ soul: "" });
    adminApi.updateAgent.mockResolvedValue({ agent: SAMPLE_AGENT });
    const router = makeRouter();
    await router.push("/admin/agents");
    const w = mount(Agents, { global: { plugins: [router], attachTo: document.body } });
    await flushPromises();

    await w.find("[data-testid='agent-edit-a1']").trigger("click");
    await flushPromises();

    // Set textarea to 4097 bytes of ascii
    const ta = document.querySelector("[data-testid='agent-soul']");
    ta.value = "x".repeat(4097);
    ta.dispatchEvent(new Event("input"));
    await flushPromises();

    const saveBtn = document.querySelector("[data-testid='agent-save-btn']");
    expect(saveBtn.disabled).toBe(true);

    // Click anyway — setAgentSoul must NOT be called
    saveBtn.click();
    await flushPromises();
    expect(agentsApi.setAgentSoul).not.toHaveBeenCalled();
    w.unmount();
  });

  it("personality textarea IS rendered in create mode (editId is null)", async () => {
    const router = makeRouter();
    await router.push("/admin/agents");
    const w = mount(Agents, { global: { plugins: [router], attachTo: document.body } });
    await flushPromises();

    // Open create modal (not edit)
    await w.find("[data-testid='agent-create-btn']").trigger("click");
    await flushPromises();

    expect(document.querySelector("[data-testid='agent-soul']")).not.toBeNull();
    w.unmount();
  });

  it("race guard: soul from agent A does not overwrite state after agent B's edit opens", async () => {
    // Agent A's soul fetch is slow; agent B's resolves first.
    const AGENT_A = { ...SAMPLE_AGENT, id: "a1", name: "agent-a" };
    const AGENT_B = { ...SAMPLE_AGENT, id: "b2", name: "agent-b" };
    adminApi.listAgents.mockResolvedValue({ agents: [AGENT_A, AGENT_B] });

    let resolveA;
    agentsApi.getAgentSoul.mockImplementation((id) => {
      if (id === "a1") return new Promise((res) => { resolveA = () => res({ soul: "soul-a" }); });
      return Promise.resolve({ soul: "soul-b" });
    });

    const router = makeRouter();
    await router.push("/admin/agents");
    const w = mount(Agents, { global: { plugins: [router], attachTo: document.body } });
    await flushPromises();

    // Open A — fetch is pending
    await w.find("[data-testid='agent-edit-a1']").trigger("click");
    // Open B before A resolves — editId is now "b2"
    await w.find("[data-testid='agent-edit-b2']").trigger("click");
    await flushPromises();  // B's soul resolves

    // Now resolve A's late response
    resolveA();
    await flushPromises();

    // fSoul must reflect B, not A
    const ta = document.querySelector("[data-testid='agent-soul']");
    expect(ta).not.toBeNull();
    expect(ta.value).toBe("soul-b");
    w.unmount();
  });

  it("setAgentSoul failure sets formError and keeps modal open", async () => {
    agentsApi.getAgentSoul.mockResolvedValue({ soul: "existing" });
    agentsApi.setAgentSoul.mockRejectedValue(new Error("soul write failed"));
    adminApi.updateAgent.mockResolvedValue({ agent: SAMPLE_AGENT });
    const router = makeRouter();
    await router.push("/admin/agents");
    const w = mount(Agents, { global: { plugins: [router] }, attachTo: document.body });
    await flushPromises();

    await w.find("[data-testid='agent-edit-a1']").trigger("click");
    await flushPromises();

    document.querySelector("[data-testid='agent-save-btn']").click();
    await flushPromises();

    // modal must still be open (teleported into document.body)
    expect(document.querySelector("[data-testid='agent-modal']")).not.toBeNull();
    // formError surfaced in the Name FormField error slot (inside teleport → check body)
    expect(document.body.innerHTML).toContain("soul write failed");
    w.unmount();
  });
});
