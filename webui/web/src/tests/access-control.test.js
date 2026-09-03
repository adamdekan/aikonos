// Characterization test net for AccessControl.vue (CP1).
// Pins the assembled monolith's behavior — all four tabs — via data-testid
// hooks and assignRole/revokeRole mock-call assertions.
// access-derive.js is real (not mocked). api/admin.js is fully mocked.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";

vi.mock("../api/admin.js", () => ({
  listAssignments:        vi.fn(),
  assignRole:             vi.fn(),
  revokeRole:             vi.fn(),
  listMcpConnections:     vi.fn(),
  listAgents:             vi.fn(),
  listSkills:             vi.fn(),
  listSkillBundles:       vi.fn(),
  // Members.vue (embedded as the default tab) imports this
  listMembers:            vi.fn(),
  // Provisioning.vue and AdvancedTuples.vue also import these
  listNetworkRules:       vi.fn(),
  addNetworkRule:         vi.fn(),
  deleteNetworkRule:      vi.fn(),
  listProvisioningRules:  vi.fn(),
  addProvisioningRule:    vi.fn(),
  deleteProvisioningRule: vi.fn(),
  listAdminRuns:          vi.fn(),
  listLlmProviders:       vi.fn(),
  listAlerts:             vi.fn(),
  createAgent:            vi.fn(),
  updateAgent:            vi.fn(),
  deleteAgent:            vi.fn(),
}));

import AccessControl from "../views/admin/AccessControl.vue";
import * as adminApi from "../api/admin.js";

// ── fixtures ──────────────────────────────────────────────────────────────────

// Two users
const USER_ALICE = { id: "user:alice@example.com", kind: "user", displayName: "Alice", email: "alice@example.com" };
const USER_BOB   = { id: "user:bob@example.com",   kind: "user", displayName: "Bob",   email: "bob@example.com" };

// One tenant
const TENANT_REF = "tenant:aikonos";

// Tuples:
//   - alice is member of security-team
//   - security-team has web.fetch + doc.write (permitted_group)
//   - security-team has bundle-1 (can_use)
//   - alice has admin tenant role
//   - bob has member tenant role
//   - bob has direct permitted_group on email.draft (stale direct grant)
//   - agent-1 owned by alice
//   - agent-1 usable_by bob
//   - agent-1 permitted_agent on mcp-conn-1
//   - security-team delegatable self-ref
const TUPLES = [
  { user: "user:alice@example.com", relation: "member",         object: "group:security-team" },
  { user: "group:security-team#member", relation: "permitted_group", object: "skill:web.fetch" },
  { user: "group:security-team#member", relation: "permitted_group", object: "skill:doc.write" },
  { user: "group:security-team#member", relation: "can_use",         object: "agentskill:bundle-1" },
  { user: "user:alice@example.com", relation: "admin",          object: TENANT_REF },
  { user: "user:bob@example.com",   relation: "member",         object: TENANT_REF },
  { user: "user:bob@example.com",   relation: "permitted_group",object: "skill:email.draft" },
  { user: "user:alice@example.com", relation: "owner_user",     object: "agent:agent-1" },
  { user: "user:bob@example.com",   relation: "usable_by",      object: "agent:agent-1" },
  { user: "agent:agent-1",         relation: "permitted_agent",object: "mcp_connector:mcp-conn-1" },
  { user: "group:security-team#member", relation: "delegatable", object: "group:security-team" },
  // second group for group-drop tests
  { user: "user:bob@example.com", relation: "member", object: "group:ops-team" },
];

const PRINCIPALS = [USER_ALICE, USER_BOB];

const SKILLS = [
  { toolId: "web.fetch",   scope: "web" },
  { toolId: "doc.write",   scope: "doc" },
  { toolId: "email.draft", scope: "email" },
];

const SKILL_BUNDLES = [
  { id: "bundle-1", name: "Research Bundle", description: "Web + doc bundle" },
];

const AGENTS = [
  { id: "agent-1", name: "Research Agent" },
  { id: "agent-2", name: "Ops Agent" },
];

const MCP_CONNECTIONS = [
  { id: "mcp-conn-1", name: "Jira Connector" },
  { id: "mcp-conn-2", name: "Slack Connector" },
];

function makeAssignmentsResp() {
  return {
    tuples:     TUPLES,
    principals: PRINCIPALS,
    fgaEnabled: true,
    warnings:   [],
  };
}

function setupMocks() {
  adminApi.listAssignments.mockResolvedValue(makeAssignmentsResp());
  adminApi.assignRole.mockResolvedValue({});
  adminApi.revokeRole.mockResolvedValue({});
  adminApi.listMcpConnections.mockResolvedValue({ connections: MCP_CONNECTIONS });
  adminApi.listAgents.mockResolvedValue({ agents: AGENTS });
  adminApi.listSkills.mockResolvedValue({ skills: SKILLS });
  adminApi.listSkillBundles.mockResolvedValue({ bundles: SKILL_BUNDLES });
  adminApi.listMembers.mockResolvedValue({ members: [] });
  adminApi.listProvisioningRules.mockResolvedValue({ rules: [] });
  adminApi.listNetworkRules.mockResolvedValue({ rules: [] });
  adminApi.listAdminRuns.mockResolvedValue({ runs: [] });
  adminApi.listLlmProviders.mockResolvedValue({ providers: [] });
}

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/admin/access", component: AccessControl },
      { path: "/",             component: { template: "<div/>" } },
    ],
  });
}

// tab: optional tab to activate after mount (Members is the default tab).
async function mountAC(tab) {
  const router = makeRouter();
  await router.push("/admin/access");
  const w = mount(AccessControl, { global: { plugins: [router] } });
  await flushPromises();
  if (tab) {
    await w.find(`[data-testid='tab-${tab}']`).trigger("click");
    await flushPromises();
  }
  return w;
}

describe("AccessControl.vue — Load + tabs", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    vi.clearAllMocks();
    setupMocks();
  });
  afterEach(() => vi.restoreAllMocks());

  it("renders Members tab by default", async () => {
    const w = await mountAC();
    expect(w.find("[data-testid='pane-Members']").exists()).toBe(true);
    expect(w.find("[data-testid='pane-Users']").exists()).toBe(false);
    expect(w.find("[data-testid='pane-Groups']").exists()).toBe(false);
    expect(w.find("[data-testid='pane-Tools']").exists()).toBe(false);
    expect(w.find("[data-testid='pane-Agents']").exists()).toBe(false);
  });

  it("tab strip shows all seven tabs", async () => {
    const w = await mountAC();
    for (const tab of ["Members", "Users", "Groups", "Tools", "Agents", "Provisioning", "Advanced"]) {
      expect(w.find(`[data-testid='tab-${tab}']`).exists()).toBe(true);
    }
  });

  it("clicking Groups tab shows Groups pane and hides Users pane", async () => {
    const w = await mountAC();
    await w.find("[data-testid='tab-Groups']").trigger("click");
    expect(w.find("[data-testid='pane-Groups']").exists()).toBe(true);
    expect(w.find("[data-testid='pane-Users']").exists()).toBe(false);
  });

  it("clicking Tools tab shows Tools pane", async () => {
    const w = await mountAC();
    await w.find("[data-testid='tab-Tools']").trigger("click");
    expect(w.find("[data-testid='pane-Tools']").exists()).toBe(true);
    expect(w.find("[data-testid='pane-Users']").exists()).toBe(false);
  });

  it("clicking Agents tab shows Agents pane", async () => {
    const w = await mountAC();
    await w.find("[data-testid='tab-Agents']").trigger("click");
    expect(w.find("[data-testid='pane-Agents']").exists()).toBe(true);
    expect(w.find("[data-testid='pane-Users']").exists()).toBe(false);
  });

  it("switching back to Users clears prior tab selection", async () => {
    const w = await mountAC("Users");
    // Select a user first
    await w.find(`[data-testid='user-row-${USER_ALICE.id}']`).trigger("click");
    expect(w.find("[data-testid='user-detail-pane']").text()).toContain("Alice");
    // Switch to Groups then back to Users
    await w.find("[data-testid='tab-Groups']").trigger("click");
    await w.find("[data-testid='tab-Users']").trigger("click");
    // Selection should be cleared
    expect(w.find("[data-testid='user-detail-pane']").text()).toContain("Select a user");
  });

  it("switching away from Groups clears search and selection", async () => {
    const w = await mountAC();
    await w.find("[data-testid='tab-Groups']").trigger("click");
    // Select a group
    await w.find("[data-testid='group-row-ops-team']").trigger("click");
    expect(w.find("[data-testid='group-detail-pane']").text()).toContain("ops-team");
    // Switch to Users then back to Groups
    await w.find("[data-testid='tab-Users']").trigger("click");
    await w.find("[data-testid='tab-Groups']").trigger("click");
    // Selection cleared
    expect(w.find("[data-testid='group-detail-pane']").text()).toContain("Select a group");
  });
});

describe("AccessControl.vue — Forbidden", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    vi.clearAllMocks();
  });
  afterEach(() => vi.restoreAllMocks());

  it("listAssignments → {forbidden:true} renders forbidden empty-state", async () => {
    adminApi.listAssignments.mockResolvedValue({ forbidden: true });
    adminApi.listMcpConnections.mockResolvedValue({ connections: [] });
    adminApi.listAgents.mockResolvedValue({ agents: [] });
    adminApi.listSkills.mockResolvedValue({ skills: [] });
    adminApi.listSkillBundles.mockResolvedValue({ bundles: [] });

    const w = await mountAC();

    expect(w.find("[data-testid='forbidden']").exists()).toBe(true);
    // Tab strip not shown when forbidden
    expect(w.find("[data-testid='tab-strip']").exists()).toBe(false);
  });
});

describe("AccessControl.vue — Users tab", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    vi.clearAllMocks();
    setupMocks();
  });
  afterEach(() => vi.restoreAllMocks());

  it("user list shows both users", async () => {
    const w = await mountAC("Users");
    expect(w.find(`[data-testid='user-row-${USER_ALICE.id}']`).exists()).toBe(true);
    expect(w.find(`[data-testid='user-row-${USER_BOB.id}']`).exists()).toBe(true);
  });

  it("selecting a user shows their groups", async () => {
    const w = await mountAC("Users");
    await w.find(`[data-testid='user-row-${USER_ALICE.id}']`).trigger("click");
    const detail = w.find("[data-testid='user-detail-pane']");
    expect(detail.text()).toContain("security-team");
  });

  it("selecting a user shows their tenant role", async () => {
    const w = await mountAC("Users");
    await w.find(`[data-testid='user-row-${USER_ALICE.id}']`).trigger("click");
    const detail = w.find("[data-testid='user-detail-pane']");
    expect(detail.text()).toContain("admin");
  });

  it("selecting a user shows effective skills via group membership", async () => {
    const w = await mountAC("Users");
    await w.find(`[data-testid='user-row-${USER_ALICE.id}']`).trigger("click");
    const detail = w.find("[data-testid='user-detail-pane']");
    expect(detail.text()).toContain("web.fetch");
    expect(detail.text()).toContain("doc.write");
  });

  it("selecting a user shows assigned skill bundles", async () => {
    const w = await mountAC("Users");
    await w.find(`[data-testid='user-row-${USER_ALICE.id}']`).trigger("click");
    const detail = w.find("[data-testid='user-detail-pane']");
    expect(detail.text()).toContain("Research Bundle");
  });

  it("add-to-group calls assignRole(user, 'member', 'group:<name>')", async () => {
    const w = await mountAC("Users");
    await w.find(`[data-testid='user-row-${USER_BOB.id}']`).trigger("click");

    const select = w.find("[data-testid='add-to-group-select']");
    // security-team is not in bob's current groups yet (bob is in ops-team)
    await select.setValue("security-team");
    await flushPromises();

    expect(adminApi.assignRole).toHaveBeenCalledWith(
      expect.objectContaining({
        user:     "user:bob@example.com",
        relation: "member",
        object:   "group:security-team",
      }),
    );
  });

  it("remove from group calls revokeRole(user, 'member', 'group:<name>')", async () => {
    const w = await mountAC("Users");
    await w.find(`[data-testid='user-row-${USER_ALICE.id}']`).trigger("click");

    await w.find("[data-testid='user-remove-group-security-team']").trigger("click");
    await flushPromises();

    expect(adminApi.revokeRole).toHaveBeenCalledWith(
      expect.objectContaining({
        user:     "user:alice@example.com",
        relation: "member",
        object:   "group:security-team",
      }),
    );
  });

  it("set-tenant-role member revokes existing role then assigns member", async () => {
    const w = await mountAC("Users");
    await w.find(`[data-testid='user-row-${USER_ALICE.id}']`).trigger("click");

    // Alice currently has admin. Setting member should revoke admin + admin (the existing), then assign member.
    await w.find("[data-testid='set-role-member']").trigger("click");
    await flushPromises();

    // revokeRole called with admin
    expect(adminApi.revokeRole).toHaveBeenCalledWith(
      expect.objectContaining({
        user:     "user:alice@example.com",
        relation: "admin",
        object:   TENANT_REF,
      }),
    );
    // assignRole called with member
    expect(adminApi.assignRole).toHaveBeenCalledWith(
      expect.objectContaining({
        user:     "user:alice@example.com",
        relation: "member",
        object:   TENANT_REF,
      }),
    );
  });

  it("set-tenant-role admin revokes existing and assigns admin", async () => {
    const w = await mountAC("Users");
    await w.find(`[data-testid='user-row-${USER_BOB.id}']`).trigger("click");

    // Bob currently has member role
    await w.find("[data-testid='set-role-admin']").trigger("click");
    await flushPromises();

    expect(adminApi.revokeRole).toHaveBeenCalledWith(
      expect.objectContaining({
        user:     "user:bob@example.com",
        relation: "member",
        object:   TENANT_REF,
      }),
    );
    expect(adminApi.assignRole).toHaveBeenCalledWith(
      expect.objectContaining({
        user:     "user:bob@example.com",
        relation: "admin",
        object:   TENANT_REF,
      }),
    );
  });

  it("direct skill revoke calls revokeRole(user, 'permitted_group', 'skill:<id>')", async () => {
    const w = await mountAC("Users");
    // Bob has direct email.draft grant
    await w.find(`[data-testid='user-row-${USER_BOB.id}']`).trigger("click");

    await w.find("[data-testid='revoke-direct-skill-email.draft']").trigger("click");
    await flushPromises();

    expect(adminApi.revokeRole).toHaveBeenCalledWith(
      expect.objectContaining({
        user:     "user:bob@example.com",
        relation: "permitted_group",
        object:   "skill:email.draft",
      }),
    );
  });

  it("create-group-and-add rejects bad name and shows error", async () => {
    const w = await mountAC("Users");
    await w.find(`[data-testid='user-row-${USER_ALICE.id}']`).trigger("click");

    // Show new group input
    await w.find("[data-testid='toggle-new-group-input']").trigger("click");
    await w.find("[data-testid='new-group-name-input']").setValue("INVALID NAME!");
    await w.find("[data-testid='new-group-name-input']").trigger("keydown.enter");

    expect(w.find("[data-testid='new-group-name-error']").exists()).toBe(true);
    expect(adminApi.assignRole).not.toHaveBeenCalled();
  });

  it("create-group-and-add with valid name calls assignRole", async () => {
    const w = await mountAC("Users");
    await w.find(`[data-testid='user-row-${USER_ALICE.id}']`).trigger("click");

    await w.find("[data-testid='toggle-new-group-input']").trigger("click");
    await w.find("[data-testid='new-group-name-input']").setValue("new-group-01");
    await w.find("[data-testid='new-group-name-input']").trigger("keydown.enter");
    await flushPromises();

    expect(adminApi.assignRole).toHaveBeenCalledWith(
      expect.objectContaining({
        user:     "user:alice@example.com",
        relation: "member",
        object:   "group:new-group-01",
      }),
    );
  });
});

describe("AccessControl.vue — Groups tab", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    vi.clearAllMocks();
    setupMocks();
  });
  afterEach(() => vi.restoreAllMocks());

  async function goGroups(w) {
    await w.find("[data-testid='tab-Groups']").trigger("click");
  }

  it("shows both groups in the list", async () => {
    const w = await mountAC();
    await goGroups(w);
    expect(w.find("[data-testid='group-row-security-team']").exists()).toBe(true);
    expect(w.find("[data-testid='group-row-ops-team']").exists()).toBe(true);
  });

  it("selecting a group shows members, managers, skills, bundles, delegation state", async () => {
    const w = await mountAC();
    await goGroups(w);
    await w.find("[data-testid='group-row-security-team']").trigger("click");

    const detail = w.find("[data-testid='group-detail-pane']");
    // member alice
    expect(detail.find("[data-testid='group-member-user:alice@example.com']").exists()).toBe(true);
    // skills
    expect(detail.text()).toContain("web.fetch");
    expect(detail.text()).toContain("doc.write");
    // bundle
    expect(detail.text()).toContain("Research Bundle");
    // delegation toggle on (ToggleSwitch, role=switch)
    const toggle = detail.find("[data-testid='delegation-toggle']");
    expect(toggle.attributes("aria-checked")).toBe("true");
  });

  it("add member via bulk checkbox calls assignRole(userRef, 'member', 'group:<n>')", async () => {
    const w = await mountAC();
    await goGroups(w);
    await w.find("[data-testid='group-row-security-team']").trigger("click");

    // Bob is not in security-team — he appears in the bulk checkbox list.
    const bobCb = w.find("[data-testid='bulk-member-cb-user:bob@example.com']");
    expect(bobCb.exists()).toBe(true);
    await bobCb.trigger("click");

    const bulkBtn = w.find("[data-testid='bulk-add-members-btn']");
    await bulkBtn.trigger("click");
    await flushPromises();

    expect(adminApi.assignRole).toHaveBeenCalledWith(
      expect.objectContaining({
        user:     "user:bob@example.com",
        relation: "member",
        object:   "group:security-team",
      }),
    );
  });

  it("remove member calls revokeRole(userRef, 'member', 'group:<n>')", async () => {
    const w = await mountAC();
    await goGroups(w);
    await w.find("[data-testid='group-row-security-team']").trigger("click");

    await w.find("[data-testid='group-remove-member-user:alice@example.com']").trigger("click");
    await flushPromises();

    expect(adminApi.revokeRole).toHaveBeenCalledWith(
      expect.objectContaining({
        user:     "user:alice@example.com",
        relation: "member",
        object:   "group:security-team",
      }),
    );
  });

  it("add skill via bulk checkbox calls assignRole('group:<n>#member', 'permitted_group', 'skill:<id>')", async () => {
    const w = await mountAC();
    await goGroups(w);
    await w.find("[data-testid='group-row-ops-team']").trigger("click");

    // ops-team has no skills — all registry skills appear in the bulk toggle list.
    const checkboxes = w.findAll(".bulk-checkbox-list [role='switch']");
    // Select just web.fetch
    const webFetchCb = checkboxes.find((cb) => {
      const label = cb.element.closest("label");
      return label && label.textContent.includes("web.fetch");
    });
    expect(webFetchCb).toBeTruthy();
    await webFetchCb.trigger("click");

    const bulkBtn = w.find("[data-testid='bulk-add-skills-btn']");
    await bulkBtn.trigger("click");
    await flushPromises();

    expect(adminApi.assignRole).toHaveBeenCalledWith(
      expect.objectContaining({
        user:     "group:ops-team#member",
        relation: "permitted_group",
        object:   "skill:web.fetch",
      }),
    );
  });

  it("add bundle calls assignRole('group:<n>#member', 'can_use', 'agentskill:<id>')", async () => {
    const w = await mountAC();
    await goGroups(w);
    await w.find("[data-testid='group-row-ops-team']").trigger("click");

    const select = w.find("[data-testid='add-bundle-select']");
    await select.setValue("bundle-1");
    await flushPromises();

    expect(adminApi.assignRole).toHaveBeenCalledWith(
      expect.objectContaining({
        user:     "group:ops-team#member",
        relation: "can_use",
        object:   "agentskill:bundle-1",
      }),
    );
  });

  it("delegation toggle enable calls assignRole with delegatable", async () => {
    const w = await mountAC();
    await goGroups(w);
    // ops-team has no delegatable tuple — toggle should be unchecked
    await w.find("[data-testid='group-row-ops-team']").trigger("click");

    const toggle = w.find("[data-testid='delegation-toggle']");
    expect(toggle.attributes("aria-checked")).toBe("false");
    await toggle.trigger("click");
    await flushPromises();

    expect(adminApi.assignRole).toHaveBeenCalledWith(
      expect.objectContaining({
        user:     "group:ops-team#member",
        relation: "delegatable",
        object:   "group:ops-team",
      }),
    );
  });

  it("delegation toggle disable calls revokeRole with delegatable", async () => {
    const w = await mountAC();
    await goGroups(w);
    // security-team is delegatable — toggle should be checked
    await w.find("[data-testid='group-row-security-team']").trigger("click");

    const toggle = w.find("[data-testid='delegation-toggle']");
    expect(toggle.attributes("aria-checked")).toBe("true");
    await toggle.trigger("click");
    await flushPromises();

    expect(adminApi.revokeRole).toHaveBeenCalledWith(
      expect.objectContaining({
        user:     "group:security-team#member",
        relation: "delegatable",
        object:   "group:security-team",
      }),
    );
  });

  it("add manager calls assignRole(userRef, 'manager', 'group:<n>')", async () => {
    const w = await mountAC();
    await goGroups(w);
    await w.find("[data-testid='group-row-security-team']").trigger("click");

    const select = w.find("[data-testid='add-manager-select']");
    // Alice or Bob are available as managers
    await select.setValue("user:bob@example.com");
    await flushPromises();

    expect(adminApi.assignRole).toHaveBeenCalledWith(
      expect.objectContaining({
        user:     "user:bob@example.com",
        relation: "manager",
        object:   "group:security-team",
      }),
    );
  });

  it("create-group rejects bad name and shows error", async () => {
    const w = await mountAC();
    await goGroups(w);

    await w.find("[data-testid='tab-Groups']").trigger("click");
    // Click "+ New group" button
    await w.find("[data-testid='toggle-new-group-grp']").trigger("click");

    await w.find("[data-testid='new-group-name-grp']").setValue("INVALID NAME!");
    await w.find("[data-testid='new-group-name-grp']").trigger("keydown.enter");

    expect(w.find("[data-testid='new-group-name-grp-error']").exists()).toBe(true);
    expect(adminApi.assignRole).not.toHaveBeenCalled();
  });

  it("bulk add members iterates assignRole for each selected member", async () => {
    // Add a third user to principals so both bob and charlie are non-members of security-team
    const CHARLIE = { id: "user:charlie@example.com", kind: "user", displayName: "Charlie", email: "charlie@example.com" };
    const tuples = [...TUPLES]; // alice is in security-team; bob and charlie are not
    adminApi.listAssignments.mockResolvedValue({
      tuples,
      principals: [USER_ALICE, USER_BOB, CHARLIE],
      fgaEnabled: true,
      warnings:   [],
    });

    const w = await mountAC();
    await goGroups(w);
    await w.find("[data-testid='group-row-security-team']").trigger("click");

    // Turn on bulk-add toggles for bob and charlie
    const checkboxes = w.findAll(".bulk-checkbox-list [role='switch']");
    // At least 2 members should be available (bob and charlie)
    expect(checkboxes.length).toBeGreaterThanOrEqual(2);
    for (const cb of checkboxes) {
      await cb.trigger("click");
    }

    const bulkBtn = w.find("[data-testid='bulk-add-members-btn']");
    expect(bulkBtn.attributes("disabled")).toBeUndefined();
    await bulkBtn.trigger("click");
    await flushPromises();

    // assignRole called once per selected member
    const calls = adminApi.assignRole.mock.calls;
    expect(calls.length).toBeGreaterThanOrEqual(2);
    for (const call of calls) {
      expect(call[0]).toMatchObject({ relation: "member", object: "group:security-team" });
    }
  });

  it("bulk add skills iterates assignRole for each selected skill", async () => {
    // Use ops-team which has no skills yet
    const w = await mountAC();
    await goGroups(w);
    await w.find("[data-testid='group-row-ops-team']").trigger("click");

    const checkboxes = w.findAll(".bulk-checkbox-list [role='switch']");
    expect(checkboxes.length).toBeGreaterThanOrEqual(1);
    for (const cb of checkboxes) {
      await cb.trigger("click");
    }

    const bulkBtn = w.find("[data-testid='bulk-add-skills-btn']");
    await bulkBtn.trigger("click");
    await flushPromises();

    const calls = adminApi.assignRole.mock.calls;
    expect(calls.length).toBeGreaterThanOrEqual(1);
    for (const call of calls) {
      expect(call[0]).toMatchObject({
        user:     "group:ops-team#member",
        relation: "permitted_group",
      });
      expect(call[0].object).toMatch(/^skill:/);
    }
  });

  it("create group with valid name sets synthetic selectedGroup immediately", async () => {
    const w = await mountAC();
    await goGroups(w);

    await w.find("[data-testid='toggle-new-group-grp']").trigger("click");

    await w.find("[data-testid='new-group-name-grp']").setValue("brand-new-group");
    await w.find("[data-testid='new-group-name-grp']").trigger("keydown.enter");

    // Synthetic group selected immediately — detail pane should show the group name
    expect(w.find("[data-testid='group-detail-pane']").text()).toContain("brand-new-group");
    // No error
    expect(w.find("[data-testid='new-group-name-grp-error']").exists()).toBe(false);
  });
});

describe("AccessControl.vue — Tools tab", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    vi.clearAllMocks();
    setupMocks();
  });
  afterEach(() => vi.restoreAllMocks());

  async function goTools(w) {
    await w.find("[data-testid='tab-Tools']").trigger("click");
  }

  it("skill list shows all skills from registry", async () => {
    const w = await mountAC();
    await goTools(w);
    expect(w.find("[data-testid='skill-row-web.fetch']").exists()).toBe(true);
    expect(w.find("[data-testid='skill-row-doc.write']").exists()).toBe(true);
    expect(w.find("[data-testid='skill-row-email.draft']").exists()).toBe(true);
  });

  it("selecting a skill lists granted groups", async () => {
    const w = await mountAC();
    await goTools(w);
    await w.find("[data-testid='skill-row-web.fetch']").trigger("click");

    const detail = w.find("[data-testid='tool-detail-pane']");
    // security-team has web.fetch
    expect(detail.find("[data-testid='skill-group-chip-security-team']").exists()).toBe(true);
  });

  it("selecting a skill lists effective users with provenance", async () => {
    const w = await mountAC();
    await goTools(w);
    await w.find("[data-testid='skill-row-web.fetch']").trigger("click");

    const detail = w.find("[data-testid='tool-detail-pane']");
    // Alice is in security-team which has web.fetch
    expect(detail.text()).toContain("Alice");
    expect(detail.text()).toContain("via security-team");
  });

  it("grant-to-group calls assignRole('group:<n>#member', 'permitted_group', 'skill:<id>')", async () => {
    const w = await mountAC();
    await goTools(w);
    await w.find("[data-testid='skill-row-web.fetch']").trigger("click");

    const select = w.find("[data-testid='grant-skill-to-group-select']");
    // ops-team doesn't have web.fetch yet
    await select.setValue("ops-team");
    await flushPromises();

    expect(adminApi.assignRole).toHaveBeenCalledWith(
      expect.objectContaining({
        user:     "group:ops-team#member",
        relation: "permitted_group",
        object:   "skill:web.fetch",
      }),
    );
  });

  it("revoke-from-group calls revokeRole('group:<n>#member', 'permitted_group', 'skill:<id>')", async () => {
    const w = await mountAC();
    await goTools(w);
    await w.find("[data-testid='skill-row-web.fetch']").trigger("click");

    await w.find("[data-testid='revoke-skill-group-security-team']").trigger("click");
    await flushPromises();

    expect(adminApi.revokeRole).toHaveBeenCalledWith(
      expect.objectContaining({
        user:     "group:security-team#member",
        relation: "permitted_group",
        object:   "skill:web.fetch",
      }),
    );
  });
});

describe("AccessControl.vue — Agents tab", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    vi.clearAllMocks();
    setupMocks();
  });
  afterEach(() => vi.restoreAllMocks());

  async function goAgents(w) {
    await w.find("[data-testid='tab-Agents']").trigger("click");
  }

  it("agent list shows all agents", async () => {
    const w = await mountAC();
    await goAgents(w);
    expect(w.find("[data-testid='agent-row-agent-1']").exists()).toBe(true);
    expect(w.find("[data-testid='agent-row-agent-2']").exists()).toBe(true);
  });

  it("selecting an agent shows owner, usable_by, MCP connectors", async () => {
    const w = await mountAC();
    await goAgents(w);
    await w.find("[data-testid='agent-row-agent-1']").trigger("click");

    const detail = w.find("[data-testid='agent-detail-pane']");
    // Owner: alice
    expect(detail.text()).toContain("Alice");
    // usable_by: bob
    expect(detail.text()).toContain("Bob");
    // MCP: mcp-conn-1
    expect(detail.text()).toContain("mcp-conn-1");
  });

  it("reassign owner revokes old then assigns new", async () => {
    const w = await mountAC();
    await goAgents(w);
    await w.find("[data-testid='agent-row-agent-1']").trigger("click");

    const select = w.find("[data-testid='agent-owner-select']");
    // Bob is not current owner (alice is), so bob should appear in options
    await select.setValue("user:bob@example.com");
    await flushPromises();

    // Revoke old owner (alice)
    expect(adminApi.revokeRole).toHaveBeenCalledWith(
      expect.objectContaining({
        user:     "user:alice@example.com",
        relation: "owner_user",
        object:   "agent:agent-1",
      }),
    );
    // Assign new owner (bob)
    expect(adminApi.assignRole).toHaveBeenCalledWith(
      expect.objectContaining({
        user:     "user:bob@example.com",
        relation: "owner_user",
        object:   "agent:agent-1",
      }),
    );
  });

  it("add usable_by (user) calls assignRole(userRef, 'usable_by', 'agent:<id>')", async () => {
    const w = await mountAC();
    await goAgents(w);
    // agent-2 has no usable_by yet
    await w.find("[data-testid='agent-row-agent-2']").trigger("click");

    const select = w.find("[data-testid='agent-usable-select']");
    await select.setValue("user:alice@example.com");
    await flushPromises();

    expect(adminApi.assignRole).toHaveBeenCalledWith(
      expect.objectContaining({
        user:     "user:alice@example.com",
        relation: "usable_by",
        object:   "agent:agent-2",
      }),
    );
  });

  it("add usable_by (group) calls assignRole(group#member, 'usable_by', 'agent:<id>')", async () => {
    const w = await mountAC();
    await goAgents(w);
    await w.find("[data-testid='agent-row-agent-2']").trigger("click");

    const select = w.find("[data-testid='agent-usable-select']");
    // group:security-team#member should appear in the dropdown
    await select.setValue("group:security-team#member");
    await flushPromises();

    expect(adminApi.assignRole).toHaveBeenCalledWith(
      expect.objectContaining({
        user:     "group:security-team#member",
        relation: "usable_by",
        object:   "agent:agent-2",
      }),
    );
  });

  it("add MCP connector calls assignRole('agent:<id>', 'permitted_agent', 'mcp_connector:<id>')", async () => {
    const w = await mountAC();
    await goAgents(w);
    await w.find("[data-testid='agent-row-agent-1']").trigger("click");

    const select = w.find("[data-testid='agent-mcp-select']");
    // mcp-conn-2 not yet assigned to agent-1
    await select.setValue("mcp-conn-2");
    await flushPromises();

    expect(adminApi.assignRole).toHaveBeenCalledWith(
      expect.objectContaining({
        user:     "agent:agent-1",
        relation: "permitted_agent",
        object:   "mcp_connector:mcp-conn-2",
      }),
    );
  });
});
