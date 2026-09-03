// Tests for views/admin/Runs.vue.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";

vi.mock("../api/admin.js", () => ({
  listAssignments:   vi.fn(),
  assignRole:        vi.fn(),
  revokeRole:        vi.fn(),
  listNetworkRules:  vi.fn(),
  addNetworkRule:    vi.fn(),
  deleteNetworkRule: vi.fn(),
  listAdminRuns:     vi.fn(),
}));

import Runs from "../views/admin/Runs.vue";
import * as adminApi from "../api/admin.js";

const SAMPLE_RUN = {
  id: "run-1",
  owner: "alice@example.com",
  prompt: "daily report",
  kind: "CRON",
  cronExpr: "0 9 * * *",
  nextFireAt: "2024-01-02T09:00:00Z",
  approvedTools: [],
  workflowLineageId: "",
  workflowDisplayName: "",
  state: "ACTIVE",
  lastFireAt: null,
  lastStatus: "",
  lastSummary: "",
  runCount: 3,
  createdBy: "alice@example.com",
  createdAt: "2024-01-01T00:00:00Z",
};

// Stored workflow-mode rows always have empty prompt.
const WORKFLOW_RUN = {
  ...SAMPLE_RUN,
  id: "run-2",
  prompt: "",
  workflowLineageId: "wf-lineage-1",
  workflowDisplayName: "Weekly digest",
};

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/admin/runs", component: Runs },
      { path: "/", component: { template: "<div/>" } },
    ],
  });
}

describe("Runs.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    useUserStore().setFromProfile({ email: "admin@example.com", sub: "admin@example.com" });
    vi.clearAllMocks();
  });
  afterEach(() => vi.restoreAllMocks());

  it("renders scheduled runs list from API response", async () => {
    adminApi.listAdminRuns.mockResolvedValue({ schedules: [SAMPLE_RUN], fgaEnabled: true });
    const router = makeRouter();
    await router.push("/admin/runs");
    const w = mount(Runs, { global: { plugins: [router] } });
    await flushPromises();
    expect(w.findAll("[data-testid='run-row']").length).toBe(1);
    expect(w.text()).toContain("alice@example.com");
    // ui-set: DataTable renders the table
    expect(w.find(".dt-table").exists()).toBe(true);
  });

  it("listAdminRuns is called with owner filter when set", async () => {
    adminApi.listAdminRuns.mockResolvedValue({ schedules: [], fgaEnabled: true });
    const router = makeRouter();
    await router.push("/admin/runs");
    const w = mount(Runs, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='owner-filter']").setValue("alice@example.com");
    await w.find("[data-testid='filter-btn']").trigger("click");
    await flushPromises();

    expect(adminApi.listAdminRuns).toHaveBeenCalledWith("alice@example.com");
  });

  it("renders a workflow-mode row with a Workflow badge and the display name instead of prompt text", async () => {
    adminApi.listAdminRuns.mockResolvedValue({ schedules: [WORKFLOW_RUN], fgaEnabled: true });
    const router = makeRouter();
    await router.push("/admin/runs");
    const w = mount(Runs, { global: { plugins: [router] } });
    await flushPromises();

    const row = w.find("[data-testid='run-row']");
    expect(row.find("[data-testid='workflow-badge']").exists()).toBe(true);
    expect(row.text()).toContain("Weekly digest");
  });

  it("falls back to '(deleted workflow)' when a workflow row's display name is empty", async () => {
    const deletedWorkflowRun = { ...WORKFLOW_RUN, workflowDisplayName: "" };
    adminApi.listAdminRuns.mockResolvedValue({ schedules: [deletedWorkflowRun], fgaEnabled: true });
    const router = makeRouter();
    await router.push("/admin/runs");
    const w = mount(Runs, { global: { plugins: [router] } });
    await flushPromises();

    expect(w.find("[data-testid='run-row']").text()).toContain("(deleted workflow)");
  });

  it("renders not-an-admin empty-state on 403 (forbidden) without throwing", async () => {
    adminApi.listAdminRuns.mockResolvedValue({ forbidden: true });
    const router = makeRouter();
    await router.push("/admin/runs");
    const w = mount(Runs, { global: { plugins: [router] } });
    await flushPromises();
    expect(w.find("[data-testid='forbidden']").exists()).toBe(true);
    expect(w.find("[data-testid='run-row']").exists()).toBe(false);
  });
});
