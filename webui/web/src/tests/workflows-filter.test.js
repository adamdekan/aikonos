// F63 (CP3): Workflows.vue name filter — one input over both the shared and
// private lists, case-insensitive substring against name only (WorkflowSummary
// carries no description field). Empty filter must render identically to today
// (parity pin).
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";

vi.mock("../api/workflows.js", () => ({
  listWorkflows: vi.fn(),
}));

import Workflows from "../views/Workflows.vue";
import * as wfApi from "../api/workflows.js";

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/workflows", component: Workflows },
      { path: "/", component: { template: "<div/>" } },
    ],
  });
}

const WF_A = {
  lineageId: "a", name: "daily-report",
  version: 1, visibilityKind: "private", status: "active",
  accessState: "runnable", missingRequirements: [], isOwner: true,
};
const WF_B = {
  lineageId: "b", name: "weekly-scrape",
  version: 1, visibilityKind: "private", status: "active",
  accessState: "runnable", missingRequirements: [], isOwner: true,
};
const WF_SHARED = {
  lineageId: "c", name: "team-digest",
  version: 1, visibilityKind: "shared", status: "active",
  accessState: "runnable", missingRequirements: [], isOwner: false,
};

describe("Workflows.vue — name filter", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    useUserStore().setFromProfile({ email: "alice@example.com", sub: "alice@example.com" });
    vi.clearAllMocks();
  });

  afterEach(() => vi.restoreAllMocks());

  it("empty filter renders identically to today (parity)", async () => {
    wfApi.listWorkflows.mockResolvedValue({ workflows: [WF_A, WF_B, WF_SHARED] });
    const router = makeRouter();
    await router.push("/workflows");
    const w = mount(Workflows, { global: { plugins: [router] } });
    await flushPromises();

    expect(w.findAll("[data-testid='workflow-card']").length).toBe(3);
  });

  it("filters by name case-insensitively across shared and private", async () => {
    wfApi.listWorkflows.mockResolvedValue({ workflows: [WF_A, WF_B, WF_SHARED] });
    const router = makeRouter();
    await router.push("/workflows");
    const w = mount(Workflows, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='workflow-filter-input']").setValue("DAILY");
    expect(w.text()).toContain("daily-report");
    expect(w.text()).not.toContain("weekly-scrape");
    expect(w.text()).not.toContain("team-digest");
  });
});
