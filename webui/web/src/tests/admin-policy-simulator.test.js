// Tests for views/admin/PolicySimulator.vue
// api/client.js is mocked so no server is needed.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";

vi.mock("../api/client.js", () => ({
  get:   vi.fn(),
  post:  vi.fn(),
  del:   vi.fn(),
  patch: vi.fn(),
}));

import PolicySimulator from "../views/admin/PolicySimulator.vue";
import * as client from "../api/client.js";

const MOCK_RESPONSE = {
  outcome: "DENY",
  gates: [
    {
      gate: "network",
      outcome: "DENY",
      rule_id: "rule-block-all",
      reason: "host blocked",
      detail: "evil.example.com",
    },
    {
      gate: "access_routing",
      outcome: "ALLOW",
      rule_id: "",
      reason: "policy allows",
      detail: "",
    },
    {
      gate: "capability",
      outcome: "INFO",
      rule_id: "",
      reason: "",
      detail: "web:read",
    },
  ],
};

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/admin/policy/simulate", component: PolicySimulator },
      { path: "/", component: { template: "<div/>" } },
    ],
  });
}

describe("PolicySimulator.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    useUserStore().setFromProfile({ email: "admin@example.com", sub: "admin@example.com" });
    vi.clearAllMocks();
  });
  afterEach(() => vi.restoreAllMocks());

  // ── Case 1: fill + submit calls post; outcome + gates render; DENY network gate shows host ──
  it("submitting calls client.post with entered body; outcome + gates render; DENY network gate shows host", async () => {
    client.post.mockResolvedValue(MOCK_RESPONSE);
    const router = makeRouter();
    await router.push("/admin/policy/simulate");
    const w = mount(PolicySimulator, { global: { plugins: [router] } });

    await w.find("[data-testid='subject-user-id']").setValue("alice@example.com");
    await w.find("[data-testid='tool-id']").setValue("web.fetch");
    await w.find("[data-testid='effect-class']").setValue("READ_ONLY");
    await w.find("[data-testid='host']").setValue("evil.example.com");
    await w.find("[data-testid='simulate-btn']").trigger("click");
    await flushPromises();

    expect(client.post).toHaveBeenCalledWith(
      "/admin/policy/simulate",
      expect.objectContaining({
        body: expect.objectContaining({
          subjectUserId: "alice@example.com",
          toolId: "web.fetch",
          effectClass: "READ_ONLY",
          host: "evil.example.com",
        }),
      }),
    );

    // Aggregate outcome badge
    const outcomeBadge = w.find("[data-testid='outcome-badge']");
    expect(outcomeBadge.exists()).toBe(true);
    expect(outcomeBadge.text()).toContain("DENY");

    // Gate rows render
    const gateRows = w.findAll("[data-testid='gate-row']");
    expect(gateRows.length).toBe(3);

    // DENY network gate shows its host in detail
    const networkGate = gateRows.find(g => g.text().includes("network"));
    expect(networkGate).toBeDefined();
    expect(networkGate.text()).toContain("DENY");
    expect(networkGate.text()).toContain("evil.example.com");
  });

  // ── Case 2: forbidden:true → admin empty-state, no throw ──────────────────
  it("forbidden:true response shows admin empty-state without throwing", async () => {
    client.post.mockResolvedValue({ forbidden: true });
    const router = makeRouter();
    await router.push("/admin/policy/simulate");
    const w = mount(PolicySimulator, { global: { plugins: [router] } });

    await w.find("[data-testid='subject-user-id']").setValue("alice@example.com");
    await w.find("[data-testid='tool-id']").setValue("web.fetch");
    await w.find("[data-testid='simulate-btn']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='forbidden']").exists()).toBe(true);
    expect(w.find("[data-testid='outcome-badge']").exists()).toBe(false);
    expect(w.findAll("[data-testid='gate-row']").length).toBe(0);
  });

  // ── Case 3: effect-class select value included in posted body ─────────────
  it("effect-class select value is sent in the posted body", async () => {
    client.post.mockResolvedValue({ outcome: "ALLOW", gates: [] });
    const router = makeRouter();
    await router.push("/admin/policy/simulate");
    const w = mount(PolicySimulator, { global: { plugins: [router] } });

    await w.find("[data-testid='subject-user-id']").setValue("bob@example.com");
    await w.find("[data-testid='tool-id']").setValue("doc.write");
    await w.find("[data-testid='effect-class']").setValue("WRITE_LOCAL");
    await w.find("[data-testid='simulate-btn']").trigger("click");
    await flushPromises();

    expect(client.post).toHaveBeenCalledWith(
      "/admin/policy/simulate",
      expect.objectContaining({
        body: expect.objectContaining({ effectClass: "WRITE_LOCAL" }),
      }),
    );
  });
});
