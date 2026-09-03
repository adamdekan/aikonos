// CP21 tests: "Improve in chat" replaces the raw-JSON Edit path.
//
// WHY: clicking Improve on an owned workflow card must seed the prompt store's
// prefill (draft mode — user reviews before sending) with the workflow identity +
// definition summary + an instruction to propose via workflow_propose, then
// navigate to /chat. The raw EditWorkflowModal must be absent from the render.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";
import { usePromptStore } from "../store/prompt.js";

vi.mock("../api/workflows.js", () => ({
  listWorkflows: vi.fn(),
  getWorkflow:   vi.fn(),
}));

import Workflows from "../views/Workflows.vue";
import * as wfApi from "../api/workflows.js";

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/workflows", component: Workflows },
      { path: "/chat",      component: { template: "<div/>" } },
      { path: "/",          component: { template: "<div/>" } },
    ],
  });
}

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

const WF_DETAIL = {
  lineageId: "l2",
  version: 1,
  definitionJson: JSON.stringify({
    apiVersion: "aikonos.com/v1",
    kind: "Workflow",
    metadata: { name: "my-scraper" },
    steps: [{ tool: "web.fetch", args: { url: "${inputs.url}" } }],
  }),
};

describe("Workflows.vue CP21 — Improve in chat", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    useUserStore().setFromProfile({ email: "alice@example.com", sub: "alice@example.com" });
    vi.clearAllMocks();
  });

  afterEach(() => vi.restoreAllMocks());

  it("Improve button is present on owned workflow cards", async () => {
    wfApi.listWorkflows.mockResolvedValue({ workflows: [PRIVATE_WF] });
    const router = makeRouter();
    await router.push("/workflows");
    const w = mount(Workflows, { global: { plugins: [router] } });
    await flushPromises();

    const improveBtn = w.findAll("button").find((b) => b.text() === "Improve");
    expect(improveBtn).toBeDefined();
    expect(improveBtn.exists()).toBe(true);
  });

  it("clicking Improve seeds promptStore.prefill with workflow id + definition context (not auto-submit)", async () => {
    wfApi.listWorkflows.mockResolvedValue({ workflows: [PRIVATE_WF] });
    wfApi.getWorkflow.mockResolvedValue(WF_DETAIL);
    const router = makeRouter();
    await router.push("/workflows");
    const w = mount(Workflows, { global: { plugins: [router] } });
    await flushPromises();

    const promptStore = usePromptStore();

    const improveBtn = w.findAll("button").find((b) => b.text() === "Improve");
    await improveBtn.trigger("click");
    await flushPromises();

    // Must use prefill (draft), not pending (auto-submit).
    expect(promptStore.pending).toBe("");
    expect(promptStore.prefill).toContain("l2");
    expect(promptStore.prefill).toContain("workflow_propose");
  });

  it("clicking Improve navigates to /chat", async () => {
    wfApi.listWorkflows.mockResolvedValue({ workflows: [PRIVATE_WF] });
    wfApi.getWorkflow.mockResolvedValue(WF_DETAIL);
    const router = makeRouter();
    await router.push("/workflows");
    const w = mount(Workflows, { global: { plugins: [router] } });
    await flushPromises();

    const improveBtn = w.findAll("button").find((b) => b.text() === "Improve");
    await improveBtn.trigger("click");
    await flushPromises();

    expect(router.currentRoute.value.path).toBe("/chat");
  });

  it("Workflows.vue renders without any reference to EditWorkflowModal", async () => {
    // If EditWorkflowModal is still imported the component tree will include it;
    // assert it is entirely absent from the rendered output.
    wfApi.listWorkflows.mockResolvedValue({ workflows: [PRIVATE_WF] });
    const router = makeRouter();
    await router.push("/workflows");
    const w = mount(Workflows, { global: { plugins: [router] } });
    await flushPromises();

    // No "Edit" button should be present on owned cards.
    const editBtn = w.findAll("button").find((b) => b.text() === "Edit");
    expect(editBtn).toBeUndefined();

    // The rendered HTML must not contain EditWorkflowModal markup.
    expect(w.html()).not.toContain("edit-modal");
    expect(w.html()).not.toContain("Edit Workflow");
  });

  it("Improve button is absent for a non-owned private card (isOwner: false)", async () => {
    const nonOwned = { ...PRIVATE_WF, isOwner: false };
    wfApi.listWorkflows.mockResolvedValue({ workflows: [nonOwned] });
    const router = makeRouter();
    await router.push("/workflows");
    const w = mount(Workflows, { global: { plugins: [router] } });
    await flushPromises();

    // The card renders in the private section via the filter, but isOwner is
    // false — the v-if guard must suppress the Improve button entirely.
    const improveBtn = w.findAll("button").find((b) => b.text() === "Improve");
    expect(improveBtn).toBeUndefined();
  });

  it("clicking Improve with malformed definitionJson falls back to raw string without throwing", async () => {
    const malformedDetail = { lineageId: "l2", version: 1, definitionJson: "not-valid-json{{" };
    wfApi.listWorkflows.mockResolvedValue({ workflows: [PRIVATE_WF] });
    wfApi.getWorkflow.mockResolvedValue(malformedDetail);
    const router = makeRouter();
    await router.push("/workflows");
    const w = mount(Workflows, { global: { plugins: [router] } });
    await flushPromises();

    const promptStore = usePromptStore();
    const improveBtn = w.findAll("button").find((b) => b.text() === "Improve");
    await improveBtn.trigger("click");
    await flushPromises();

    // No error banner — the fallback must absorb the parse failure.
    const errorBanner = w.find("[data-testid='error-banner']");
    expect(errorBanner.exists()).toBe(false);
    // Prefill must still contain the raw string so the user sees something.
    expect(promptStore.prefill).toContain("not-valid-json{{");
    // Navigation must still proceed to /chat.
    expect(router.currentRoute.value.path).toBe("/chat");
  });

  it("prefill message names workflow_propose as the improve path", async () => {
    wfApi.listWorkflows.mockResolvedValue({ workflows: [PRIVATE_WF] });
    wfApi.getWorkflow.mockResolvedValue(WF_DETAIL);
    const router = makeRouter();
    await router.push("/workflows");
    const w = mount(Workflows, { global: { plugins: [router] } });
    await flushPromises();

    const promptStore = usePromptStore();
    const improveBtn = w.findAll("button").find((b) => b.text() === "Improve");
    await improveBtn.trigger("click");
    await flushPromises();

    // The seeded text must instruct the agent to use workflow_propose.
    expect(promptStore.prefill).toContain("workflow_propose");
    // Must include the workflow name so the agent knows what it is improving.
    expect(promptStore.prefill).toContain("my-scraper");
  });
});
