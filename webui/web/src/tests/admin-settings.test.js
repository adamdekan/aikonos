// Tests for views/admin/Settings.vue
// api/client.js is mocked so no server is needed.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";

vi.mock("../api/client.js", () => ({
  get:   vi.fn(),
  post:  vi.fn(),
  put:   vi.fn(),
  del:   vi.fn(),
  patch: vi.fn(),
}));

import Settings from "../views/admin/Settings.vue";
import * as client from "../api/client.js";

const ENTRIES = [
  {
    key: "approval_expiry_hours",
    value: "24",
    defaultValue: "24",
    kind: "INT",
    doc: "Hours a pending human-approval gate stays open before it expires",
  },
];

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/admin/settings", component: Settings },
      { path: "/", component: { template: "<div/>" } },
    ],
  });
}

describe("Settings.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    useUserStore().setFromProfile({ email: "admin@example.com", sub: "admin@example.com" });
    vi.clearAllMocks();
  });
  afterEach(() => vi.restoreAllMocks());

  // ── Case 1: mounts → calls get("/admin/config"); renders a row per entry ─
  it("mounts, calls get('/admin/config'), and renders one row per entry", async () => {
    client.get.mockResolvedValue({ entries: ENTRIES });

    const router = makeRouter();
    await router.push("/admin/settings");
    const w = mount(Settings, { global: { plugins: [router] } });
    await flushPromises();

    expect(client.get).toHaveBeenCalledWith("/admin/config");

    const rows = w.findAll("[data-testid='config-row']");
    expect(rows.length).toBe(1);

    const rowText = rows[0].text();
    expect(rowText).toContain("approval_expiry_hours");
    expect(rowText).toContain("Hours a pending human-approval gate stays open");

    // Input bound to current value
    const input = rows[0].find("[data-testid='config-value-input']");
    expect(input.element.value).toBe("24");
  });

  // ── Case 2: editing a value + Save calls put with the edited value ─────
  it("editing a value and clicking Save calls put('/admin/config') with the new value", async () => {
    client.get.mockResolvedValue({ entries: ENTRIES });
    client.put.mockResolvedValue({});

    const router = makeRouter();
    await router.push("/admin/settings");
    const w = mount(Settings, { global: { plugins: [router] } });
    await flushPromises();

    const row = w.find("[data-testid='config-row']");
    const input = row.find("[data-testid='config-value-input']");
    await input.setValue("48");

    const saveBtn = row.find("[data-testid='config-save-btn']");
    await saveBtn.trigger("click");
    await flushPromises();

    expect(client.put).toHaveBeenCalledWith("/admin/config", {
      body: { key: "approval_expiry_hours", value: "48" },
    });
  });

  // ── Case 3a: 400/validation error → inline error shown, no throw ───────
  it("a 400/validation response shows an inline error per row without throwing", async () => {
    client.get.mockResolvedValue({ entries: ENTRIES });
    client.put.mockRejectedValue(new Error("value must be between 1 and 720"));

    const router = makeRouter();
    await router.push("/admin/settings");
    const w = mount(Settings, { global: { plugins: [router] } });
    await flushPromises();

    const row = w.find("[data-testid='config-row']");
    await row.find("[data-testid='config-value-input']").setValue("0");
    await row.find("[data-testid='config-save-btn']").trigger("click");
    await flushPromises();

    const errorEl = row.find("[data-testid='config-row-error']");
    expect(errorEl.exists()).toBe(true);
    expect(errorEl.text()).toContain("value must be between 1 and 720");
  });

  // ── Case 3b: forbidden → admin empty-state, no throw ──────────────────
  it("forbidden:true response shows admin empty-state without throwing", async () => {
    client.get.mockResolvedValue({ forbidden: true });

    const router = makeRouter();
    await router.push("/admin/settings");
    const w = mount(Settings, { global: { plugins: [router] } });
    await flushPromises();

    expect(w.find("[data-testid='forbidden']").exists()).toBe(true);
    expect(w.findAll("[data-testid='config-row']").length).toBe(0);
  });
});
