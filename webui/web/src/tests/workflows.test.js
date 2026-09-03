// View tests for Workflows.vue.
// api/workflows.js is fully mocked so no server is needed.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";

vi.mock("../api/workflows.js", () => ({
  listWorkflows: vi.fn(),
}));

vi.mock("../api/agents.js", () => ({
  listMyAgents: vi.fn().mockResolvedValue({ agents: [] }),
}));

import Workflows from "../views/Workflows.vue";
import * as wfApi from "../api/workflows.js";
import * as agentsApi from "../api/agents.js";

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/workflows", component: Workflows },
      { path: "/",          component: { template: "<div/>" } },
    ],
  });
}

// A shared workflow: public, not owned by the viewer.
const SHARED_WF = {
  lineageId: "l1",
  name: "daily-report",
  version: 2,
  visibilityKind: "shared",
  status: "active",
  accessState: "runnable",
  missingRequirements: [],
  isOwner: false,
};

// A private workflow: owned by the viewer.
const PRIVATE_WF = {
  lineageId: "l2",
  name: "my-scraper",
  version: 1,
  visibilityKind: "private",
  status: "active",
  accessState: "runnable",
  missingRequirements: [],
  isOwner: true,
};

// A greyed-out shared workflow: viewer lacks required skills.
const GREYED_WF = {
  lineageId: "l3",
  name: "restricted-task",
  version: 1,
  visibilityKind: "shared",
  status: "active",
  accessState: "greyed_out",
  missingRequirements: ["skill:gdrive.write"],
  isOwner: false,
};

describe("Workflows.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    useUserStore().setFromProfile({ email: "alice@example.com", sub: "alice@example.com" });
    vi.clearAllMocks();
  });

  afterEach(() => vi.restoreAllMocks());

  it("renders Shared and Private sections from listWorkflows response", async () => {
    wfApi.listWorkflows.mockResolvedValue({ workflows: [SHARED_WF, PRIVATE_WF] });
    const router = makeRouter();
    await router.push("/workflows");
    const w = mount(Workflows, { global: { plugins: [router] } });
    await flushPromises();

    const cards = w.findAll("[data-testid='workflow-card']");
    expect(cards.length).toBe(2);
    expect(w.text()).toContain("Shared");
    expect(w.text()).toContain("Private");
    expect(w.text()).toContain("daily-report");
    expect(w.text()).toContain("my-scraper");
  });

  it("greyed_out card renders the missing requirement label", async () => {
    wfApi.listWorkflows.mockResolvedValue({ workflows: [GREYED_WF] });
    const router = makeRouter();
    await router.push("/workflows");
    const w = mount(Workflows, { global: { plugins: [router] } });
    await flushPromises();

    const card = w.find("[data-testid='workflow-card']");
    expect(card.classes()).toContain("workflow-card--greyed");

    const req = w.find("[data-testid='missing-req']");
    expect(req.exists()).toBe(true);
    expect(req.text()).toContain("skill:gdrive.write");
  });

  it("greyed_out Run button is disabled", async () => {
    wfApi.listWorkflows.mockResolvedValue({ workflows: [GREYED_WF] });
    const router = makeRouter();
    await router.push("/workflows");
    const w = mount(Workflows, { global: { plugins: [router] } });
    await flushPromises();

    const runBtn = w.findAll("button").find((b) => b.text() === "Run");
    expect(runBtn.element.disabled).toBe(true);
  });

  it("runnable Run button is not disabled", async () => {
    wfApi.listWorkflows.mockResolvedValue({ workflows: [PRIVATE_WF] });
    const router = makeRouter();
    await router.push("/workflows");
    const w = mount(Workflows, { global: { plugins: [router] } });
    await flushPromises();

    const runBtn = w.findAll("button").find((b) => b.text() === "Run");
    expect(runBtn.element.disabled).toBe(false);
  });

  it("shows empty state when no workflows", async () => {
    wfApi.listWorkflows.mockResolvedValue({ workflows: [] });
    const router = makeRouter();
    await router.push("/workflows");
    const w = mount(Workflows, { global: { plugins: [router] } });
    await flushPromises();

    expect(w.find("[data-testid='empty-state']").exists()).toBe(true);
    expect(w.findAll("[data-testid='workflow-card']").length).toBe(0);
  });

  it("shows error banner on API error", async () => {
    wfApi.listWorkflows.mockRejectedValue(new Error("server down"));
    const router = makeRouter();
    await router.push("/workflows");
    const w = mount(Workflows, { global: { plugins: [router] } });
    await flushPromises();

    expect(w.find("[data-testid='error-banner']").exists()).toBe(true);
    expect(w.text()).toContain("server down");
  });

  // ── F9: agent-bound workflows ──────────────────────────────────────────────
  const BOUND_WF = {
    ...PRIVATE_WF,
    lineageId: "lb1",
    name: "bound-flow",
    boundAgentId: "abcdef1234567890",
    boundAgentOk: true,
  };

  it("shows bound-agent badge with the agent name when the store resolves it", async () => {
    agentsApi.listMyAgents.mockResolvedValue({ agents: [{ id: "abcdef1234567890", name: "Reporter" }] });
    wfApi.listWorkflows.mockResolvedValue({ workflows: [BOUND_WF] });
    const router = makeRouter();
    await router.push("/workflows");
    const w = mount(Workflows, { global: { plugins: [router] } });
    await flushPromises();

    const badge = w.find("[data-testid='bound-agent']");
    expect(badge.exists()).toBe(true);
    expect(badge.text()).toContain("agent: Reporter");
  });

  it("shows the short-id fallback in the bound-agent badge when the agent is unknown", async () => {
    agentsApi.listMyAgents.mockResolvedValue({ agents: [] });
    wfApi.listWorkflows.mockResolvedValue({ workflows: [BOUND_WF] });
    const router = makeRouter();
    await router.push("/workflows");
    const w = mount(Workflows, { global: { plugins: [router] } });
    await flushPromises();

    const badge = w.find("[data-testid='bound-agent']");
    expect(badge.exists()).toBe(true);
    expect(badge.text()).toContain("agent: abcdef12");
  });

  it("renders no bound-agent badge when boundAgentId is empty", async () => {
    // Personal workflow: proto pass-through sends boundAgentId as "" (not undefined).
    wfApi.listWorkflows.mockResolvedValue({ workflows: [{ ...PRIVATE_WF, boundAgentId: "", boundAgentOk: false }] });
    const router = makeRouter();
    await router.push("/workflows");
    const w = mount(Workflows, { global: { plugins: [router] } });
    await flushPromises();

    expect(w.find("[data-testid='bound-agent']").exists()).toBe(false);
    // Empty-string boundAgentId must NOT disable Run despite boundAgentOk:false.
    const runBtn = w.findAll("button").find((b) => b.text() === "Run");
    expect(runBtn.element.disabled).toBe(false);
  });

  it("disables Run and shows the agent-denied hint when the viewer lacks agent access", async () => {
    wfApi.listWorkflows.mockResolvedValue({ workflows: [{ ...BOUND_WF, boundAgentOk: false }] });
    const router = makeRouter();
    await router.push("/workflows");
    const w = mount(Workflows, { global: { plugins: [router] } });
    await flushPromises();

    const runBtn = w.findAll("button").find((b) => b.text() === "Run");
    expect(runBtn.element.disabled).toBe(true);
    expect(w.find("[data-testid='agent-denied']").exists()).toBe(true);
  });

  it("enables Run when the viewer has agent access and accessState is normal", async () => {
    wfApi.listWorkflows.mockResolvedValue({ workflows: [{ ...BOUND_WF, boundAgentOk: true }] });
    const router = makeRouter();
    await router.push("/workflows");
    const w = mount(Workflows, { global: { plugins: [router] } });
    await flushPromises();

    const runBtn = w.findAll("button").find((b) => b.text() === "Run");
    expect(runBtn.element.disabled).toBe(false);
    expect(w.find("[data-testid='agent-denied']").exists()).toBe(false);
  });

  it("owned shared workflow appears in Private section, not Shared", async () => {
    // An owned workflow that was published is isOwner:true — goes to Private, not Shared.
    const ownedShared = { ...SHARED_WF, lineageId: "l4", isOwner: true };
    wfApi.listWorkflows.mockResolvedValue({ workflows: [ownedShared] });
    const router = makeRouter();
    await router.push("/workflows");
    const w = mount(Workflows, { global: { plugins: [router] } });
    await flushPromises();

    // Should appear in Private (isOwner=true), Shared section should be absent.
    expect(w.text()).toContain("Private");
    expect(w.text()).not.toContain("Shared");
  });

  // ── Pagination (Task 1) ──────────────────────────────────────────────────────

  it("requests the first page with a page-size limit", async () => {
    wfApi.listWorkflows.mockResolvedValue({ workflows: [PRIVATE_WF] });
    const router = makeRouter();
    await router.push("/workflows");
    const w = mount(Workflows, { global: { plugins: [router] } });
    await flushPromises();

    expect(wfApi.listWorkflows).toHaveBeenCalledWith({ limit: 50 });
    expect(w.find("[data-testid='load-more']").exists()).toBe(false);
  });

  it("shows Load more when nextCursor is set and appends the next page", async () => {
    wfApi.listWorkflows.mockResolvedValueOnce({ workflows: [PRIVATE_WF], nextCursor: "cur-1" });
    const router = makeRouter();
    await router.push("/workflows");
    const w = mount(Workflows, { global: { plugins: [router] } });
    await flushPromises();

    const more = w.find("[data-testid='load-more']");
    expect(more.exists()).toBe(true);

    wfApi.listWorkflows.mockResolvedValueOnce({
      workflows: [{ ...PRIVATE_WF, lineageId: "l9", name: "page-two-wf" }],
      nextCursor: "",
    });
    await more.trigger("click");
    await flushPromises();

    expect(wfApi.listWorkflows).toHaveBeenLastCalledWith({ limit: 50, cursor: "cur-1" });
    expect(w.findAll("[data-testid='workflow-card']").length).toBe(2);
    expect(w.text()).toContain("page-two-wf");
    // Cursor exhausted → Load more gone.
    expect(w.find("[data-testid='load-more']").exists()).toBe(false);
  });

  // ── sharedUnavailable banner (Task 3) ────────────────────────────────────────

  it("renders the sharedUnavailable banner and Retry re-loads", async () => {
    wfApi.listWorkflows.mockResolvedValue({ workflows: [PRIVATE_WF], sharedUnavailable: true });
    const router = makeRouter();
    await router.push("/workflows");
    const w = mount(Workflows, { global: { plugins: [router] } });
    await flushPromises();

    expect(w.find("[data-testid='shared-unavailable']").exists()).toBe(true);
    // Own workflows still render.
    expect(w.text()).toContain("my-scraper");

    const callsBefore = wfApi.listWorkflows.mock.calls.length;
    await w.find("[data-testid='shared-retry']").trigger("click");
    await flushPromises();
    expect(wfApi.listWorkflows.mock.calls.length).toBe(callsBefore + 1);
  });

  it("does not render the sharedUnavailable banner when the flag is absent", async () => {
    wfApi.listWorkflows.mockResolvedValue({ workflows: [PRIVATE_WF] });
    const router = makeRouter();
    await router.push("/workflows");
    const w = mount(Workflows, { global: { plugins: [router] } });
    await flushPromises();

    expect(w.find("[data-testid='shared-unavailable']").exists()).toBe(false);
  });

  // ── Loading state (Task 2) ───────────────────────────────────────────────────

  it("does not flash the empty state while the first load is in flight", async () => {
    let resolveList;
    wfApi.listWorkflows.mockReturnValue(new Promise((res) => { resolveList = res; }));
    const router = makeRouter();
    await router.push("/workflows");
    const w = mount(Workflows, { global: { plugins: [router] } });
    await flushPromises();

    // Mid-load: loading indicator shown, empty-state suppressed.
    expect(w.find("[data-testid='loading']").exists()).toBe(true);
    expect(w.find("[data-testid='empty-state']").exists()).toBe(false);

    resolveList({ workflows: [] });
    await flushPromises();
    // Settled empty: now the empty-state shows, loading gone.
    expect(w.find("[data-testid='loading']").exists()).toBe(false);
    expect(w.find("[data-testid='empty-state']").exists()).toBe(true);
  });
});
