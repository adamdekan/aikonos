// ShareSkillModal.vue — pick target (user/group) → confirm → POST share
//. Discovery is the shared
// users+groups store (PublishWorkflowDialog precedent); shareSkill is mocked.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";

// discovery.js's underlying datasets — mocked so refresh() on open never hits
// a real (token-less) client.js call.
vi.mock("../api/admin.js", () => ({ listUserSkillBundles: vi.fn().mockResolvedValue({ bundles: [] }) }));
vi.mock("../api/files.js", () => ({ listFiles: vi.fn().mockResolvedValue({ files: [] }) }));
const delegationApi = vi.hoisted(() => ({ listDelegatableUsers: vi.fn() }));
vi.mock("../api/delegation.js", () => delegationApi);

vi.mock("../api/skills.js", () => ({ shareSkill: vi.fn() }));

import ShareSkillModal from "../components/ShareSkillModal.vue";
import * as skillsApi from "../api/skills.js";

const USERS = [
  { userId: "alice@example.com", displayName: "Alice Smith" },
  { userId: "bob@example.com", displayName: "Bob Jones" },
];

const GROUPS = [{ groupId: "group:eng", displayName: "Engineering", memberCount: 5 }];

function mountModal(props = {}) {
  return mount(ShareSkillModal, {
    props: { skillName: "release-notes", visible: true, ...props },
  });
}

describe("ShareSkillModal.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    vi.clearAllMocks();
    delegationApi.listDelegatableUsers.mockResolvedValue({ users: USERS, groups: GROUPS });
  });

  afterEach(() => vi.restoreAllMocks());

  it("lists users and groups from discovery on open", async () => {
    const w = mountModal();
    await flushPromises();

    const list = w.find("[data-testid='share-target-list']");
    expect(list.text()).toContain("Alice Smith");
    expect(list.text()).toContain("Bob Jones");
    expect(list.text()).toContain("Engineering");
  });

  it("filters the target list by displayName, case-insensitively", async () => {
    const w = mountModal();
    await flushPromises();

    await w.find("[data-testid='share-search']").setValue("ALICE");
    expect(w.find("[data-testid='share-target-list']").text()).toContain("Alice Smith");
    expect(w.find("[data-testid='share-target-list']").text()).not.toContain("Bob Jones");
  });

  it("direct share: pick a user, confirm, and POST {user_id}", async () => {
    skillsApi.shareSkill.mockResolvedValue({ envelopeIds: ["env-1"], skippedUserIds: [] });
    const w = mountModal();
    await flushPromises();

    await w.find("[data-testid='share-target-alice@example.com']").trigger("click");
    expect(w.text()).toContain("Alice Smith");

    await w.find("[data-testid='share-confirm-btn']").trigger("click");
    await flushPromises();

    expect(skillsApi.shareSkill).toHaveBeenCalledWith("release-notes", { user_id: "alice@example.com" });
    expect(w.find("[data-testid='share-success']").exists()).toBe(true);
    expect(w.find("[data-testid='share-skip-report']").exists()).toBe(false);
  });

  it("group share: pick a group, confirm, POST {group_id}, and surface the skip report", async () => {
    skillsApi.shareSkill.mockResolvedValue({ envelopeIds: ["env-1", "env-2"], skippedUserIds: ["carol@example.com"] });
    const w = mountModal();
    await flushPromises();

    await w.find("[data-testid='share-target-group:eng']").trigger("click");
    await w.find("[data-testid='share-confirm-btn']").trigger("click");
    await flushPromises();

    expect(skillsApi.shareSkill).toHaveBeenCalledWith("release-notes", { group_id: "group:eng" });
    const skipReport = w.find("[data-testid='share-skip-report']");
    expect(skipReport.exists()).toBe(true);
    expect(skipReport.text()).toContain("1 member skipped — lack the skill.");
  });

  it("group share with no skipped members shows no skip report", async () => {
    skillsApi.shareSkill.mockResolvedValue({ envelopeIds: ["env-1"], skippedUserIds: [] });
    const w = mountModal();
    await flushPromises();

    await w.find("[data-testid='share-target-group:eng']").trigger("click");
    await w.find("[data-testid='share-confirm-btn']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='share-skip-report']").exists()).toBe(false);
  });

  it("Back returns from confirm to the pick step without sending", async () => {
    const w = mountModal();
    await flushPromises();

    await w.find("[data-testid='share-target-alice@example.com']").trigger("click");
    await w.find(".btn-cancel").trigger("click"); // Back button in confirm step
    await flushPromises();

    expect(skillsApi.shareSkill).not.toHaveBeenCalled();
    expect(w.find("[data-testid='share-target-list']").exists()).toBe(true);
  });

  it("a send failure surfaces inline and does not close the modal", async () => {
    skillsApi.shareSkill.mockRejectedValue(new Error("recipient lacks the skill grant"));
    const w = mountModal();
    await flushPromises();

    await w.find("[data-testid='share-target-alice@example.com']").trigger("click");
    await w.find("[data-testid='share-confirm-btn']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='share-error']").text()).toContain("recipient lacks the skill grant");
  });

  it("resets state on every open (no stale pick survives)", async () => {
    const w = mountModal({ visible: false });
    await flushPromises();
    await w.setProps({ visible: true });
    await flushPromises();

    expect(w.find("[data-testid='share-target-list']").exists()).toBe(true);
    expect(delegationApi.listDelegatableUsers).toHaveBeenCalled();
  });

  it("emits close when Cancel is clicked on the pick step", async () => {
    const w = mountModal();
    await flushPromises();

    await w.find(".btn-cancel").trigger("click");
    expect(w.emitted("close")).toBeTruthy();
  });
});
