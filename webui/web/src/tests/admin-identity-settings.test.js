// Tests for views/admin/IdentitySettings.vue (AG-02 / AG-17 switches).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";

vi.mock("../api/client.js", () => ({
  get: vi.fn(), post: vi.fn(), put: vi.fn(), del: vi.fn(), patch: vi.fn(),
}));

import IdentitySettings from "../views/admin/IdentitySettings.vue";
import * as client from "../api/client.js";

const CONFIG = { entries: [
  { key: "identity.enforce_user_bound", value: "audit" },
  { key: "identity.obo", value: "enforce" },
  { key: "llm_model", value: "x/y" },
] };
const EVENTS = { events: [{
  event_id: "e1",
  event_type: "aikonos.broker.user_bound.would_deny",
  context: { user: "user:alice@example.com", agent: "agent:a1", connector: "c1", stage: "invoke" },
}] };

function mockGet() {
  client.get.mockImplementation((url) => Promise.resolve(url.startsWith("/admin/audit/query") ? EVENTS : CONFIG));
}

describe("IdentitySettings.vue", () => {
  beforeEach(() => vi.clearAllMocks());

  it("shows both switches from config and lists would-deny events", async () => {
    mockGet();
    const w = mount(IdentitySettings);
    await flushPromises();
    expect(w.find("[data-testid='identity-user-bound-toggle']").element.checked).toBe(false);
    expect(w.find("[data-testid='identity-obo-toggle']").element.checked).toBe(true);
    expect(client.get).toHaveBeenCalledWith(
      "/admin/audit/query?event_type=aikonos.broker.user_bound.would_deny&limit=50");
    const rows = w.findAll("[data-testid='would-deny-row']");
    expect(rows.length).toBe(1);
    expect(rows[0].text()).toContain("user:alice@example.com");
    expect(rows[0].text()).toContain("agent:a1");
    expect(rows[0].text()).toContain("c1");
  });

  it("toggling enforce saves identity.enforce_user_bound=enforce", async () => {
    mockGet();
    client.put.mockResolvedValue({});
    const w = mount(IdentitySettings);
    await flushPromises();
    await w.find("[data-testid='identity-user-bound-toggle']").setValue(true);
    await flushPromises();
    expect(client.put).toHaveBeenCalledWith("/admin/config", {
      body: { key: "identity.enforce_user_bound", value: "enforce" },
    });
  });

  it("shows the save error inline", async () => {
    mockGet();
    client.put.mockRejectedValue(new Error("nope"));
    const w = mount(IdentitySettings);
    await flushPromises();
    await w.find("[data-testid='identity-obo-toggle']").setValue(false);
    await flushPromises();
    expect(w.find("[data-testid='identity-obo-error']").text()).toContain("nope");
  });

  it("forbidden shows the admin empty-state", async () => {
    client.get.mockResolvedValue({ forbidden: true });
    const w = mount(IdentitySettings);
    await flushPromises();
    expect(w.find("[data-testid='forbidden']").exists()).toBe(true);
  });
});
