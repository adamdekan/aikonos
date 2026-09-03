// Tests for Agents.vue — Fix 1 (live skills) + Fix 2 (soul in create mode).
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

const SAMPLE_SKILLS = [
  { toolId: "doc.read",        displayName: "Doc Read",    enabled: true,  effectClass: "read"  },
  { toolId: "web.fetch",       displayName: "Web Fetch",   enabled: true,  effectClass: "write" },
  { toolId: "mcp:my-server",   displayName: "MCP Server",  enabled: true,  effectClass: "mcp"   },
  { toolId: "email.draft",     displayName: "Email Draft", enabled: false, effectClass: "write" },
  { toolId: "workspace.read",  displayName: "Workspace",   enabled: true,  effectClass: "read"  },
  // enabled field absent (undefined) — must be treated as enabled (filter is `!== false`, not `=== true`)
  { toolId: "siem.query",      displayName: "SIEM Query",                  effectClass: "read"  },
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

describe("Agents.vue — live skills + create-mode soul", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    useUserStore().setFromProfile({ email: "admin@example.com", sub: "admin@example.com" });
    vi.clearAllMocks();
    adminApi.listAgents.mockResolvedValue({ agents: [] });
    adminApi.listMcpConnections.mockResolvedValue({ connections: [] });
    adminApi.listLlmProviders.mockResolvedValue({ providers: [] });
    adminApi.listAgentApiKeys.mockResolvedValue({ keys: [] });
    adminApi.listSkills.mockResolvedValue({ skills: SAMPLE_SKILLS });
    agentsApi.getAgentSoul.mockResolvedValue({ soul: "" });
    agentsApi.setAgentSoul.mockResolvedValue({});
  });
  afterEach(() => vi.restoreAllMocks());

  // Fix 1 — Test 1: Skills checkbox group renders only enabled non-mcp skills from listSkills()
  it("renders one checkbox per enabled non-mcp skill from listSkills(); mcp: and disabled skills absent", async () => {
    const router = makeRouter();
    await router.push("/admin/agents");
    const w = mount(Agents, { global: { plugins: [router], attachTo: document.body } });
    await flushPromises();

    await w.find("[data-testid='agent-create-btn']").trigger("click");
    await flushPromises();

    // doc.read, web.fetch, workspace.read are enabled non-mcp → should appear
    expect(document.querySelector("[data-testid='skill-doc.read']")).not.toBeNull();
    expect(document.querySelector("[data-testid='skill-web.fetch']")).not.toBeNull();
    expect(document.querySelector("[data-testid='skill-workspace.read']")).not.toBeNull();

    // mcp:my-server is mcp: prefixed → must NOT appear in skills group
    expect(document.querySelector("[data-testid='skill-mcp:my-server']")).toBeNull();

    // email.draft is enabled:false → must NOT appear
    expect(document.querySelector("[data-testid='skill-email.draft']")).toBeNull();

    // siem.query has no enabled field (undefined) → must appear (filter is !== false, not === true)
    expect(document.querySelector("[data-testid='skill-siem.query']")).not.toBeNull();

    w.unmount();
  });

  // Fix 2 — Test 2: Personality textarea present in CREATE mode
  it("personality textarea is present in create mode (not gated away)", async () => {
    const router = makeRouter();
    await router.push("/admin/agents");
    const w = mount(Agents, { global: { plugins: [router], attachTo: document.body } });
    await flushPromises();

    await w.find("[data-testid='agent-create-btn']").trigger("click");
    await flushPromises();

    expect(document.querySelector("[data-testid='agent-soul']")).not.toBeNull();
    w.unmount();
  });

  // Fix 2 — Test 3: CREATE with non-empty soul calls createAgent then setAgentSoul(newId, soul)
  it("create with soul calls createAgent then setAgentSoul with the returned id", async () => {
    const newAgent = { id: "new-id", name: "agent-x" };
    adminApi.createAgent.mockResolvedValue({ agent: newAgent });

    const router = makeRouter();
    await router.push("/admin/agents");
    const w = mount(Agents, { global: { plugins: [router], attachTo: document.body } });
    await flushPromises();

    await w.find("[data-testid='agent-create-btn']").trigger("click");
    await flushPromises();

    // Set name (required)
    const nameInput = document.querySelector("[data-testid='agent-name']");
    nameInput.value = "agent-x";
    nameInput.dispatchEvent(new Event("input"));

    // Set soul
    const ta = document.querySelector("[data-testid='agent-soul']");
    ta.value = "Be concise.";
    ta.dispatchEvent(new Event("input"));
    await flushPromises();

    document.querySelector("[data-testid='agent-save-btn']").click();
    await flushPromises();

    expect(adminApi.createAgent).toHaveBeenCalledTimes(1);
    expect(agentsApi.setAgentSoul).toHaveBeenCalledWith("new-id", "Be concise.");

    // Verify order: createAgent resolved before setAgentSoul was called
    const createOrder = adminApi.createAgent.mock.invocationCallOrder[0];
    const soulOrder   = agentsApi.setAgentSoul.mock.invocationCallOrder[0];
    expect(createOrder).toBeLessThan(soulOrder);

    w.unmount();
  });

  // Fix 2 — Test 4: Soul > 4096 bytes in CREATE mode blocks submit; createAgent NOT called
  it("soul > 4096 bytes in create mode blocks submit and sets formError", async () => {
    const router = makeRouter();
    await router.push("/admin/agents");
    const w = mount(Agents, { global: { plugins: [router], attachTo: document.body } });
    await flushPromises();

    await w.find("[data-testid='agent-create-btn']").trigger("click");
    await flushPromises();

    const nameInput = document.querySelector("[data-testid='agent-name']");
    nameInput.value = "agent-x";
    nameInput.dispatchEvent(new Event("input"));

    const ta = document.querySelector("[data-testid='agent-soul']");
    ta.value = "x".repeat(4097);
    ta.dispatchEvent(new Event("input"));
    await flushPromises();

    document.querySelector("[data-testid='agent-save-btn']").click();
    await flushPromises();

    expect(adminApi.createAgent).not.toHaveBeenCalled();
    // formError must be visible somewhere in the modal
    expect(document.body.innerHTML).toContain("4096");

    w.unmount();
  });

  // Fix 2 — Test 5: Partial failure — createAgent succeeds but setAgentSoul rejects.
  // Contract: createAgent called, formError set (distinct "created but personality failed" message),
  // list still reloads (listAgents called a second time).
  it("setAgentSoul rejection after successful createAgent sets distinct formError and still reloads list", async () => {
    const newAgent = { id: "new-id", name: "agent-x" };
    adminApi.createAgent.mockResolvedValue({ agent: newAgent });
    agentsApi.setAgentSoul.mockRejectedValue(new Error("soul write failed"));

    const router = makeRouter();
    await router.push("/admin/agents");
    const w = mount(Agents, { global: { plugins: [router], attachTo: document.body } });
    await flushPromises();

    // listAgents was called once on mount
    const loadCallsAfterMount = adminApi.listAgents.mock.calls.length;

    await w.find("[data-testid='agent-create-btn']").trigger("click");
    await flushPromises();

    const nameInput = document.querySelector("[data-testid='agent-name']");
    nameInput.value = "agent-x";
    nameInput.dispatchEvent(new Event("input"));

    const ta = document.querySelector("[data-testid='agent-soul']");
    ta.value = "Be concise.";
    ta.dispatchEvent(new Event("input"));
    await flushPromises();

    document.querySelector("[data-testid='agent-save-btn']").click();
    await flushPromises();

    // (a) createAgent was called
    expect(adminApi.createAgent).toHaveBeenCalledTimes(1);

    // (b) formError is set; message must mention "created" (success) AND failure of personality/soul
    //     — distinct from a plain create failure which would not mention "created"
    const bodyText = document.body.innerHTML;
    expect(bodyText).toMatch(/created/i);
    expect(bodyText).toMatch(/personality|soul/i);

    // (c) list reloads — listAgents called at least once more after mount
    expect(adminApi.listAgents.mock.calls.length).toBeGreaterThan(loadCallsAfterMount);

    w.unmount();
  });
});
