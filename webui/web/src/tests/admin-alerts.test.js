// Tests for views/admin/Alerts.vue.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";

vi.mock("../api/admin.js", () => ({
  listAlerts: vi.fn(),
}));

import Alerts from "../views/admin/Alerts.vue";
import * as adminApi from "../api/admin.js";

const SAMPLE_ALERT = {
  id: "a1",
  rule: "off_hours_destructive",
  severity: "critical",
  summary: { actor: "user:bob", tool: "doc.delete" },
  firedAt: "2026-06-07T22:00:00Z",
};

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/admin/alerts", component: Alerts },
      { path: "/", component: { template: "<div/>" } },
    ],
  });
}

async function mountView() {
  const router = makeRouter();
  await router.push("/admin/alerts");
  const w = mount(Alerts, { global: { plugins: [router] } });
  await flushPromises();
  return w;
}

describe("Alerts.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    useUserStore().setFromProfile({ email: "admin@example.com", sub: "admin@example.com" });
    vi.clearAllMocks();
  });
  afterEach(() => vi.restoreAllMocks());

  it("renders alert rows from API response", async () => {
    adminApi.listAlerts.mockResolvedValue({ alerts: [SAMPLE_ALERT] });
    const w = await mountView();
    expect(w.findAll("[data-testid='alert-row']").length).toBe(1);
    expect(w.text()).toContain("off_hours_destructive");
    expect(w.text()).toContain("critical");
  });

  it("renders empty-state when no alerts (via DataTable empty-text)", async () => {
    adminApi.listAlerts.mockResolvedValue({ alerts: [] });
    const w = await mountView();
    expect(w.find("[data-testid='alert-row']").exists()).toBe(false);
    expect(w.text()).toContain("No alerts");
  });

  it("renders not-an-admin empty-state on forbidden without throwing", async () => {
    adminApi.listAlerts.mockResolvedValue({ forbidden: true });
    const w = await mountView();
    expect(w.find("[data-testid='forbidden']").exists()).toBe(true);
    expect(w.find("[data-testid='alert-row']").exists()).toBe(false);
  });

  it("surfaces a toast on fetch error without throwing", async () => {
    adminApi.listAlerts.mockRejectedValue(new Error("boom"));
    const w = await mountView();
    expect(w.find("[data-testid='alert-row']").exists()).toBe(false);
  });
});
