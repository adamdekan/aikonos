// Tests for views/admin/SpendCaps.vue.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";

vi.mock("../api/admin.js", () => ({
  listSpendCaps:   vi.fn(),
  setSpendCap:     vi.fn(),
  deleteSpendCap:  vi.fn(),
  getSpendSummary: vi.fn(),
  listMembers:     vi.fn(),
  listAgents:      vi.fn(),
}));

import SpendCaps from "../views/admin/SpendCaps.vue";
import * as adminApi from "../api/admin.js";
import { useToast } from "../components/ui/useToast.js";

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/admin/spend-caps", component: SpendCaps },
      { path: "/", component: { template: "<div/>" } },
    ],
  });
}

async function mountView() {
  const router = makeRouter();
  await router.push("/admin/spend-caps");
  const w = mount(SpendCaps, { global: { plugins: [router] }, attachTo: document.body });
  await flushPromises();
  return w;
}

function q(testid) {
  return document.querySelector(`[data-testid='${testid}']`);
}

function setField(testid, value, evt = "input") {
  const el = q(testid);
  el.value = value;
  el.dispatchEvent(new Event(evt));
}

const SUMMARY = {
  orgSpendMicros: 12_500_000,
  orgCapMicros:   50_000_000,
  users: [
    { userId: "alice@example.com", spendMicros: 4_000_000, capMicros: 10_000_000 },
    { userId: "bob@example.com",   spendMicros: 1_000_000, capMicros: 0 }, // spend, no cap
  ],
  agents: [
    { agentId: "agent-1", spendMicros: 0, capMicros: 5_000_000 }, // cap, zero spend
  ],
};

const CAPS = [
  { id: "cap-org",   scope: "org",   subjectId: "",                capMicros: 50_000_000 },
  { id: "cap-alice", scope: "user",  subjectId: "alice@example.com", capMicros: 10_000_000 },
  { id: "cap-agent1", scope: "agent", subjectId: "agent-1",         capMicros: 5_000_000 },
];

describe("SpendCaps.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    useUserStore().setFromProfile({ email: "admin@example.com", sub: "admin@example.com" });
    vi.clearAllMocks();
    const { toasts } = useToast();
    toasts.splice(0);
    adminApi.listMembers.mockResolvedValue({ members: [{ subject: "alice@example.com", name: "Alice", email: "alice@example.com" }] });
    adminApi.listAgents.mockResolvedValue({ agents: [{ id: "agent-1", name: "Agent One" }] });
  });
  afterEach(() => {
    document.body.innerHTML = "";
    vi.restoreAllMocks();
  });

  it("renders the forbidden empty state when not a tenant admin", async () => {
    adminApi.getSpendSummary.mockResolvedValue({ forbidden: true });
    adminApi.listSpendCaps.mockResolvedValue({ caps: [] });
    const w = await mountView();
    expect(w.find("[data-testid='forbidden']").exists()).toBe(true);
  });

  it("renders org spend/cap and utilization %", async () => {
    adminApi.getSpendSummary.mockResolvedValue(SUMMARY);
    adminApi.listSpendCaps.mockResolvedValue({ caps: CAPS });
    const w = await mountView();

    expect(w.text()).toContain("12.50");
    expect(w.text()).toContain("50.00");
    expect(q("org-pct").textContent).toContain("25%");
    // Deployments bill in different currencies — amounts render bare, never
    // claiming a currency the tenant may not use.
    expect(w.text()).not.toContain("$");
  });

  it("renders per-user and per-agent rows including no-cap and zero-spend edge cases", async () => {
    adminApi.getSpendSummary.mockResolvedValue(SUMMARY);
    adminApi.listSpendCaps.mockResolvedValue({ caps: CAPS });
    const w = await mountView();

    const userRows = w.findAll("[data-testid='user-row']");
    expect(userRows.length).toBe(2);
    expect(userRows[0].text()).toContain("alice@example.com");
    expect(userRows[0].text()).toContain("40%"); // 4/10

    // bob has spend but no cap — % column blank, delete button absent (no cap row)
    expect(userRows[1].text()).toContain("bob@example.com");
    expect(userRows[1].text()).toContain("—");
    expect(userRows[1].find("[data-testid='delete-user-bob@example.com']").exists()).toBe(false);

    const agentRows = w.findAll("[data-testid='agent-row']");
    expect(agentRows.length).toBe(1);
    expect(agentRows[0].text()).toContain("agent-1");
    expect(agentRows[0].text()).toContain("0%"); // 0/5
  });

  it("sets the org cap converting major units to micro-units", async () => {
    adminApi.getSpendSummary.mockResolvedValue({ orgSpendMicros: 0, orgCapMicros: 0, users: [], agents: [] });
    adminApi.listSpendCaps.mockResolvedValue({ caps: [] });
    adminApi.setSpendCap.mockResolvedValue({ id: "cap-org" });
    const w = await mountView();

    setField("org-cap-input", "25.50");
    await q("org-cap-save-btn").click();
    await flushPromises();

    expect(adminApi.setSpendCap).toHaveBeenCalledWith({ scope: "org", subjectId: "", capMicros: 25_500_000 });
  });

  it("sets a per-user cap using the picked subject", async () => {
    adminApi.getSpendSummary.mockResolvedValue({ orgSpendMicros: 0, orgCapMicros: 0, users: [], agents: [] });
    adminApi.listSpendCaps.mockResolvedValue({ caps: [] });
    adminApi.setSpendCap.mockResolvedValue({ id: "cap-alice" });
    const w = await mountView();

    setField("user-pick", "alice@example.com");
    setField("user-cap-input", "10");
    await q("user-cap-save-btn").click();
    await flushPromises();

    expect(adminApi.setSpendCap).toHaveBeenCalledWith({ scope: "user", subjectId: "alice@example.com", capMicros: 10_000_000 });
  });

  it("sets a per-agent cap using the picked agent id", async () => {
    adminApi.getSpendSummary.mockResolvedValue({ orgSpendMicros: 0, orgCapMicros: 0, users: [], agents: [] });
    adminApi.listSpendCaps.mockResolvedValue({ caps: [] });
    adminApi.setSpendCap.mockResolvedValue({ id: "cap-agent1" });
    const w = await mountView();

    setField("agent-pick", "agent-1");
    setField("agent-cap-input", "5");
    await q("agent-cap-save-btn").click();
    await flushPromises();

    expect(adminApi.setSpendCap).toHaveBeenCalledWith({ scope: "agent", subjectId: "agent-1", capMicros: 5_000_000 });
  });

  it("deletes a user cap via the row's delete button", async () => {
    adminApi.getSpendSummary.mockResolvedValue(SUMMARY);
    adminApi.listSpendCaps.mockResolvedValue({ caps: CAPS });
    adminApi.deleteSpendCap.mockResolvedValue({});
    const w = await mountView();

    await w.find("[data-testid='delete-user-alice@example.com']").trigger("click");
    await flushPromises();

    expect(adminApi.deleteSpendCap).toHaveBeenCalledWith("cap-alice");
  });

  it("shows an error toast and does not clear the cap when clearing the org cap is forbidden", async () => {
    adminApi.getSpendSummary.mockResolvedValue(SUMMARY);
    adminApi.listSpendCaps.mockResolvedValue({ caps: CAPS });
    adminApi.deleteSpendCap.mockResolvedValue({ forbidden: true, error: "You are not a tenant admin." });
    const w = await mountView();
    const { toasts } = useToast();

    await q("org-cap-clear-btn").click();
    await flushPromises();

    expect(adminApi.deleteSpendCap).toHaveBeenCalledWith("cap-org");
    expect(toasts.some((t) => t.type === "error")).toBe(true);
    expect(toasts.some((t) => t.type === "ok")).toBe(false);
    // listSpendCaps only called once for the initial load — no re-fetch after a forbidden clear.
    expect(adminApi.listSpendCaps).toHaveBeenCalledTimes(1);
  });

  it("clears the org cap and shows a success toast", async () => {
    adminApi.getSpendSummary.mockResolvedValue(SUMMARY);
    adminApi.listSpendCaps.mockResolvedValue({ caps: CAPS });
    adminApi.deleteSpendCap.mockResolvedValue({});
    const w = await mountView();
    const { toasts } = useToast();

    await q("org-cap-clear-btn").click();
    await flushPromises();

    expect(adminApi.deleteSpendCap).toHaveBeenCalledWith("cap-org");
    expect(toasts.some((t) => t.type === "ok")).toBe(true);
  });
});
