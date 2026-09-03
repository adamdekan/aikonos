// CP2 (deepening-contracts, C12) guard: each of Workflows.vue's 5 modal-open
// functions must hand its modal the COMPLETE backing object, not a
// hand-picked field subset. `missingRequirements` is present on every
// WorkflowSummary row but was never copied by any of the 5 pre-CP2 subset
// literals — so its presence on what each modal receives is proof the whole
// object crossed the boundary. Red-first: this assertion fails against the
// pre-CP2 `{ field: wf.field, ... }` construction (the field is simply absent).
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";

vi.mock("../api/workflows.js", () => ({
  listWorkflows: vi.fn(),
  getWorkflow: vi.fn(),
}));

import Workflows from "../views/Workflows.vue";
import * as wfApi from "../api/workflows.js";

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/workflows", component: Workflows },
      { path: "/",          component: { template: "<div/>" } },
    ],
  });
}

// Owned + private so Run/Publish/Versions/Fork/Delete all render on one card.
// `missingRequirements` is the probe field: absent from every pre-CP2 subset.
const WF = {
  lineageId: "l1",
  name: "daily-report",
  version: 2,
  visibilityKind: "private",
  status: "active",
  accessState: "runnable",
  missingRequirements: ["skill:probe-field"],
  isOwner: true,
};

// Minimal stand-ins for the 5 modal components — capture exactly the
// `workflow` prop they were handed, without exercising their own internals
// (network calls, discovery store, etc.), which are out of scope for this
// pass-through guard.
function stub(testid) {
  return {
    props: ["workflow", "visible"],
    template: `<div :data-testid="'${testid}'">{{ JSON.stringify(workflow) }}</div>`,
  };
}

function clickButtonNamed(w, text) {
  const btn = w.findAll("button").find((b) => b.text() === text);
  expect(btn, `button "${text}" not found`).toBeTruthy();
  return btn.trigger("click");
}

describe("Workflows.vue modal-open functions pass the whole backing object (CP2)", () => {
  let w;

  beforeEach(async () => {
    setActivePinia(createPinia());
    useUserStore().setFromProfile({ email: "alice@example.com", sub: "alice@example.com" });
    vi.clearAllMocks();
    wfApi.listWorkflows.mockResolvedValue({ workflows: [WF] });
    wfApi.getWorkflow.mockResolvedValue({ definitionJson: JSON.stringify({ inputs: [] }), version: 2 });

    const router = makeRouter();
    await router.push("/workflows");
    w = mount(Workflows, {
      global: {
        plugins: [router],
        stubs: {
          RunWorkflowModal: stub("run-stub"),
          PublishWorkflowDialog: stub("publish-stub"),
          VersionSwitcherModal: stub("versions-stub"),
          ForkWorkflowModal: stub("fork-stub"),
        },
      },
    });
    await flushPromises();
  });

  afterEach(() => vi.restoreAllMocks());

  it("openRunModal hands RunWorkflowModal the full row + fetched definition", async () => {
    await clickButtonNamed(w, "Run");
    await flushPromises();
    expect(w.find("[data-testid='run-stub']").text()).toContain("skill:probe-field");
  });

  it("openPublishDialog hands PublishWorkflowDialog the full row", async () => {
    await clickButtonNamed(w, "Publish");
    expect(w.find("[data-testid='publish-stub']").text()).toContain("skill:probe-field");
  });

  it("openVersionsModal hands VersionSwitcherModal the full row", async () => {
    await clickButtonNamed(w, "Versions");
    expect(w.find("[data-testid='versions-stub']").text()).toContain("skill:probe-field");
  });

  it("openForkModal hands ForkWorkflowModal the full row", async () => {
    await clickButtonNamed(w, "Fork");
    expect(w.find("[data-testid='fork-stub']").text()).toContain("skill:probe-field");
  });

  it("openDeleteConfirm holds the full row (deleteTarget), not a subset", async () => {
    await clickButtonNamed(w, "Delete");
    // deleteTarget has no separate modal component to inspect a prop on (its
    // confirmation UI is inline in Workflows.vue), so check the exposed ref
    // directly: pre-CP2 it held only { lineageId, name, shared }.
    expect(w.vm.deleteTarget.missingRequirements).toEqual(["skill:probe-field"]);
  });
});
