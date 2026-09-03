// Tests for views/admin/DecisionTrace.vue
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

import DecisionTrace from "../views/admin/DecisionTrace.vue";
import * as client from "../api/client.js";

const DENY_STEP = {
  seq: 1,
  tool_id: "web.fetch",
  effect_class: "READ_ONLY",
  decision: "DENY",
  justification: "policy denies external fetch",
  gates: [
    {
      gate: "access_routing",
      outcome: "DENY",
      rule_id: "rule-no-web",
      reason: "web fetch not allowed",
      detail: "opa: web_denied",
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

const ALLOW_STEP = {
  seq: 2,
  tool_id: "doc.read",
  effect_class: "READ_ONLY",
  decision: "ALLOW",
  justification: "approved",
  gates: [
    {
      gate: "access_routing",
      outcome: "ALLOW",
      rule_id: "",
      reason: "policy allows",
      detail: "",
    },
    {
      gate: "network",
      outcome: "ASK",
      rule_id: "",
      reason: "acl ask",
      detail: "example.com",
    },
    {
      gate: "capability",
      outcome: "INFO",
      rule_id: "",
      reason: "",
      detail: "doc:read",
    },
  ],
};

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/admin/decisions", component: DecisionTrace },
      { path: "/", component: { template: "<div/>" } },
    ],
  });
}

describe("DecisionTrace.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    useUserStore().setFromProfile({ email: "admin@example.com", sub: "admin@example.com" });
    vi.clearAllMocks();
  });
  afterEach(() => vi.restoreAllMocks());

  // ── Case 1: entering a task id + Explain calls client; steps + gates render ─
  it("Explain button calls client with entered task id; DENY step shows rule_id and reason", async () => {
    client.get.mockResolvedValue({
      taskId: "task-abc",
      steps: [DENY_STEP, ALLOW_STEP],
    });
    const router = makeRouter();
    await router.push("/admin/decisions");
    const w = mount(DecisionTrace, { global: { plugins: [router] } });

    await w.find("[data-testid='task-id-input']").setValue("task-abc");
    await w.find("[data-testid='explain-btn']").trigger("click");
    await flushPromises();

    expect(client.get).toHaveBeenCalledWith(
      expect.stringContaining("/admin/decisions/task-abc"),
    );

    // Steps render
    const steps = w.findAll("[data-testid='step-row']");
    expect(steps.length).toBe(2);
    // ui-set: EmptyState component is available (not rendered when steps present)
    expect(w.find("[data-testid='forbidden']").exists()).toBe(false);

    // DENY step content
    const denyText = steps[0].text();
    expect(denyText).toContain("web.fetch");
    expect(denyText).toContain("DENY");

    // DENY step has access_routing gate with rule_id + reason
    const gateRows = steps[0].findAll("[data-testid='gate-row']");
    expect(gateRows.length).toBeGreaterThan(0);
    const accessGateText = gateRows.find(g => g.text().includes("access_routing"))?.text() ?? "";
    expect(accessGateText).toContain("rule-no-web");
    expect(accessGateText).toContain("web fetch not allowed");
  });

  // ── Case 2: empty steps → "no decision trace" state ───────────────────────
  it("empty steps response shows no-trace state", async () => {
    client.get.mockResolvedValue({ taskId: "task-xyz", steps: [] });
    const router = makeRouter();
    await router.push("/admin/decisions");
    const w = mount(DecisionTrace, { global: { plugins: [router] } });

    await w.find("[data-testid='task-id-input']").setValue("task-xyz");
    await w.find("[data-testid='explain-btn']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='no-trace']").exists()).toBe(true);
    expect(w.findAll("[data-testid='step-row']").length).toBe(0);
  });

  // ── Case 3: forbidden → admin empty-state, no throw ───────────────────────
  it("forbidden:true response shows admin empty-state without throwing", async () => {
    client.get.mockResolvedValue({ forbidden: true });
    const router = makeRouter();
    await router.push("/admin/decisions");
    const w = mount(DecisionTrace, { global: { plugins: [router] } });

    await w.find("[data-testid='task-id-input']").setValue("task-forbidden");
    await w.find("[data-testid='explain-btn']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='forbidden']").exists()).toBe(true);
    expect(w.find("[data-testid='step-row']").exists()).toBe(false);
  });
});
