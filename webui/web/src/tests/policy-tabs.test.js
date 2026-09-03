// Tests for views/admin/PolicyTabs.vue
// All child-view API deps mocked so no server needed.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";

// Audit.vue uses auditExport.js (no API); Alerts.vue uses admin.js;
// AuditHistory / DecisionTrace / PolicySimulator use client.js.
vi.mock("../api/admin.js", () => ({
  listAlerts: vi.fn().mockResolvedValue({ alerts: [] }),
}));

vi.mock("../api/client.js", () => ({
  get:   vi.fn().mockResolvedValue({}),
  post:  vi.fn().mockResolvedValue({}),
  del:   vi.fn().mockResolvedValue({}),
  patch: vi.fn().mockResolvedValue({}),
}));

import PolicyTabs from "../views/admin/PolicyTabs.vue";

function makeRouter(initialPath = "/admin/policy") {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/admin/policy", component: PolicyTabs },
      { path: "/", component: { template: "<div/>" } },
    ],
  });
}

async function mountAt(path = "/admin/policy") {
  const router = makeRouter(path);
  await router.push(path);
  await router.isReady();
  const w = mount(PolicyTabs, { global: { plugins: [router] } });
  await flushPromises();
  return { w, router };
}

describe("PolicyTabs.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    useUserStore().setFromProfile({ email: "admin@example.com", sub: "admin@example.com" });
    vi.clearAllMocks();
  });
  afterEach(() => vi.restoreAllMocks());

  it("renders all 5 tab buttons", async () => {
    const { w } = await mountAt("/admin/policy?tab=audit");
    const tabs = ["audit", "history", "decisions", "simulator", "alerts"];
    for (const key of tabs) {
      expect(w.find(`[data-testid="policy-tab-${key}"]`).exists()).toBe(true);
    }
  });

  it("?tab=simulator shows Policy Simulator h1 and simulator tab is active", async () => {
    const { w } = await mountAt("/admin/policy?tab=simulator");
    // Panel h1 must be the simulator's — not the audit stream's
    expect(w.find("h1").text()).toBe("Policy Simulator");
    const simBtn = w.find('[data-testid="policy-tab-simulator"]');
    expect(simBtn.classes()).toContain("active");
    const auditBtn = w.find('[data-testid="policy-tab-audit"]');
    expect(auditBtn.classes()).not.toContain("active");
  });

  it("no ?tab → defaults to Audit (audit tab active)", async () => {
    const { w } = await mountAt("/admin/policy");
    const auditBtn = w.find('[data-testid="policy-tab-audit"]');
    expect(auditBtn.classes()).toContain("active");
    expect(w.find("h1").text()).toBe("Audit Stream");
  });

  it("clicking decisions tab updates route.query.tab and shows Decision Trace panel", async () => {
    const { w, router } = await mountAt("/admin/policy?tab=audit");
    await w.find('[data-testid="policy-tab-decisions"]').trigger("click");
    await flushPromises();
    expect(router.currentRoute.value.query.tab).toBe("decisions");
    const decisionsBtn = w.find('[data-testid="policy-tab-decisions"]');
    expect(decisionsBtn.classes()).toContain("active");
    expect(w.find("h1").text()).toBe("Decision Trace");
  });
});
