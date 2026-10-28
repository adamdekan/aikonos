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
  listMcpServerTools:   vi.fn(),
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
  registerAgentPublicKey: vi.fn(),
  listAgentPublicKeys:  vi.fn(),
  retireAgentPublicKey: vi.fn(),
  revokeAgentSessions:  vi.fn(),
  setAgentStatus:       vi.fn(),
  exportAgentInventory: vi.fn(),
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
    adminApi.listAgentPublicKeys.mockResolvedValue({ keys: [] });
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

  // AG-14: the inventory export downloads the gateway's file as is.
  it("exports the agent inventory as CSV and JSON", async () => {
    adminApi.listAgents.mockResolvedValue({ agents: [SAMPLE_AGENT] });
    adminApi.exportAgentInventory.mockResolvedValue({ text: "id,name\r\na1,my-agent\r\n" });
    const created = [];
    globalThis.URL.createObjectURL = vi.fn((blob) => { created.push(blob); return "blob:x"; });
    globalThis.URL.revokeObjectURL = vi.fn();
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
    const router = makeRouter();
    await router.push("/admin/agents");
    const w = mount(Agents, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='agent-inventory-export-csv']").trigger("click");
    await flushPromises();
    expect(adminApi.exportAgentInventory).toHaveBeenCalledWith("csv");
    expect(created[0].type).toBe("text/csv");
    const saved = await new Promise((resolve) => {
      const reader = new FileReader();
      reader.onload = () => resolve(reader.result);
      reader.readAsText(created[0]);
    });
    expect(saved).toBe("id,name\r\na1,my-agent\r\n");
    expect(click).toHaveBeenCalledTimes(1);

    await w.find("[data-testid='agent-inventory-export-json']").trigger("click");
    await flushPromises();
    expect(adminApi.exportAgentInventory).toHaveBeenLastCalledWith("json");

    adminApi.exportAgentInventory.mockResolvedValue({ forbidden: true });
    await w.find("[data-testid='agent-inventory-export-csv']").trigger("click");
    await flushPromises();
    expect(w.find("[data-testid='error-banner']").text()).toContain("tenant admin");
    expect(click).toHaveBeenCalledTimes(2);
  });

  // AG-06: revoke all sessions of an agent needs a reason and shows the epoch.
  it("revokes agent sessions with a reason and shows when", async () => {
    adminApi.listAgents.mockResolvedValue({ agents: [{ ...SAMPLE_AGENT, sessionsRevokedAt: "2026-10-28T12:00:00Z" }] });
    adminApi.revokeAgentSessions.mockResolvedValue({ notBefore: "2026-10-28T12:05:00Z" });
    const router = makeRouter();
    await router.push("/admin/agents");
    const w = mount(Agents, { global: { plugins: [router] }, attachTo: document.body });
    await flushPromises();
    expect(w.find("[data-testid='agent-sessions-revoked-a1']").exists()).toBe(true);

    await w.find("[data-testid='agent-revoke-sessions-a1']").trigger("click");
    await flushPromises();
    const confirm = document.querySelector("[data-testid='agent-revoke-confirm']");
    expect(confirm.disabled).toBe(true); // no reason yet
    const reason = document.querySelector("[data-testid='agent-revoke-reason']");
    reason.value = "key leaked";
    reason.dispatchEvent(new Event("input"));
    await flushPromises();
    document.querySelector("[data-testid='agent-revoke-confirm']").click();
    await flushPromises();
    expect(adminApi.revokeAgentSessions).toHaveBeenCalledWith("a1", "key leaked");
    expect(document.querySelector("[data-testid='agent-revoke-confirm']")).toBeNull();
    w.unmount();
  });

  // AG-03: status badge and lifecycle actions per status.
  it("shows the status badge and only the actions the status allows", async () => {
    adminApi.listAgents.mockResolvedValue({ agents: [
      { ...SAMPLE_AGENT, id: "s1", status: "staged" },
      { ...SAMPLE_AGENT, id: "a1", status: "active" },
      { ...SAMPLE_AGENT, id: "p1", status: "suspended", statusReason: "incident 42" },
      { ...SAMPLE_AGENT, id: "r1", status: "retired" },
      { ...SAMPLE_AGENT, id: "d1", status: "active", isDefault: true },
    ] });
    const router = makeRouter();
    await router.push("/admin/agents");
    const w = mount(Agents, { global: { plugins: [router] } });
    await flushPromises();
    const has = (id) => w.find(`[data-testid='${id}']`).exists();
    expect(w.find("[data-testid='agent-status-p1']").text()).toBe("suspended");
    expect(w.find("[data-testid='agent-status-p1']").attributes("title")).toBe("incident 42");
    expect([has("agent-activate-s1"), has("agent-suspend-s1"), has("agent-retire-s1")]).toEqual([true, false, true]);
    expect([has("agent-activate-a1"), has("agent-suspend-a1"), has("agent-retire-a1")]).toEqual([false, true, true]);
    expect([has("agent-activate-p1"), has("agent-suspend-p1"), has("agent-retire-p1")]).toEqual([true, false, true]);
    expect([has("agent-activate-r1"), has("agent-suspend-r1"), has("agent-retire-r1")]).toEqual([false, false, false]);
    expect([has("agent-suspend-d1"), has("agent-retire-d1")]).toEqual([true, false]);
  });

  it("activates directly and suspends with a reason", async () => {
    adminApi.listAgents.mockResolvedValue({ agents: [
      { ...SAMPLE_AGENT, id: "s1", status: "staged" },
      { ...SAMPLE_AGENT, id: "a1", status: "active" },
    ] });
    adminApi.setAgentStatus.mockResolvedValue({ agent: {} });
    const router = makeRouter();
    await router.push("/admin/agents");
    const w = mount(Agents, { global: { plugins: [router] }, attachTo: document.body });
    await flushPromises();

    await w.find("[data-testid='agent-activate-s1']").trigger("click");
    await flushPromises();
    expect(adminApi.setAgentStatus).toHaveBeenCalledWith("s1", "active");

    await w.find("[data-testid='agent-suspend-a1']").trigger("click");
    await flushPromises();
    expect(document.querySelector("[data-testid='agent-status-confirm']").disabled).toBe(true);
    const reason = document.querySelector("[data-testid='agent-status-reason']");
    reason.value = "incident 42";
    reason.dispatchEvent(new Event("input"));
    await flushPromises();
    document.querySelector("[data-testid='agent-status-confirm']").click();
    await flushPromises();
    expect(adminApi.setAgentStatus).toHaveBeenCalledWith("a1", "suspended", "incident 42");
    expect(document.querySelector("[data-testid='agent-status-confirm']")).toBeNull();
    w.unmount();
  });

  it("shows the broker refusal when activation needs an owner", async () => {
    adminApi.listAgents.mockResolvedValue({ agents: [{ ...SAMPLE_AGENT, id: "s1", status: "staged" }] });
    adminApi.setAgentStatus.mockRejectedValue(new Error("agent needs at least one owner who is not blocked before it can be activated"));
    const router = makeRouter();
    await router.push("/admin/agents");
    const w = mount(Agents, { global: { plugins: [router] } });
    await flushPromises();
    await w.find("[data-testid='agent-activate-s1']").trigger("click");
    await flushPromises();
    expect(w.find("[data-testid='error-banner']").text()).toContain("owner");
  });

  // AG-04: ListAgents flags an agent whose owners are all blocked (or gone).
  it("shows the orphaned badge only on orphaned agents", async () => {
    adminApi.listAgents.mockResolvedValue({ agents: [
      { ...SAMPLE_AGENT, orphaned: true },
      { ...SAMPLE_AGENT, id: "a2", name: "owned" },
    ] });
    const router = makeRouter();
    await router.push("/admin/agents");
    const w = mount(Agents, { global: { plugins: [router] } });
    await flushPromises();
    expect(w.find("[data-testid='agent-orphaned-a1']").exists()).toBe(true);
    expect(w.find("[data-testid='agent-orphaned-a2']").exists()).toBe(false);
  });

  // AG-01: the tenant default agent is badged and cannot be deleted.
  it("badges the default agent and offers no delete for it", async () => {
    adminApi.listAgents.mockResolvedValue({ agents: [
      { ...SAMPLE_AGENT, isDefault: true },
      { ...SAMPLE_AGENT, id: "a2", name: "named" },
    ] });
    const router = makeRouter();
    await router.push("/admin/agents");
    const w = mount(Agents, { global: { plugins: [router] } });
    await flushPromises();
    expect(w.find("[data-testid='agent-default-a1']").exists()).toBe(true);
    expect(w.find("[data-testid='agent-delete-a1']").exists()).toBe(false);
    expect(w.find("[data-testid='agent-default-a2']").exists()).toBe(false);
    expect(w.find("[data-testid='agent-delete-a2']").exists()).toBe(true);
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
        serviceAgent: false,
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

  // AG-08: per-connector tool checkboxes; the read-only preset picks exactly the
  // tools an admin confirmed read_only, and save sends the allowlist.
  it("offers per-connector MCP tools with a read-only preset", async () => {
    adminApi.listMcpConnections.mockResolvedValue({ connections: [{ id: "c1", name: "crm" }] });
    adminApi.listMcpServerTools.mockResolvedValue({ tools: [
      { toolId: "mcp:c1:read_rows",  name: "read_rows",  effectClass: "read_only",      effectClassConfirmed: true },
      { toolId: "mcp:c1:query_x",    name: "query_x",    effectClass: "destructive",    effectClassConfirmed: false },
      { toolId: "mcp:c1:write_rows", name: "write_rows", effectClass: "write_external", effectClassConfirmed: true },
    ] });
    adminApi.listAgents.mockResolvedValue({ agents: [{ ...SAMPLE_AGENT, mcp_servers: ["c1"], mcpTools: ["mcp:c1:write_rows"] }] });
    adminApi.updateAgent.mockResolvedValue({ agent: SAMPLE_AGENT });
    const w = await openEditModal();

    expect(adminApi.listMcpServerTools).toHaveBeenCalledWith("c1");
    expect(document.querySelector("[data-testid='mcptool-mcp:c1:query_x']")).not.toBeNull();
    expect(document.body.textContent).toContain("query_x (unconfirmed)");

    document.querySelector("[data-testid='mcp-preset-readonly-c1']").click();
    await flushPromises();
    document.querySelector("[data-testid='agent-save-btn']").click();
    await flushPromises();

    const [, body] = adminApi.updateAgent.mock.calls[0];
    expect(body.mcpServers).toEqual(["c1"]);
    expect(body.mcpTools).toEqual(["mcp:c1:read_rows"]);
    w.unmount();
  });

  // AG-15: the key list shows expiry and last use, warns within 7 days, and the
  // mint form defaults the lifetime to the tenant maximum.
  it("shows key expiry and last use, warns for keys expiring within 7 days", async () => {
    adminApi.listAgents.mockResolvedValue({ agents: [SAMPLE_AGENT] });
    const day = 86400000;
    adminApi.listAgentApiKeys.mockResolvedValue({
      maxTtlDays: 30,
      keys: [
        { id: "k1", keyPrefix: "tk_soon", label: "", expiresAt: new Date(Date.now() + 2 * day).toISOString(), lastUsedAt: null },
        { id: "k2", keyPrefix: "tk_late", label: "", expiresAt: new Date(Date.now() + 20 * day).toISOString(), lastUsedAt: new Date().toISOString() },
      ],
    });
    adminApi.mintAgentApiKey.mockResolvedValue({ rawKey: "tk_new" });
    const w = await openEditModal();

    const expiry = [...document.querySelectorAll("[data-testid='api-key-expiry']")];
    expect(expiry.map(e => e.classList.contains("key-expiring"))).toEqual([true, false]);
    const used = [...document.querySelectorAll("[data-testid='api-key-last-used']")].map(e => e.textContent);
    expect(used[0]).toBe("Never used");
    expect(used[1]).toMatch(/^Last used /);

    expect(document.querySelector("[data-testid='mint-key-ttl']").value).toBe("30");
    document.querySelector("[data-testid='mint-key-btn']").click();
    await flushPromises();
    expect(adminApi.mintAgentApiKey).toHaveBeenCalledWith("a1", "", 30);
    w.unmount();
  });

  // AG-05: public keys are listed with kid, alg, expiry and last use; the
  // form registers a pasted key and each row retires its key.
  it("lists, registers and retires agent public keys", async () => {
    adminApi.listAgents.mockResolvedValue({ agents: [SAMPLE_AGENT] });
    adminApi.listAgentPublicKeys.mockResolvedValue({
      maxTtlDays: 30,
      keys: [{ id: "p1", kid: "k1", alg: "ES256", expiresAt: new Date(Date.now() + 20 * 86400000).toISOString(), lastUsedAt: null }],
    });
    adminApi.registerAgentPublicKey.mockResolvedValue({ key: {} });
    adminApi.retireAgentPublicKey.mockResolvedValue({ success: true });
    const w = await openEditModal();

    expect(document.querySelectorAll("[data-testid='public-key-row']").length).toBe(1);
    expect(document.querySelector("[data-testid='public-key-kid']").textContent).toBe("k1");
    expect(document.querySelector("[data-testid='public-key-alg']").textContent).toBe("ES256");
    expect(document.querySelector("[data-testid='public-key-last-used']").textContent).toBe("Never used");
    expect(document.querySelector("[data-testid='public-key-ttl']").value).toBe("30");

    // Empty paste is refused client-side.
    document.querySelector("[data-testid='register-public-key-btn']").click();
    await flushPromises();
    expect(adminApi.registerAgentPublicKey).not.toHaveBeenCalled();
    expect(document.querySelector("[data-testid='public-key-error']").textContent).toMatch(/Paste/);

    const ta = document.querySelector("[data-testid='public-key-input']");
    ta.value = "-----BEGIN PUBLIC KEY-----\nabc\n-----END PUBLIC KEY-----";
    ta.dispatchEvent(new Event("input"));
    const kid = document.querySelector("[data-testid='public-key-kid-input']");
    kid.value = "k2";
    kid.dispatchEvent(new Event("input"));
    document.querySelector("[data-testid='register-public-key-btn']").click();
    await flushPromises();
    expect(adminApi.registerAgentPublicKey).toHaveBeenCalledWith("a1", "-----BEGIN PUBLIC KEY-----\nabc\n-----END PUBLIC KEY-----", "k2", 30);

    document.querySelector("[data-testid='retire-public-key-p1']").click();
    await flushPromises();
    expect(adminApi.retireAgentPublicKey).toHaveBeenCalledWith("a1", "p1");
    w.unmount();
  });

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
