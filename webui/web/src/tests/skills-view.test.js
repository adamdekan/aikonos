// View tests for Skills.vue.
// api/skills.js is fully mocked; ShareSkillModal is stubbed — its own behavior
// (discovery store, group skip-report) is covered by share-skill-modal.test.js.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";

vi.mock("../api/skills.js", () => ({
  listSkills: vi.fn(),
  importSkill: vi.fn(),
  deleteSkill: vi.fn(),
}));

import Skills from "../views/Skills.vue";
import ShareSkillModal from "../components/ShareSkillModal.vue";
import * as skillsApi from "../api/skills.js";

const VALID_SKILL = {
  name: "release-notes",
  description: "Drafts release notes",
  keywords: [],
  allowedTools: [],
  disableModelInvocation: false,
  valid: true,
  warning: "",
  sizeBytes: 2048,
};

const INVALID_SKILL = {
  name: "broken-skill",
  description: "",
  keywords: [],
  allowedTools: [],
  disableModelInvocation: false,
  valid: false,
  warning: "missing description",
  sizeBytes: 512,
};

const GRANTED_BUNDLE = {
  id: "bundle-1",
  name: "billing-helper",
  description: "Admin-authored billing skill",
  body: "# Billing helper\nDo the thing.",
  allowedTools: [],
  contextFork: false,
  disableModelInvocation: false,
  createdBy: "admin@example.com",
  keywords: [],
};

function mountSkills() {
  return mount(Skills, {
    global: { stubs: { ShareSkillModal: true } },
  });
}

describe("Skills.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    vi.clearAllMocks();
  });

  afterEach(() => vi.restoreAllMocks());

  it("renders My skills rows with a valid chip and size", async () => {
    skillsApi.listSkills.mockResolvedValue({ skills: [VALID_SKILL], granted: [], grantedUnavailable: false });
    const w = mountSkills();
    await flushPromises();

    const rows = w.findAll("[data-testid='skill-row']");
    expect(rows.length).toBe(1);
    expect(w.text()).toContain("release-notes");
    expect(w.find("[data-testid='skill-status']").text()).toBe("valid");
    expect(w.text()).toContain("2.0 KB");
    expect(w.find("[data-testid='skill-warning']").exists()).toBe(false);
  });

  it("shows the invalid chip and warning text for an invalid skill", async () => {
    skillsApi.listSkills.mockResolvedValue({ skills: [INVALID_SKILL], granted: [], grantedUnavailable: false });
    const w = mountSkills();
    await flushPromises();

    expect(w.find("[data-testid='skill-status']").text()).toBe("invalid");
    expect(w.find("[data-testid='skill-warning']").text()).toBe("missing description");
  });

  it("Delete requires confirmation, then calls deleteSkill and reloads", async () => {
    skillsApi.listSkills.mockResolvedValue({ skills: [VALID_SKILL], granted: [], grantedUnavailable: false });
    skillsApi.deleteSkill.mockResolvedValue({ success: true });
    const w = mountSkills();
    await flushPromises();

    await w.find("[data-testid='btn-delete']").trigger("click");
    expect(w.find("[data-testid='delete-confirm']").exists()).toBe(true);
    expect(skillsApi.deleteSkill).not.toHaveBeenCalled();

    skillsApi.listSkills.mockResolvedValueOnce({ skills: [], granted: [], grantedUnavailable: false });
    await w.find("[data-testid='btn-delete-confirm']").trigger("click");
    await flushPromises();

    expect(skillsApi.deleteSkill).toHaveBeenCalledWith("release-notes");
    expect(w.find("[data-testid='delete-confirm']").exists()).toBe(false);
    expect(w.find("[data-testid='skill-row']").exists()).toBe(false);
  });

  it("clicking Share opens ShareSkillModal with the row's skill name", async () => {
    skillsApi.listSkills.mockResolvedValue({ skills: [VALID_SKILL], granted: [], grantedUnavailable: false });
    const w = mountSkills();
    await flushPromises();

    let modal = w.findComponent(ShareSkillModal);
    expect(modal.props("visible")).toBe(false);

    await w.find("[data-testid='btn-share']").trigger("click");
    modal = w.findComponent(ShareSkillModal);
    expect(modal.props("visible")).toBe(true);
    expect(modal.props("skillName")).toBe("release-notes");
  });

  it("import: a zip file (PK magic) is sent as application/zip and reloads on success", async () => {
    skillsApi.listSkills
      .mockResolvedValueOnce({ skills: [], granted: [], grantedUnavailable: false })
      .mockResolvedValueOnce({ skills: [VALID_SKILL], granted: [], grantedUnavailable: false });
    skillsApi.importSkill.mockResolvedValue({ name: "release-notes" });
    const w = mountSkills();
    await flushPromises();

    const zipBytes = new Uint8Array([0x50, 0x4b, 0x03, 0x04, 0, 0]);
    const file = new File([zipBytes], "bundle.zip", { type: "application/zip" });
    await w.vm.importFile(file);
    await flushPromises();

    expect(skillsApi.importSkill).toHaveBeenCalledTimes(1);
    const [body, contentType] = skillsApi.importSkill.mock.calls[0];
    expect(contentType).toBe("application/zip");
    expect(body).toBeInstanceOf(ArrayBuffer);
    expect(skillsApi.listSkills).toHaveBeenCalledTimes(2);
    expect(w.find("[data-testid='skill-row']").exists()).toBe(true);
  });

  it("import: a bare SKILL.md text file is sent as text/markdown", async () => {
    skillsApi.listSkills.mockResolvedValue({ skills: [], granted: [], grantedUnavailable: false });
    skillsApi.importSkill.mockResolvedValue({ name: "my-notes" });
    const w = mountSkills();
    await flushPromises();

    const file = new File(["---\nname: my-notes\ndescription: d\n---\nbody"], "SKILL.md", { type: "text/markdown" });
    await w.vm.importFile(file);
    await flushPromises();

    const [body, contentType] = skillsApi.importSkill.mock.calls[0];
    expect(contentType).toBe("text/markdown");
    expect(body).toContain("name: my-notes");
  });

  it("import: a 409 conflict surfaces the suggested name, never a generic error", async () => {
    skillsApi.listSkills.mockResolvedValue({ skills: [VALID_SKILL], granted: [], grantedUnavailable: false });
    const conflictErr = new Error('a skill named "my-notes" already exists');
    conflictErr.data = { error: conflictErr.message, suggested_name: "my-notes-2" };
    skillsApi.importSkill.mockRejectedValue(conflictErr);
    const w = mountSkills();
    await flushPromises();

    const file = new File(["---\nname: my-notes\ndescription: d\n---\nbody"], "SKILL.md", { type: "text/markdown" });
    await w.vm.importFile(file);
    await flushPromises();

    const conflictNode = w.find("[data-testid='import-conflict']");
    expect(conflictNode.exists()).toBe(true);
    expect(conflictNode.text()).toContain("my-notes-2");
    expect(w.find("[data-testid='import-error']").exists()).toBe(false);
  });

  it("import: a non-conflict failure surfaces as a plain import error", async () => {
    skillsApi.listSkills.mockResolvedValue({ skills: [], granted: [], grantedUnavailable: false });
    skillsApi.importSkill.mockRejectedValue(new Error("bundle exceeds folder cap"));
    const w = mountSkills();
    await flushPromises();

    const file = new File(["junk"], "SKILL.md", { type: "text/markdown" });
    await w.vm.importFile(file);
    await flushPromises();

    expect(w.find("[data-testid='import-error']").text()).toContain("bundle exceeds folder cap");
    expect(w.find("[data-testid='import-conflict']").exists()).toBe(false);
  });

  it("renders the Granted skills section read-only — no share/delete actions", async () => {
    skillsApi.listSkills.mockResolvedValue({ skills: [], granted: [GRANTED_BUNDLE], grantedUnavailable: false });
    const w = mountSkills();
    await flushPromises();

    const row = w.find("[data-testid='granted-row']");
    expect(row.exists()).toBe(true);
    expect(row.text()).toContain("billing-helper");
    expect(row.find("[data-testid='btn-share']").exists()).toBe(false);
    expect(row.find("[data-testid='btn-delete']").exists()).toBe(false);

    await row.find("[data-testid='btn-view-granted']").trigger("click");
    await flushPromises();
    expect(w.find("[data-testid='granted-detail']").text()).toContain("Do the thing.");
  });

  it("shows an ErrorBanner scoped to the Granted section when grantedUnavailable", async () => {
    skillsApi.listSkills.mockResolvedValue({ skills: [VALID_SKILL], granted: [], grantedUnavailable: true });
    const w = mountSkills();
    await flushPromises();

    expect(w.find("[data-testid='granted-unavailable']").exists()).toBe(true);
    // My skills still render — the granted-section outage does not blank it.
    expect(w.find("[data-testid='skill-row']").exists()).toBe(true);
  });

  it("renders a no-access panel on a 403, keeping the nav usable elsewhere", async () => {
    skillsApi.listSkills.mockResolvedValue({ forbidden: true, error: "denied" });
    const w = mountSkills();
    await flushPromises();

    expect(w.find("[data-testid='forbidden']").exists()).toBe(true);
    expect(w.find("[data-testid='skill-row']").exists()).toBe(false);
    expect(w.find("[data-testid='error-banner']").exists()).toBe(false);
  });

  it("filters My skills and Granted skills by name, case-insensitively", async () => {
    skillsApi.listSkills.mockResolvedValue({
      skills: [VALID_SKILL, { ...INVALID_SKILL, name: "other-thing" }],
      granted: [GRANTED_BUNDLE],
      grantedUnavailable: false,
    });
    const w = mountSkills();
    await flushPromises();

    await w.find("[data-testid='skill-filter-input']").setValue("RELEASE");
    await flushPromises();

    expect(w.find("[data-testid='skill-row']").text()).toContain("release-notes");
    expect(w.findAll("[data-testid='skill-row']").length).toBe(1);
    expect(w.find("[data-testid='granted-row']").exists()).toBe(false);
  });
});
