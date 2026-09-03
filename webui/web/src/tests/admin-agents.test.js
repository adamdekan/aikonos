// Tests for views/admin/Agents.vue (CP4).
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";

vi.mock("../api/agents.js", () => ({
  listMyAgents: vi.fn(),
  getAgentSoul: vi.fn().mockResolvedValue({ soul: "" }),
  setAgentSoul: vi.fn().mockResolvedValue({ soul: "" }),
}));

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

import Agents from "../views/admin/Agents.vue";
import * as adminApi from "../api/admin.js";
import * as agentsApi from "../api/agents.js";

const SAMPLE_AGENT = {
  id: "a1",
  name: "my-agent",
  llm_model: "gpt-4o",
  approval_mode: "needs_approval",
  skills: ["web.fetch", "doc.read"],
  mcp_servers: [],
  usable_by: ["user:alice@example.com"],
};

// The Model field offers only ids a configured provider actually carries, so any
// test that picks a model needs a provider offering it. "openai" is the tenant
// default, which is what decides whether a model needs Preferred provider set.
const SAMPLE_PROVIDERS = [
  { id: "openai", name: "OpenAI", enabled: true, isDefault: true, models: [{ id: "gpt-4o" }] },
  { id: "azure",  name: "Azure",  enabled: true, models: [{ id: "gpt-5.6-terra" }] },
];

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/admin/agents", component: Agents },
      { path: "/", component: { template: "<div/>" } },
    ],
  });
}

describe("Agents.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    useUserStore().setFromProfile({ email: "admin@example.com", sub: "admin@example.com" });
    vi.clearAllMocks();
    adminApi.listMcpConnections.mockResolvedValue({ connections: [] });
    adminApi.listLlmProviders.mockResolvedValue({ providers: SAMPLE_PROVIDERS });
    adminApi.listAgentApiKeys.mockResolvedValue({ keys: [] });
    adminApi.listSkills.mockResolvedValue({ skills: [] });
    agentsApi.getAgentSoul.mockResolvedValue({ soul: "" });
    agentsApi.setAgentSoul.mockResolvedValue({ soul: "" });
  });
  afterEach(() => vi.restoreAllMocks());

  it("renders agent list from API response", async () => {
    adminApi.listAgents.mockResolvedValue({ agents: [SAMPLE_AGENT] });
    const router = makeRouter();
    await router.push("/admin/agents");
    const w = mount(Agents, { global: { plugins: [router] } });
    await flushPromises();
    expect(w.findAll("[data-testid='agent-row']").length).toBe(1);
    expect(w.text()).toContain("my-agent");
    expect(w.find(".dt-table").exists()).toBe(true);
  });

  it("opens create modal when New Agent button is clicked", async () => {
    adminApi.listAgents.mockResolvedValue({ agents: [] });
    const router = makeRouter();
    await router.push("/admin/agents");
    const w = mount(Agents, { global: { plugins: [router], attachTo: document.body } });
    await flushPromises();

    expect(document.querySelector("[data-testid='agent-modal']")).toBeNull();
    await w.find("[data-testid='agent-create-btn']").trigger("click");
    await flushPromises();
    expect(document.querySelector("[data-testid='agent-modal']")).not.toBeNull();
    w.unmount();
  });

  it("createAgent is called with correct body on submit", async () => {
    adminApi.listAgents.mockResolvedValue({ agents: [] });
    adminApi.createAgent.mockResolvedValue({ agent: SAMPLE_AGENT });
    const router = makeRouter();
    await router.push("/admin/agents");
    const w = mount(Agents, { global: { plugins: [router], attachTo: document.body } });
    await flushPromises();

    await w.find("[data-testid='agent-create-btn']").trigger("click");
    await flushPromises();

    document.querySelector("[data-testid='agent-name']").value = "my-agent";
    document.querySelector("[data-testid='agent-name']").dispatchEvent(new Event("input"));
    document.querySelector("[data-testid='agent-model']").value = "openai::gpt-4o";
    document.querySelector("[data-testid='agent-model']").dispatchEvent(new Event("change"));
    document.querySelector("[data-testid='agent-approval']").value = "auto";
    document.querySelector("[data-testid='agent-approval']").dispatchEvent(new Event("change"));
    await flushPromises();

    document.querySelector("[data-testid='agent-save-btn']").click();
    await flushPromises();

    expect(adminApi.createAgent).toHaveBeenCalledWith(
      expect.objectContaining({
        name: "my-agent",
        llmModel: "gpt-4o",
        approvalMode: "auto",
        allowedProviders: [],
        // Derived from the model choice — the two are one selection now.
        preferredProvider: "openai",
        gatewayEnabled: false,
      }),
    );
    w.unmount();
  });

  it("deleteAgent is called with agent id on delete click", async () => {
    adminApi.listAgents.mockResolvedValue({ agents: [SAMPLE_AGENT] });
    adminApi.deleteAgent.mockResolvedValue({ success: true });
    const router = makeRouter();
    await router.push("/admin/agents");
    const w = mount(Agents, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='agent-delete-a1']").trigger("click");
    await flushPromises();
    expect(adminApi.deleteAgent).toHaveBeenCalledWith("a1");
  });

  it("renders not-an-admin empty-state on forbidden response", async () => {
    adminApi.listAgents.mockResolvedValue({ forbidden: true });
    const router = makeRouter();
    await router.push("/admin/agents");
    const w = mount(Agents, { global: { plugins: [router] } });
    await flushPromises();
    expect(w.find("[data-testid='forbidden']").exists()).toBe(true);
    expect(w.find("[data-testid='agent-row']").exists()).toBe(false);
  });

  it("shows no create button when forbidden", async () => {
    adminApi.listAgents.mockResolvedValue({ forbidden: true });
    const router = makeRouter();
    await router.push("/admin/agents");
    const w = mount(Agents, { global: { plugins: [router] } });
    await flushPromises();
    expect(w.find("[data-testid='agent-create-btn']").exists()).toBe(false);
  });

  it("assignRole is called with correct tuple when assigning a user to an agent", async () => {
    adminApi.listAgents.mockResolvedValue({ agents: [SAMPLE_AGENT] });
    adminApi.assignRole.mockResolvedValue({ success: true });
    const router = makeRouter();
    await router.push("/admin/agents");
    const w = mount(Agents, { global: { plugins: [router], attachTo: document.body } });
    await flushPromises();

    // Open edit modal for agent a1
    await w.find("[data-testid='agent-edit-a1']").trigger("click");
    await flushPromises();

    // Fill the assign-subject input
    const input = document.querySelector("[data-testid='assign-subject']");
    input.value = "user:alice@example.com";
    input.dispatchEvent(new Event("input"));
    await flushPromises();

    // Click assign
    document.querySelector("[data-testid='assign-btn']").click();
    await flushPromises();

    expect(adminApi.assignRole).toHaveBeenCalledWith({
      user:     "user:alice@example.com",
      relation: "usable_by",
      object:   "agent:a1",
      section:  4,
    });
    w.unmount();
  });

  it("revokeRole is called with correct tuple when revoking a user from an agent", async () => {
    adminApi.listAgents.mockResolvedValue({ agents: [SAMPLE_AGENT] });
    adminApi.revokeRole.mockResolvedValue({ success: true });
    const router = makeRouter();
    await router.push("/admin/agents");
    const w = mount(Agents, { global: { plugins: [router], attachTo: document.body } });
    await flushPromises();

    // Open edit modal for agent a1 (which has usable_by: ["user:alice@example.com"])
    await w.find("[data-testid='agent-edit-a1']").trigger("click");
    await flushPromises();

    // Click the revoke button for the existing assignment
    document.querySelector("[data-testid='revoke-assignment-btn']").click();
    await flushPromises();

    expect(adminApi.revokeRole).toHaveBeenCalledWith({
      user:     "user:alice@example.com",
      relation: "usable_by",
      object:   "agent:a1",
      section:  4,
    });
    w.unmount();
  });

  // ── Model field ────────────────────────────────────────────────────────────
  // Model was a free-text input beside a separate Preferred provider select, and
  // the two competed: the gateway honours a preferred provider only inside the
  // branch that also requires a model, so a provider alone did nothing, and a
  // model bound only when the serving provider listed it — otherwise the run
  // silently used that provider's first model. One select now carries the pair.

  async function openEditModal() {
    const router = makeRouter();
    await router.push("/admin/agents");
    const w = mount(Agents, { global: { plugins: [router], attachTo: document.body } });
    await flushPromises();
    await w.find("[data-testid='agent-edit-a1']").trigger("click");
    await flushPromises();
    return w;
  }

  it("replaces the separate provider select with one model choice", async () => {
    adminApi.listAgents.mockResolvedValue({ agents: [SAMPLE_AGENT] });
    const w = await openEditModal();

    expect(document.querySelector("[data-testid='agent-preferred-provider']")).toBeNull();
    const groups = [...document.querySelectorAll("[data-testid='agent-model'] optgroup")];
    expect(groups.map(g => g.label)).toEqual(["OpenAI", "Azure"]);
    // Each option carries its provider, so the same model id on two providers
    // stays distinguishable.
    const values = [...document.querySelectorAll("[data-testid='agent-model'] option")].map(o => o.value);
    expect(values).toEqual(["", "openai::gpt-4o", "azure::gpt-5.6-terra"]);
    w.unmount();
  });

  it("saves the chosen model and its provider from one selection", async () => {
    adminApi.listAgents.mockResolvedValue({ agents: [SAMPLE_AGENT] });
    adminApi.updateAgent.mockResolvedValue({ agent: SAMPLE_AGENT });
    const w = await openEditModal();

    const select = document.querySelector("[data-testid='agent-model']");
    select.value = "azure::gpt-5.6-terra";
    select.dispatchEvent(new Event("change"));
    await flushPromises();

    document.querySelector("[data-testid='agent-save-btn']").click();
    await flushPromises();

    const [, body] = adminApi.updateAgent.mock.calls[0];
    expect(body.llmModel).toBe("gpt-5.6-terra");
    expect(body.preferredProvider).toBe("azure");
    w.unmount();
  });

  it("pins a model stored without a provider to one that offers it", async () => {
    // Pre-existing rows have llm_model set and preferred_provider empty, which
    // only bound when the tenant default happened to list the model.
    adminApi.listAgents.mockResolvedValue({
      agents: [{ ...SAMPLE_AGENT, llm_model: "gpt-5.6-terra", preferred_provider: "" }],
    });
    adminApi.updateAgent.mockResolvedValue({ agent: SAMPLE_AGENT });
    const w = await openEditModal();

    expect(document.querySelector("[data-testid='agent-model']").value).toBe("azure::gpt-5.6-terra");

    document.querySelector("[data-testid='agent-save-btn']").click();
    await flushPromises();

    const [, body] = adminApi.updateAgent.mock.calls[0];
    expect(body.llmModel).toBe("gpt-5.6-terra");
    expect(body.preferredProvider).toBe("azure");
    w.unmount();
  });

  it("prefers the tenant default when several providers offer the model", async () => {
    adminApi.listLlmProviders.mockResolvedValue({
      providers: [
        { id: "azure",  name: "Azure",  enabled: true, models: [{ id: "gpt-4o" }] },
        { id: "openai", name: "OpenAI", enabled: true, isDefault: true, models: [{ id: "gpt-4o" }] },
      ],
    });
    adminApi.listAgents.mockResolvedValue({
      agents: [{ ...SAMPLE_AGENT, llm_model: "gpt-4o", preferred_provider: "" }],
    });
    const w = await openEditModal();

    expect(document.querySelector("[data-testid='agent-model']").value).toBe("openai::gpt-4o");
    w.unmount();
  });

  it("keeps a stored model no provider offers, rather than blanking it", async () => {
    adminApi.listAgents.mockResolvedValue({
      agents: [{ ...SAMPLE_AGENT, llm_model: "retired-model", preferred_provider: "openai" }],
    });
    adminApi.updateAgent.mockResolvedValue({ agent: SAMPLE_AGENT });
    const w = await openEditModal();

    expect(document.querySelector("[data-testid='agent-model']").value).toBe("openai::retired-model");
    expect(document.querySelector("[data-testid='agent-model-warning']").textContent)
      .toContain("No enabled provider offers retired-model");

    document.querySelector("[data-testid='agent-save-btn']").click();
    await flushPromises();

    const [, body] = adminApi.updateAgent.mock.calls[0];
    expect(body.llmModel).toBe("retired-model");
    expect(body.preferredProvider).toBe("openai");
    w.unmount();
  });

  it("inherit clears the model and any stored provider", async () => {
    adminApi.listAgents.mockResolvedValue({ agents: [SAMPLE_AGENT] });
    adminApi.updateAgent.mockResolvedValue({ agent: SAMPLE_AGENT });
    const w = await openEditModal();

    const select = document.querySelector("[data-testid='agent-model']");
    select.value = "";
    select.dispatchEvent(new Event("change"));
    await flushPromises();

    document.querySelector("[data-testid='agent-save-btn']").click();
    await flushPromises();

    const [, body] = adminApi.updateAgent.mock.calls[0];
    expect(body.llmModel).toBe("");
    // A provider with no model was inert, so inheriting must not leave one behind.
    expect(body.preferredProvider).toBe("");
    w.unmount();
  });

  it("stays silent for a model a provider actually offers", async () => {
    adminApi.listAgents.mockResolvedValue({ agents: [SAMPLE_AGENT] });
    const w = await openEditModal();
    expect(document.querySelector("[data-testid='agent-model-warning']")).toBeNull();
    w.unmount();
  });
});
