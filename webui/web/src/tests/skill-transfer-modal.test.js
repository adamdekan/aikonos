// SkillTransferModal.vue — preview fetch → informed accept. api/skills.js is fully
// mocked so no server is needed.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";

vi.mock("../api/skills.js", () => ({
  getSkillTransferPreview: vi.fn(),
  acceptSkillTransfer:     vi.fn(),
}));

import SkillTransferModal from "../components/SkillTransferModal.vue";
import * as skillsApi from "../api/skills.js";

const PREVIEW = {
  skillName:   "release-notes",
  fromUserId:  "alice@example.com",
  body:        "---\nname: release-notes\n---\n\nDraft release notes.",
  manifest:    [{ path: "SKILL.md", size: 128 }, { path: "references/style.md", size: 2048 }],
  flags:       [],
  contentHash: "sha256:abc",
  conflict:    false,
};

function mountModal(props = {}) {
  return mount(SkillTransferModal, {
    props: { envelopeId: "env-1", fromDisplayName: "Alice Smith", visible: true, ...props },
  });
}

describe("SkillTransferModal.vue", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    skillsApi.getSkillTransferPreview.mockResolvedValue(PREVIEW);
  });

  afterEach(() => vi.restoreAllMocks());

  it("fetches the preview on open and renders sender + body + manifest", async () => {
    const w = mountModal();
    await flushPromises();

    expect(skillsApi.getSkillTransferPreview).toHaveBeenCalledWith("env-1");

    const sender = w.find("[data-testid='transfer-sender']");
    expect(sender.text()).toContain("Alice Smith");
    expect(sender.text()).toContain("release-notes");

    expect(w.find("[data-testid='transfer-body']").text()).toContain("Draft release notes.");

    const manifest = w.find("[data-testid='transfer-manifest']");
    expect(manifest.text()).toContain("SKILL.md");
    expect(manifest.text()).toContain("references/style.md");
    expect(manifest.text()).toContain("2.0 KB");
  });

  it("renders the body as text, never as HTML", async () => {
    skillsApi.getSkillTransferPreview.mockResolvedValue({
      ...PREVIEW,
      body: '<img src="x" onerror="window.__pwned = true">',
    });
    const w = mountModal();
    await flushPromises();

    const bodyEl = w.find("[data-testid='transfer-body']");
    // The literal markup survives as plain text …
    expect(bodyEl.text()).toContain('<img src="x" onerror="window.__pwned = true">');
    // … and is never parsed into a real element or executed.
    expect(w.find("[data-testid='transfer-body'] img").exists()).toBe(false);
    expect(w.html()).not.toMatch(/<img[^>]*onerror/);
    expect(globalThis.__pwned).toBeUndefined();
  });

  it("renders flags as a loud warning when present", async () => {
    skillsApi.getSkillTransferPreview.mockResolvedValue({ ...PREVIEW, flags: ["prompt_injection_pattern"] });
    const w = mountModal();
    await flushPromises();

    const warning = w.find("[data-testid='transfer-flags-warning']");
    expect(warning.exists()).toBe(true);
    expect(warning.text()).toContain("prompt_injection_pattern");
  });

  it("renders no flags warning when flags is empty", async () => {
    const w = mountModal();
    await flushPromises();

    expect(w.find("[data-testid='transfer-flags-warning']").exists()).toBe(false);
  });

  it("accept rename (no conflict) posts {mode: rename} with no name_override", async () => {
    skillsApi.acceptSkillTransfer.mockResolvedValue({ installedName: "release-notes" });
    const w = mountModal();
    await flushPromises();

    await w.find("[data-testid='transfer-confirm-btn']").trigger("click");
    await flushPromises();

    expect(skillsApi.acceptSkillTransfer).toHaveBeenCalledWith("env-1", { mode: "rename" });
    expect(w.emitted("accepted")).toBeTruthy();
  });

  it("accept rename on a conflict prefills a free name and posts it as name_override", async () => {
    skillsApi.getSkillTransferPreview.mockResolvedValue({ ...PREVIEW, conflict: true });
    skillsApi.acceptSkillTransfer.mockResolvedValue({ installedName: "release-notes-2" });
    const w = mountModal();
    await flushPromises();

    const nameInput = w.find("[data-testid='transfer-name-input']");
    expect(nameInput.exists()).toBe(true);
    expect(nameInput.element.value).toBe("release-notes-2");

    await nameInput.setValue("release-notes-mine");
    await w.find("[data-testid='transfer-confirm-btn']").trigger("click");
    await flushPromises();

    expect(skillsApi.acceptSkillTransfer).toHaveBeenCalledWith("env-1", {
      mode: "rename",
      name_override: "release-notes-mine",
    });
  });

  it("replace is offered only on a conflict, labeled destructively, and posts {mode: replace}", async () => {
    skillsApi.getSkillTransferPreview.mockResolvedValue({ ...PREVIEW, conflict: true });
    skillsApi.acceptSkillTransfer.mockResolvedValue({ installedName: "release-notes" });
    const w = mountModal();
    await flushPromises();

    expect(w.find("[data-testid='transfer-mode-replace']").exists()).toBe(true);

    await w.find("[data-testid='transfer-mode-replace']").setValue(true);
    const hint = w.find("[data-testid='transfer-replace-hint']");
    expect(hint.text()).toContain("deletes your existing");
    expect(hint.text()).toContain("release-notes");
    expect(hint.text()).toContain("cannot be undone");

    await w.find("[data-testid='transfer-confirm-btn']").trigger("click");
    await flushPromises();

    expect(skillsApi.acceptSkillTransfer).toHaveBeenCalledWith("env-1", { mode: "replace" });
  });

  it("replace is not offered when there is no conflict", async () => {
    const w = mountModal();
    await flushPromises();

    expect(w.find("[data-testid='transfer-mode-replace']").exists()).toBe(false);
  });

  it("a failed accept surfaces inline and does not close the modal", async () => {
    skillsApi.acceptSkillTransfer.mockRejectedValue(new Error("envelope expired"));
    const w = mountModal();
    await flushPromises();

    await w.find("[data-testid='transfer-confirm-btn']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='transfer-error']").text()).toContain("envelope expired");
    expect(w.emitted("accepted")).toBeFalsy();
  });

  it("Cancel emits close without calling accept", async () => {
    const w = mountModal();
    await flushPromises();

    await w.find("[data-testid='transfer-cancel-btn']").trigger("click");

    expect(w.emitted("close")).toBeTruthy();
    expect(skillsApi.acceptSkillTransfer).not.toHaveBeenCalled();
  });

  it("re-fetches the preview every time the modal opens", async () => {
    const w = mountModal({ visible: false });
    await flushPromises();
    expect(skillsApi.getSkillTransferPreview).not.toHaveBeenCalled();

    await w.setProps({ visible: true });
    await flushPromises();
    expect(skillsApi.getSkillTransferPreview).toHaveBeenCalledTimes(1);
  });

  it("a preview load failure shows an error banner with retry", async () => {
    skillsApi.getSkillTransferPreview.mockRejectedValue(new Error("envelope not found"));
    const w = mountModal();
    await flushPromises();

    expect(w.find("[data-testid='error-banner']").text()).toContain("envelope not found");
  });

  it("prevents double-submit by ignoring accept clicks while submitting", async () => {
    // Mock acceptSkillTransfer to return a pending promise we control
    let resolveAccept;
    const acceptPromise = new Promise((resolve) => {
      resolveAccept = resolve;
    });
    skillsApi.acceptSkillTransfer.mockReturnValue(acceptPromise);

    const w = mountModal();
    await flushPromises();

    const acceptBtn = w.find("[data-testid='transfer-confirm-btn']");

    // Trigger accept twice rapidly (before promise resolves)
    acceptBtn.trigger("click");
    await w.vm.$nextTick();
    acceptBtn.trigger("click");

    // acceptSkillTransfer called exactly once despite two clicks
    expect(skillsApi.acceptSkillTransfer).toHaveBeenCalledTimes(1);

    // submitting flag is true while request is in flight
    expect(w.vm.submitting).toBe(true);

    // Resolve the promise and clean up
    resolveAccept({ installedName: "release-notes" });
    await flushPromises();

    // After resolution, submitting flag is false
    expect(w.vm.submitting).toBe(false);
  });
});
