// Settings-modal Memory pane: scope tabs, concept list, detail, and the three manage actions.
// Server enforces the authz matrix — these tests pin the surface, not the gates.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import MemorySettings from "../components/MemorySettings.vue";
import UserSettingsModal from "../components/UserSettingsModal.vue";
import { useUserStore } from "../store/user.js";

// vi.mock factories are hoisted above the module body — vi.hoisted is what lets
// the spies be shared with the assertions below.
const memoryApi = vi.hoisted(() => ({
  listMemoryGroups: vi.fn(),
  listMemoryConcepts: vi.fn(),
  getMemoryConcept: vi.fn(),
  verifyMemoryConcept: vi.fn(),
  deprecateMemoryConcept: vi.fn(),
  deleteMemoryConcept: vi.fn(),
}));
vi.mock("../api/memory.js", () => memoryApi);

const adminApi = vi.hoisted(() => ({ listAgents: vi.fn() }));
vi.mock("../api/admin.js", () => adminApi);

vi.mock("../auth/oidc.js", () => ({
  logout: vi.fn().mockResolvedValue(undefined),
  login: vi.fn(),
  getUser: vi.fn().mockResolvedValue(null),
  getAccessToken: vi.fn().mockResolvedValue(null),
  handleCallback: vi.fn(),
}));

const stableConcept = {
  id: "deploy-runbook",
  scope: "user",
  type: "runbook",
  title: "Deploy runbook",
  description: "How we ship",
  tags: ["deploy"],
  status: "stable",
  trustTier: "unverified",
  stale: false,
};
const deprecatedConcept = {
  ...stableConcept,
  id: "old-runbook",
  title: "Old runbook",
  status: "deprecated",
  stale: true,
};

function resetApi({ groups = [], concepts = [stableConcept], agents = [] } = {}) {
  memoryApi.listMemoryGroups.mockResolvedValue({ groups });
  memoryApi.listMemoryConcepts.mockResolvedValue({ concepts });
  memoryApi.getMemoryConcept.mockResolvedValue({ meta: stableConcept, body: "# body text" });
  memoryApi.verifyMemoryConcept.mockResolvedValue({ meta: { ...stableConcept, trustTier: "human-reviewed" } });
  memoryApi.deprecateMemoryConcept.mockResolvedValue({ meta: { ...stableConcept, status: "deprecated" } });
  memoryApi.deleteMemoryConcept.mockResolvedValue({ deleted: true });
  adminApi.listAgents.mockResolvedValue({ agents });
}

async function mountPane() {
  const w = mount(MemorySettings);
  await flushPromises();
  return w;
}

describe("MemorySettings.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    vi.clearAllMocks();
    resetApi();
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("lists the personal scope's concepts on mount", async () => {
    const w = await mountPane();

    expect(memoryApi.listMemoryConcepts).toHaveBeenCalledWith({ scope: "user" });
    const rows = w.findAll("[data-testid='memory-concept-row']");
    expect(rows.length).toBe(1);
    expect(w.text()).toContain("Deploy runbook");
    expect(w.text()).toContain("runbook");
    expect(w.text()).toContain("unverified");
  });

  it("dims a deprecated concept and badges a stale one", async () => {
    resetApi({ concepts: [deprecatedConcept] });
    const w = await mountPane();

    expect(w.find("[data-testid='memory-concept-row'].deprecated").exists()).toBe(true);
    expect(w.text()).toContain("stale");
  });

  it("opens a concept detail with its body", async () => {
    const w = await mountPane();
    await w.find("[data-testid='memory-concept-row']").trigger("click");
    await flushPromises();

    expect(memoryApi.getMemoryConcept).toHaveBeenCalledWith({ scope: "user", id: "deploy-runbook" });
    expect(w.find("[data-testid='memory-detail-body']").text()).toContain("# body text");
  });

  it("Verify calls the api with the scope triple and refreshes the list", async () => {
    const w = await mountPane();
    await w.find("[data-testid='memory-concept-row']").trigger("click");
    await flushPromises();
    memoryApi.listMemoryConcepts.mockClear();

    await w.find("[data-testid='memory-verify-btn']").trigger("click");
    await flushPromises();

    expect(memoryApi.verifyMemoryConcept).toHaveBeenCalledWith({ scope: "user", id: "deploy-runbook" });
    expect(memoryApi.listMemoryConcepts).toHaveBeenCalled();
  });

  it("Deprecate calls the api with the scope triple and refreshes the list", async () => {
    const w = await mountPane();
    await w.find("[data-testid='memory-concept-row']").trigger("click");
    await flushPromises();
    memoryApi.listMemoryConcepts.mockClear();

    await w.find("[data-testid='memory-deprecate-btn']").trigger("click");
    await flushPromises();

    expect(memoryApi.deprecateMemoryConcept).toHaveBeenCalledWith({ scope: "user", id: "deploy-runbook" });
    expect(memoryApi.listMemoryConcepts).toHaveBeenCalled();
    // Deprecation keeps the detail open (unlike delete) so the new status shows.
    expect(w.find("[data-testid='memory-detail']").exists()).toBe(true);
  });

  it("Delete asks for confirmation and drops the detail pane", async () => {
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(true);
    const w = await mountPane();
    await w.find("[data-testid='memory-concept-row']").trigger("click");
    await flushPromises();

    await w.find("[data-testid='memory-delete-btn']").trigger("click");
    await flushPromises();

    expect(confirmSpy).toHaveBeenCalled();
    expect(memoryApi.deleteMemoryConcept).toHaveBeenCalledWith({ scope: "user", id: "deploy-runbook" });
    expect(w.find("[data-testid='memory-detail']").exists()).toBe(false);
  });

  it("Delete is a no-op when confirmation is declined", async () => {
    vi.spyOn(window, "confirm").mockReturnValue(false);
    const w = await mountPane();
    await w.find("[data-testid='memory-concept-row']").trigger("click");
    await flushPromises();

    await w.find("[data-testid='memory-delete-btn']").trigger("click");
    await flushPromises();

    expect(memoryApi.deleteMemoryConcept).not.toHaveBeenCalled();
  });

  it("hides the Groups tab when the caller reaches no groups", async () => {
    const w = await mountPane();
    expect(w.find("[data-testid='memory-scope-group']").exists()).toBe(false);
  });

  it("shows the Groups tab with a manager badge and switches scope", async () => {
    resetApi({ groups: [{ groupId: "security-team", member: true, manager: true }] });
    const w = await mountPane();

    const tab = w.find("[data-testid='memory-scope-group']");
    expect(tab.exists()).toBe(true);
    await tab.trigger("click");
    await flushPromises();

    expect(memoryApi.listMemoryConcepts).toHaveBeenLastCalledWith({ scope: "group", groupId: "security-team" });
    expect(w.text()).toContain("manager");
  });

  it("offers no manage actions to a plain group member", async () => {
    resetApi({ groups: [{ groupId: "security-team", member: true, manager: false }] });
    const w = await mountPane();
    await w.find("[data-testid='memory-scope-group']").trigger("click");
    await flushPromises();
    await w.find("[data-testid='memory-concept-row']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='memory-verify-btn']").exists()).toBe(false);
    expect(w.find("[data-testid='memory-manage-hint']").exists()).toBe(true);
  });

  it("hides the Agents tab for a non-admin and never probes for agents", async () => {
    const w = await mountPane();
    expect(w.find("[data-testid='memory-scope-agent']").exists()).toBe(false);
    expect(adminApi.listAgents).not.toHaveBeenCalled();
  });

  it("shows the Agents tab for an admin", async () => {
    resetApi({ agents: [{ id: "agent-1", name: "Scribe" }] });
    const pinia = createPinia();
    setActivePinia(pinia);
    useUserStore().isAdmin = true;

    const w = await mountPane();
    const tab = w.find("[data-testid='memory-scope-agent']");
    expect(tab.exists()).toBe(true);
    await tab.trigger("click");
    await flushPromises();

    expect(memoryApi.listMemoryConcepts).toHaveBeenLastCalledWith({ scope: "agent", agentId: "agent-1" });
  });

  it("surfaces a load failure as an ErrorBanner, not a thrown render", async () => {
    resetApi();
    memoryApi.listMemoryConcepts.mockRejectedValue(new Error("broker down"));
    const w = await mountPane();

    expect(w.find("[data-testid='error-banner']").text()).toContain("broker down");
  });

  it("treats a 403 list as a permission hint rather than an error", async () => {
    resetApi();
    memoryApi.listMemoryConcepts.mockResolvedValue({ forbidden: true, error: "denied" });
    const w = await mountPane();

    expect(w.find("[data-testid='error-banner']").exists()).toBe(false);
    expect(w.find("[data-testid='memory-forbidden']").exists()).toBe(true);
  });
});

describe("UserSettingsModal.vue — Memory category", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    vi.clearAllMocks();
    resetApi();
  });

  it("renders four categories including Memory and hosts the pane", async () => {
    const w = mount(UserSettingsModal, { props: { open: true }, attachTo: document.body });
    await flushPromises();

    const labels = Array.from(document.querySelectorAll("[data-testid^='settings-cat-']"))
      .map((b) => b.textContent.trim());
    expect(labels).toEqual(["Account", "Appearance", "Chat", "Memory"]);

    document.querySelector("[data-testid='settings-cat-memory']")
      .dispatchEvent(new MouseEvent("click", { bubbles: true }));
    await flushPromises();

    expect(document.querySelector("[data-testid='memory-scope-user']")).not.toBeNull();
    w.unmount();
  });
});
