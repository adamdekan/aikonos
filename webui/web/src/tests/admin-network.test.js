// Tests for views/admin/Network.vue.
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

import Network from "../views/admin/Network.vue";
import * as adminApi from "../api/admin.js";

const SAMPLE_RULE = {
  id: "r1",
  scopeKind: "TENANT",
  scopeValue: "",
  action: "DENY",
  hostPattern: "*",
  note: "block all",
  createdBy: "admin@example.com",
  createdAt: "2024-01-01T00:00:00Z",
};

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/admin/network", component: Network },
      { path: "/", component: { template: "<div/>" } },
    ],
  });
}

describe("Network.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    useUserStore().setFromProfile({ email: "admin@example.com", sub: "admin@example.com" });
    vi.clearAllMocks();
  });
  afterEach(() => vi.restoreAllMocks());

  it("renders rule list from API response", async () => {
    adminApi.listNetworkRules.mockResolvedValue({ rules: [SAMPLE_RULE] });
    const router = makeRouter();
    await router.push("/admin/network");
    const w = mount(Network, { global: { plugins: [router] } });
    await flushPromises();
    expect(w.findAll("[data-testid='rule-row']").length).toBe(1);
    expect(w.text()).toContain("*");
  });

  it("addNetworkRule is called with correct body on form submit", async () => {
    adminApi.listNetworkRules.mockResolvedValue({ rules: [] });
    adminApi.addNetworkRule.mockResolvedValue({ rule: SAMPLE_RULE });
    const router = makeRouter();
    await router.push("/admin/network");
    const w = mount(Network, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='rule-host']").setValue("*.evil.com");
    await w.find("[data-testid='rule-action']").setValue("DENY");
    await w.find("[data-testid='add-rule-btn']").trigger("click");
    await flushPromises();

    expect(adminApi.addNetworkRule).toHaveBeenCalledWith(
      expect.objectContaining({ hostPattern: "*.evil.com", action: "DENY" }),
    );
  });

  it("deleteNetworkRule is called with rule id on delete click", async () => {
    adminApi.listNetworkRules.mockResolvedValue({ rules: [SAMPLE_RULE] });
    adminApi.deleteNetworkRule.mockResolvedValue({ success: true });
    const router = makeRouter();
    await router.push("/admin/network");
    const w = mount(Network, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='delete-r1']").trigger("click");
    await flushPromises();
    expect(adminApi.deleteNetworkRule).toHaveBeenCalledWith("r1");
  });

  it("renders not-an-admin empty-state on 403 (forbidden) without throwing", async () => {
    adminApi.listNetworkRules.mockResolvedValue({ forbidden: true });
    const router = makeRouter();
    await router.push("/admin/network");
    const w = mount(Network, { global: { plugins: [router] } });
    await flushPromises();
    expect(w.find("[data-testid='forbidden']").exists()).toBe(true);
    expect(w.find("[data-testid='rule-row']").exists()).toBe(false);
  });

  // --- new: forbidden mutation handling ---

  it("submit with forbidden response shows error banner, no success toast, form NOT cleared", async () => {
    adminApi.listNetworkRules.mockResolvedValue({ rules: [] });
    adminApi.addNetworkRule.mockResolvedValue({ forbidden: true, error: "not an admin" });
    const router = makeRouter();
    await router.push("/admin/network");
    const w = mount(Network, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='rule-host']").setValue("bad.com");
    await w.find("[data-testid='add-rule-btn']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='error-banner']").exists()).toBe(true);
    expect(w.find("[data-testid='error-banner']").text()).toContain("not an admin");
    // form field must NOT have been cleared
    expect(w.find("[data-testid='rule-host']").element.value).toBe("bad.com");
    // no second call (load) after forbidden — addNetworkRule called once, listNetworkRules once
    expect(adminApi.listNetworkRules).toHaveBeenCalledTimes(1);
  });

  it("delete with forbidden response shows error banner", async () => {
    adminApi.listNetworkRules.mockResolvedValue({ rules: [SAMPLE_RULE] });
    adminApi.deleteNetworkRule.mockResolvedValue({ forbidden: true, error: "not an admin" });
    const router = makeRouter();
    await router.push("/admin/network");
    const w = mount(Network, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='delete-r1']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='error-banner']").exists()).toBe(true);
    expect(w.find("[data-testid='error-banner']").text()).toContain("not an admin");
    // load NOT called again after forbidden
    expect(adminApi.listNetworkRules).toHaveBeenCalledTimes(1);
  });

  it("submit with empty host shows validation error, addNetworkRule NOT called", async () => {
    adminApi.listNetworkRules.mockResolvedValue({ rules: [] });
    const router = makeRouter();
    await router.push("/admin/network");
    const w = mount(Network, { global: { plugins: [router] } });
    await flushPromises();

    // host field left empty
    await w.find("[data-testid='add-rule-btn']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='error-banner']").exists()).toBe(true);
    expect(w.find("[data-testid='error-banner']").text().toLowerCase()).toContain("host pattern");
    expect(adminApi.addNetworkRule).not.toHaveBeenCalled();
  });
});
