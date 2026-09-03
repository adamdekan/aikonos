// CP10c component tests: PublishWorkflowDialog, VersionSwitcherModal, ForkWorkflowModal.
// EditWorkflowModal was removed in CP21 (replaced by "Improve in chat").
//
// WHY: each modal has a user-visible contract:
//   - Publish: renders groups from listDelegatableUsers; submit calls publishWorkflow
//     with selected groupIds; gate-unmet error is surfaced.
//   - Versions: renders items from listVersions; pin calls pinVersion with the
//     chosen version; clear calls clearPin; decide calls decideVersion for proposed.
//   - Fork: submit calls forkWorkflow with the new name.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";

// ── Module mocks ─────────────────────────────────────────────────────────────

vi.mock("../api/workflows.js", () => ({
  listWorkflows:   vi.fn(),
  getWorkflow:     vi.fn(),
  forkWorkflow:    vi.fn(),
  publishWorkflow: vi.fn(),
  pinVersion:      vi.fn(),
  clearPin:        vi.fn(),
  listVersions:    vi.fn(),
  decideVersion:   vi.fn(),
}));

vi.mock("../api/delegation.js", () => ({
  listDelegatableUsers: vi.fn(),
}));

import * as wfApi from "../api/workflows.js";
import * as delegApi from "../api/delegation.js";

import PublishWorkflowDialog from "../components/PublishWorkflowDialog.vue";
import VersionSwitcherModal from "../components/VersionSwitcherModal.vue";
import ForkWorkflowModal from "../components/ForkWorkflowModal.vue";

// ── Fixtures ─────────────────────────────────────────────────────────────────

const WF = {
  lineageId: "l1",
  name:      "my-workflow",
  version:   2,
};

// Matches the real /delegatable-users shape: { groupId, displayName, memberCount }.
const GROUPS = [
  { groupId: "g1", displayName: "Team Alpha" },
  { groupId: "g2", displayName: "Team Beta" },
];

const VERSIONS = [
  { version: 2, approvalState: "approved", createdAt: "2025-01-02T00:00:00Z" },
  { version: 1, approvalState: "approved", createdAt: "2025-01-01T00:00:00Z" },
];

// CP22: includes a proposed version for approve/reject tests.
const VERSIONS_WITH_PROPOSED = [
  { version: 2, approvalState: "proposed",  createdAt: "2025-01-02T00:00:00Z" },
  { version: 1, approvalState: "approved",  createdAt: "2025-01-01T00:00:00Z" },
];

const WF_OWNER    = { lineageId: "l1", name: "my-workflow", version: 1, isOwner: true };
const WF_NON_OWNER = { lineageId: "l1", name: "my-workflow", version: 1, isOwner: false };

// ── Helpers ───────────────────────────────────────────────────────────────────

function mountWith(Component, props = {}) {
  setActivePinia(createPinia());
  return mount(Component, {
    props,
    global: { stubs: { Icon: { template: "<span />" } } },
  });
}

// ── PublishWorkflowDialog ─────────────────────────────────────────────────────

describe("PublishWorkflowDialog", () => {
  beforeEach(() => vi.clearAllMocks());

  it("renders groups from listDelegatableUsers", async () => {
    delegApi.listDelegatableUsers.mockResolvedValue({ groups: GROUPS });
    wfApi.publishWorkflow.mockResolvedValue({ visibilityKind: "shared", groups: ["g1"] });

    const w = mountWith(PublishWorkflowDialog, { workflow: WF, visible: true });
    await flushPromises();

    expect(w.find("[data-testid='group-list']").exists()).toBe(true);
    expect(w.text()).toContain("Team Alpha");
    expect(w.text()).toContain("Team Beta");
  });

  it("submit calls publishWorkflow with selected groupIds", async () => {
    delegApi.listDelegatableUsers.mockResolvedValue({ groups: GROUPS });
    wfApi.publishWorkflow.mockResolvedValue({ visibilityKind: "shared", groups: ["g1"] });

    const w = mountWith(PublishWorkflowDialog, { workflow: WF, visible: true });
    await flushPromises();

    // Trigger toggleGroup by clicking the row's toggle switch.
    const toggle = w.find("[data-testid='group-g1'] [role='switch']");
    await toggle.trigger("click");

    await w.find("[data-testid='btn-publish']").trigger("click");
    await flushPromises();

    expect(wfApi.publishWorkflow).toHaveBeenCalledWith(
      WF.lineageId,
      expect.objectContaining({ groupIds: expect.arrayContaining(["g1"]) }),
    );
  });

  it("surfaces gate-unmet error when publishWorkflow rejects", async () => {
    delegApi.listDelegatableUsers.mockResolvedValue({ groups: GROUPS });
    wfApi.publishWorkflow.mockRejectedValue(new Error("run and rate it successfully first"));

    const w = mountWith(PublishWorkflowDialog, { workflow: WF, visible: true });
    await flushPromises();

    // Select a group so submit is enabled.
    const toggle = w.find("[data-testid='group-g1'] [role='switch']");
    await toggle.trigger("click");

    await w.find("[data-testid='btn-publish']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='pub-error']").exists()).toBe(true);
    expect(w.text()).toContain("run and rate it successfully first");
  });
});

// ── VersionSwitcherModal ──────────────────────────────────────────────────────

describe("VersionSwitcherModal", () => {
  beforeEach(() => vi.clearAllMocks());

  it("renders versions from listVersions", async () => {
    wfApi.listVersions.mockResolvedValue({ versions: VERSIONS });

    const w = mountWith(VersionSwitcherModal, { workflow: WF, visible: true });
    await flushPromises();

    const items = w.findAll("[data-testid='version-item']");
    expect(items.length).toBe(2);
    expect(w.text()).toContain("v2");
    expect(w.text()).toContain("v1");
  });

  it("pin button calls pinVersion with the chosen version", async () => {
    wfApi.listVersions.mockResolvedValue({ versions: VERSIONS });
    wfApi.pinVersion.mockResolvedValue({ ok: true });

    // workflow.version = 2, so version 1 should show the Pin button.
    const w = mountWith(VersionSwitcherModal, { workflow: WF, visible: true });
    await flushPromises();

    const pinBtn = w.find("[data-testid='btn-pin-1']");
    expect(pinBtn.exists()).toBe(true);
    await pinBtn.trigger("click");
    await flushPromises();

    expect(wfApi.pinVersion).toHaveBeenCalledWith(WF.lineageId, { version: 1 });
  });

  it("clear button calls clearPin", async () => {
    wfApi.listVersions.mockResolvedValue({ versions: VERSIONS });
    wfApi.clearPin.mockResolvedValue({ ok: true });

    const w = mountWith(VersionSwitcherModal, { workflow: WF, visible: true });
    await flushPromises();

    await w.find("[data-testid='btn-clear-pin']").trigger("click");
    await flushPromises();

    expect(wfApi.clearPin).toHaveBeenCalledWith(WF.lineageId);
  });
});

// ── VersionSwitcherModal — CP22 (approve/reject proposed) ────────────────────

describe("VersionSwitcherModal — CP22 approve/reject", () => {
  beforeEach(() => vi.clearAllMocks());

  it("proposed version renders Approve and Reject buttons for owner", async () => {
    wfApi.listVersions.mockResolvedValue({ versions: VERSIONS_WITH_PROPOSED });

    const w = mountWith(VersionSwitcherModal, { workflow: WF_OWNER, visible: true });
    await flushPromises();

    expect(w.find("[data-testid='btn-approve-2']").exists()).toBe(true);
    expect(w.find("[data-testid='btn-reject-2']").exists()).toBe(true);
  });

  it("Approve calls decideVersion with approved:true and refreshes list", async () => {
    // Stable default: always return proposed list (used for initial load).
    wfApi.listVersions.mockResolvedValue({ versions: VERSIONS_WITH_PROPOSED });
    wfApi.decideVersion.mockResolvedValue({ approvalState: "approved" });

    const w = mountWith(VersionSwitcherModal, { workflow: WF_OWNER, visible: true });
    await flushPromises();

    const callsBeforeDecide = wfApi.listVersions.mock.calls.length;

    // Queue the post-decide refresh response before triggering the action.
    wfApi.listVersions.mockResolvedValueOnce({ versions: [
      { version: 2, approvalState: "approved", createdAt: "2025-01-02T00:00:00Z" },
      { version: 1, approvalState: "approved", createdAt: "2025-01-01T00:00:00Z" },
    ] });

    await w.find("[data-testid='btn-approve-2']").trigger("click");
    await flushPromises();

    expect(wfApi.decideVersion).toHaveBeenCalledWith(
      WF_OWNER.lineageId,
      { version: 2, approved: true, reason: "" },
    );
    // list refreshed: one additional call after the decision
    expect(wfApi.listVersions.mock.calls.length).toBe(callsBeforeDecide + 1);
  });

  it("Reject calls decideVersion with approved:false and refreshes list", async () => {
    wfApi.listVersions
      .mockResolvedValueOnce({ versions: VERSIONS_WITH_PROPOSED })
      .mockResolvedValueOnce({ versions: [
        { version: 2, approvalState: "rejected", createdAt: "2025-01-02T00:00:00Z" },
        { version: 1, approvalState: "approved",  createdAt: "2025-01-01T00:00:00Z" },
      ] });
    wfApi.decideVersion.mockResolvedValue({ approvalState: "rejected" });

    const w = mountWith(VersionSwitcherModal, { workflow: WF_OWNER, visible: true });
    await flushPromises();

    await w.find("[data-testid='btn-reject-2']").trigger("click");
    await flushPromises();

    expect(wfApi.decideVersion).toHaveBeenCalledWith(
      WF_OWNER.lineageId,
      { version: 2, approved: false, reason: "" },
    );
    expect(wfApi.listVersions).toHaveBeenCalledTimes(2);
  });

  it("non-owner sees no Approve/Reject buttons on proposed version", async () => {
    wfApi.listVersions.mockResolvedValue({ versions: VERSIONS_WITH_PROPOSED });

    const w = mountWith(VersionSwitcherModal, { workflow: WF_NON_OWNER, visible: true });
    await flushPromises();

    expect(w.find("[data-testid='btn-approve-2']").exists()).toBe(false);
    expect(w.find("[data-testid='btn-reject-2']").exists()).toBe(false);
  });

  it("non-proposed versions show no Approve/Reject buttons", async () => {
    wfApi.listVersions.mockResolvedValue({ versions: VERSIONS });

    const w = mountWith(VersionSwitcherModal, { workflow: WF_OWNER, visible: true });
    await flushPromises();

    expect(w.find("[data-testid='btn-approve-2']").exists()).toBe(false);
    expect(w.find("[data-testid='btn-reject-2']").exists()).toBe(false);
    expect(w.find("[data-testid='btn-approve-1']").exists()).toBe(false);
    expect(w.find("[data-testid='btn-reject-1']").exists()).toBe(false);
  });
});

// ── ForkWorkflowModal ─────────────────────────────────────────────────────────

describe("ForkWorkflowModal", () => {
  beforeEach(() => vi.clearAllMocks());

  it("submit calls forkWorkflow with the new name", async () => {
    wfApi.forkWorkflow.mockResolvedValue({ lineageId: "l2" });

    const w = mountWith(ForkWorkflowModal, { workflow: WF, visible: true });
    await flushPromises();

    const nameInput = w.find("[data-testid='input-fork-name']");
    await nameInput.setValue("my-fork");

    await w.find("[data-testid='btn-fork']").trigger("click");
    await flushPromises();

    expect(wfApi.forkWorkflow).toHaveBeenCalledWith(WF.lineageId, { newName: "my-fork" });
  });

  it("pre-fills name with 'Fork of <original>'", async () => {
    const w = mountWith(ForkWorkflowModal, { workflow: WF, visible: true });
    await flushPromises();

    const nameInput = w.find("[data-testid='input-fork-name']");
    expect(nameInput.element.value).toContain("Fork of my-workflow");
  });

  it("surfaces error when forkWorkflow rejects", async () => {
    wfApi.forkWorkflow.mockRejectedValue(new Error("fork failed"));

    const w = mountWith(ForkWorkflowModal, { workflow: WF, visible: true });
    await flushPromises();

    await w.find("[data-testid='btn-fork']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='fork-error']").exists()).toBe(true);
    expect(w.text()).toContain("fork failed");
  });
});
