// Tests for api/admin.js skill-bundle methods + SkillBundles.vue (CP9).
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";

// ── api/admin.js unit tests ───────────────────────────────────────────────────

vi.mock("../api/client.js", () => ({
  get:    vi.fn(),
  post:   vi.fn(),
  del:    vi.fn(),
  upload: vi.fn(),
}));

import * as clientMod from "../api/client.js";
import {
  listSkillBundles,
  uploadSkillBundle,
  updateSkillBundle,
  deleteSkillBundle,
  grantSkillBundle,
} from "../api/admin.js";

describe("api/admin.js — skill-bundle methods", () => {
  beforeEach(() => vi.clearAllMocks());

  it("listSkillBundles calls GET /admin/skill-bundles", async () => {
    clientMod.get.mockResolvedValue({ bundles: [] });
    await listSkillBundles();
    expect(clientMod.get).toHaveBeenCalledWith("/admin/skill-bundles");
  });

  it("uploadSkillBundle sends raw markdown to /admin/skills/upload with text/markdown content-type", async () => {
    clientMod.upload.mockResolvedValue({ bundle: null });
    const body = "---\nname: my-skill\n---\n# body";
    await uploadSkillBundle(body);
    expect(clientMod.upload).toHaveBeenCalledWith(
      "/admin/skills/upload",
      { body, contentType: "text/markdown" },
    );
  });

  it("deleteSkillBundle calls DELETE /admin/skill-bundles/:id", async () => {
    clientMod.del.mockResolvedValue({ success: true });
    await deleteSkillBundle("bundle-uuid-123");
    expect(clientMod.del).toHaveBeenCalledWith("/admin/skill-bundles/bundle-uuid-123");
  });

  it("updateSkillBundle sends PUT to /admin/skill-bundles/:id with given contentType", async () => {
    clientMod.upload.mockResolvedValue({ bundle: null });
    const body = "---\nname: my-skill\n---\n# updated body";
    await updateSkillBundle("some-id", body, "text/markdown");
    expect(clientMod.upload).toHaveBeenCalledWith(
      "/admin/skill-bundles/some-id",
      { body, contentType: "text/markdown", method: "PUT" },
    );
  });

  it("updateSkillBundle with keywords appends ?keywords= query param", async () => {
    clientMod.upload.mockResolvedValue({ bundle: null });
    const body = "---\nname: my-skill\n---\n# updated body";
    await updateSkillBundle("some-id", body, "text/markdown", ["billing", "invoice"]);
    expect(clientMod.upload).toHaveBeenCalledWith(
      "/admin/skill-bundles/some-id?keywords=billing,invoice",
      { body, contentType: "text/markdown", method: "PUT" },
    );
  });

  it("updateSkillBundle with an empty keywords array clears keywords", async () => {
    clientMod.upload.mockResolvedValue({ bundle: null });
    const body = "---\nname: my-skill\n---\n# updated body";
    await updateSkillBundle("some-id", body, "text/markdown", []);
    expect(clientMod.upload).toHaveBeenCalledWith(
      "/admin/skill-bundles/some-id?keywords=",
      { body, contentType: "text/markdown", method: "PUT" },
    );
  });

  it("updateSkillBundle with zip sends PUT with application/zip", async () => {
    clientMod.upload.mockResolvedValue({ bundle: null });
    const blob = new Uint8Array([0x50, 0x4b]);
    await updateSkillBundle("zip-id", blob, "application/zip");
    expect(clientMod.upload).toHaveBeenCalledWith(
      "/admin/skill-bundles/zip-id",
      { body: blob, contentType: "application/zip", method: "PUT" },
    );
  });

  it("uploadSkillBundle passes explicit contentType to upload", async () => {
    clientMod.upload.mockResolvedValue({ bundle: null });
    const blob = new Uint8Array([0x50, 0x4b]);
    await uploadSkillBundle(blob, "application/zip");
    expect(clientMod.upload).toHaveBeenCalledWith(
      "/admin/skills/upload",
      { body: blob, contentType: "application/zip" },
    );
  });

  it("grantSkillBundle calls assignRole with agentskill object and can_use relation", async () => {
    clientMod.post.mockResolvedValue({ success: true });
    await grantSkillBundle("bundle-uuid-456", "security-team");
    expect(clientMod.post).toHaveBeenCalledWith(
      "/admin/assignments",
      {
        body: {
          tuple: {
            user: "group:security-team#member",
            relation: "can_use",
            object: "agentskill:bundle-uuid-456",
          },
        },
      },
    );
  });
});

// ── SkillBundles.vue component tests ─────────────────────────────────────────

import SkillBundles from "../views/admin/SkillBundles.vue";
import * as adminApi from "../api/admin.js";

const SAMPLE_BUNDLES = [
  {
    id: "uuid-1",
    name: "my-skill",
    description: "Does something useful",
    // allowedTools: dormant field — still
    // present on the wire, no longer rendered or edited in this view.
    allowedTools: ["web.fetch"],
    contextFork: false,
    disableModelInvocation: false,
    createdBy: "admin@example.com",
    keywords: ["billing", "invoice"],
    filePaths: ["references/guide.md", "scripts/run.py"],
  },
  {
    id: "uuid-2",
    name: "another-skill",
    description: "Another skill",
    allowedTools: ["doc.write"],
    contextFork: false,
    disableModelInvocation: true,
    createdBy: "admin@example.com",
    keywords: [],
    filePaths: [],
  },
];

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/admin/skill-bundles", component: SkillBundles },
      { path: "/", component: { template: "<div/>" } },
    ],
  });
}

describe("SkillBundles.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    useUserStore().setFromProfile({ email: "admin@example.com", sub: "admin@example.com" });
    vi.clearAllMocks();
  });
  afterEach(() => vi.restoreAllMocks());

  it("renders bundle list from listSkillBundles response", async () => {
    vi.spyOn(adminApi, "listSkillBundles").mockResolvedValue({ bundles: SAMPLE_BUNDLES });
    const router = makeRouter();
    await router.push("/admin/skill-bundles");
    const w = mount(SkillBundles, { global: { plugins: [router] } });
    await flushPromises();

    expect(w.findAll("[data-testid='bundle-row']").length).toBe(2);
    expect(w.text()).toContain("my-skill");
    expect(w.text()).toContain("another-skill");
  });

  // ── Files column: read-only file count ──

  it("renders a file count derived from filePaths, and a placeholder when empty", async () => {
    vi.spyOn(adminApi, "listSkillBundles").mockResolvedValue({ bundles: SAMPLE_BUNDLES });
    const router = makeRouter();
    await router.push("/admin/skill-bundles");
    const w = mount(SkillBundles, { global: { plugins: [router] } });
    await flushPromises();

    const rows = w.findAll("[data-testid='bundle-row']");
    expect(rows[0].find(".files-cell").text()).toBe("2");
    expect(rows[1].find(".files-cell").text()).toBe("—");
    // the allowed-tools table column is gone (D5)
    expect(w.text()).not.toContain("Allowed tools");
  });

  it("shows empty state when no bundles exist", async () => {
    vi.spyOn(adminApi, "listSkillBundles").mockResolvedValue({ bundles: [] });
    const router = makeRouter();
    await router.push("/admin/skill-bundles");
    const w = mount(SkillBundles, { global: { plugins: [router] } });
    await flushPromises();

    expect(w.findAll("[data-testid='bundle-row']").length).toBe(0);
    expect(w.find("[data-testid='empty-bundles']").exists()).toBe(true);
  });

  it("upload calls uploadSkillBundle with textarea content", async () => {
    vi.spyOn(adminApi, "listSkillBundles").mockResolvedValue({ bundles: [] });
    vi.spyOn(adminApi, "uploadSkillBundle").mockResolvedValue({ bundle: SAMPLE_BUNDLES[0] });
    const router = makeRouter();
    await router.push("/admin/skill-bundles");
    const w = mount(SkillBundles, { global: { plugins: [router] } });
    await flushPromises();

    const content = "---\nname: my-skill\n---\n# Body";
    await w.find("[data-testid='upload-input']").setValue(content);
    await w.find("[data-testid='upload-btn']").trigger("click");
    await flushPromises();

    expect(adminApi.uploadSkillBundle).toHaveBeenCalledWith(content, "text/markdown");
  });

  it("delete calls deleteSkillBundle with bundle id after confirmation", async () => {
    vi.spyOn(adminApi, "listSkillBundles").mockResolvedValue({ bundles: [SAMPLE_BUNDLES[0]] });
    vi.spyOn(adminApi, "deleteSkillBundle").mockResolvedValue({ success: true });
    vi.stubGlobal("confirm", () => true);
    const router = makeRouter();
    await router.push("/admin/skill-bundles");
    const w = mount(SkillBundles, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='delete-uuid-1']").trigger("click");
    await flushPromises();

    expect(adminApi.deleteSkillBundle).toHaveBeenCalledWith("uuid-1");
  });

  it("delete is cancelled when user declines confirmation", async () => {
    vi.spyOn(adminApi, "listSkillBundles").mockResolvedValue({ bundles: [SAMPLE_BUNDLES[0]] });
    vi.spyOn(adminApi, "deleteSkillBundle").mockResolvedValue({ success: true });
    vi.stubGlobal("confirm", () => false);
    const router = makeRouter();
    await router.push("/admin/skill-bundles");
    const w = mount(SkillBundles, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='delete-uuid-1']").trigger("click");
    await flushPromises();

    expect(adminApi.deleteSkillBundle).not.toHaveBeenCalled();
  });

  it("grant calls grantSkillBundle with bundle id and selected group", async () => {
    vi.spyOn(adminApi, "listSkillBundles").mockResolvedValue({ bundles: [SAMPLE_BUNDLES[0]] });
    vi.spyOn(adminApi, "grantSkillBundle").mockResolvedValue({ success: true });
    const router = makeRouter();
    await router.push("/admin/skill-bundles");
    const w = mount(SkillBundles, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='grant-group-uuid-1']").setValue("security-team");
    await w.find("[data-testid='grant-btn-uuid-1']").trigger("click");
    await flushPromises();

    expect(adminApi.grantSkillBundle).toHaveBeenCalledWith("uuid-1", "security-team");
  });

  it("shows error banner on upload failure", async () => {
    vi.spyOn(adminApi, "listSkillBundles").mockResolvedValue({ bundles: [] });
    vi.spyOn(adminApi, "uploadSkillBundle").mockRejectedValue(new Error("scripts/ entry rejected"));
    const router = makeRouter();
    await router.push("/admin/skill-bundles");
    const w = mount(SkillBundles, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='upload-input']").setValue("---\nname: bad\n---");
    await w.find("[data-testid='upload-btn']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='error-banner']").exists()).toBe(true);
    expect(w.find("[data-testid='error-banner']").text()).toContain("scripts/ entry rejected");
  });

  // ── CP12: textarea-paste create path ─────────────────────────────────────────

  it("textarea-paste create calls uploadSkillBundle with text/markdown", async () => {
    vi.spyOn(adminApi, "listSkillBundles").mockResolvedValue({ bundles: [] });
    vi.spyOn(adminApi, "uploadSkillBundle").mockResolvedValue({ bundle: SAMPLE_BUNDLES[0] });
    vi.spyOn(adminApi, "updateSkillBundle").mockResolvedValue({});
    const router = makeRouter();
    await router.push("/admin/skill-bundles");
    const w = mount(SkillBundles, { global: { plugins: [router] } });
    await flushPromises();

    const content = "---\nname: my-skill\n---\n# Body";
    await w.find("[data-testid='upload-input']").setValue(content);
    await w.find("[data-testid='upload-btn']").trigger("click");
    await flushPromises();

    expect(adminApi.uploadSkillBundle).toHaveBeenCalledWith(content, "text/markdown");
    expect(adminApi.updateSkillBundle).not.toHaveBeenCalled();
  });

  // ── CP12: Edit button enters edit mode + prefills textarea ───────────────────

  it("Edit button enters edit mode and prefills textarea with bundle frontmatter+body", async () => {
    vi.spyOn(adminApi, "listSkillBundles").mockResolvedValue({ bundles: [SAMPLE_BUNDLES[0]] });
    const router = makeRouter();
    await router.push("/admin/skill-bundles");
    const w = mount(SkillBundles, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='edit-uuid-1']").trigger("click");
    await flushPromises();

    const textarea = w.find("[data-testid='upload-input']");
    const val = textarea.element.value;
    // buildSkillMd JSON-quotes scalars so values with special chars are safe YAML
    expect(val).toContain('name: "my-skill"');
    expect(val).toContain('description: "Does something useful"');
    // allowed-tools frontmatter is preserved for SKILL.md round-trip fidelity
    // (portable format; other agent runtimes read the key)
    expect(val).toContain("allowed-tools");
    // panel label indicates edit mode
    expect(w.find("[data-testid='upload-panel-label']").text()).toContain("Edit");
  });

  // ── CP12: Edit mode omits allowed-tools when bundle has no tools ────────────
  it("buildSkillMd omits allowed-tools frontmatter when allowedTools is empty", async () => {
    const bundleWithoutTools = {
      id: "uuid-3",
      name: "empty-skill",
      description: "No tools",
      allowedTools: [],
      contextFork: false,
      disableModelInvocation: false,
      createdBy: "admin@example.com",
      keywords: [],
      filePaths: [],
    };
    vi.spyOn(adminApi, "listSkillBundles").mockResolvedValue({ bundles: [bundleWithoutTools] });
    const router = makeRouter();
    await router.push("/admin/skill-bundles");
    const w = mount(SkillBundles, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='edit-uuid-3']").trigger("click");
    await flushPromises();

    const textarea = w.find("[data-testid='upload-input']");
    const val = textarea.element.value;
    // allowed-tools should NOT appear when the array is empty (SKILL.md fidelity)
    expect(val).not.toContain("allowed-tools");
    expect(val).toContain('name: "empty-skill"');
  });

  // ── CP12: saving in edit mode calls updateSkillBundle, not uploadSkillBundle ──

  it("saving in edit mode calls updateSkillBundle with the row id", async () => {
    vi.spyOn(adminApi, "listSkillBundles").mockResolvedValue({ bundles: [SAMPLE_BUNDLES[0]] });
    vi.spyOn(adminApi, "uploadSkillBundle").mockResolvedValue({});
    vi.spyOn(adminApi, "updateSkillBundle").mockResolvedValue({ bundle: SAMPLE_BUNDLES[0] });
    const router = makeRouter();
    await router.push("/admin/skill-bundles");
    const w = mount(SkillBundles, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='edit-uuid-1']").trigger("click");
    await flushPromises();

    await w.find("[data-testid='upload-btn']").trigger("click");
    await flushPromises();

    expect(adminApi.updateSkillBundle).toHaveBeenCalledWith(
      "uuid-1", expect.any(String), "text/markdown", ["billing", "invoice"],
    );
    expect(adminApi.uploadSkillBundle).not.toHaveBeenCalled();
  });

  // ── CP12: Cancel exits edit mode back to create mode ─────────────────────────

  it("Cancel button exits edit mode and restores create mode", async () => {
    vi.spyOn(adminApi, "listSkillBundles").mockResolvedValue({ bundles: [SAMPLE_BUNDLES[0]] });
    const router = makeRouter();
    await router.push("/admin/skill-bundles");
    const w = mount(SkillBundles, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='edit-uuid-1']").trigger("click");
    await flushPromises();
    expect(w.find("[data-testid='cancel-edit-btn']").exists()).toBe(true);

    await w.find("[data-testid='cancel-edit-btn']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='cancel-edit-btn']").exists()).toBe(false);
    expect(w.find("[data-testid='upload-panel-label']").text()).not.toContain("Edit");
  });

  // ── CP12: buildSkillMd — description with colon produces valid YAML ───────────

  it("buildSkillMd produces valid YAML frontmatter when description contains a colon", async () => {
    vi.spyOn(adminApi, "listSkillBundles").mockResolvedValue({ bundles: [SAMPLE_BUNDLES[0]] });
    const router = makeRouter();
    await router.push("/admin/skill-bundles");
    const w = mount(SkillBundles, { global: { plugins: [router] } });
    await flushPromises();

    // Trigger Edit on a bundle whose description contains a colon
    const bundle = {
      id: "uuid-colon",
      name: "my-skill",
      description: "Does: something useful: here",
      allowedTools: ["web.fetch"],
      contextFork: false,
      disableModelInvocation: false,
    };
    // Directly call buildSkillMd via the Edit button on a bundle with a colon description.
    // We mount with bundles that have a colon description.
    vi.spyOn(adminApi, "listSkillBundles").mockResolvedValue({ bundles: [bundle] });
    const w2 = mount(SkillBundles, { global: { plugins: [makeRouter()] } });
    await flushPromises();

    await w2.find("[data-testid='edit-uuid-colon']").trigger("click");
    await flushPromises();

    const textarea = w2.find("[data-testid='upload-input']");
    const content = textarea.element.value;

    // Parse the YAML frontmatter from the generated content
    const fmMatch = content.match(/^---\n([\s\S]*?)\n---/);
    expect(fmMatch).toBeTruthy();
    const frontmatter = fmMatch[1];

    // The description line must be parseable as a quoted scalar — JSON.parse of
    // the quoted value must round-trip to the original string.
    const descLine = frontmatter.split("\n").find((l) => l.startsWith("description:"));
    expect(descLine).toBeTruthy();
    const quotedValue = descLine.replace(/^description:\s*/, "");
    // JSON.stringify → JSON.parse is the inverse of our quoting strategy
    expect(JSON.parse(quotedValue)).toBe("Does: something useful: here");
  });

  // ── CP12: file picker — .zip sends application/zip ───────────────────────────

  it("file picker with .zip file calls uploadSkillBundle with application/zip in create mode", async () => {
    vi.spyOn(adminApi, "listSkillBundles").mockResolvedValue({ bundles: [] });
    vi.spyOn(adminApi, "uploadSkillBundle").mockResolvedValue({ bundle: SAMPLE_BUNDLES[0] });
    const router = makeRouter();
    await router.push("/admin/skill-bundles");
    const w = mount(SkillBundles, { global: { plugins: [router] } });
    await flushPromises();

    const zipBytes = new Uint8Array([0x50, 0x4b, 0x03, 0x04]);
    const file = new File([zipBytes], "bundle.zip", { type: "application/zip" });

    // mock FileReader to simulate async read
    const originalFR = globalThis.FileReader;
    const mockRead = vi.fn();
    globalThis.FileReader = class {
      readAsArrayBuffer() { mockRead("arraybuffer"); Promise.resolve().then(() => { this.onload({ target: { result: zipBytes.buffer } }); }); }
      readAsText() {}
    };

    const input = w.find("[data-testid='file-input']");
    Object.defineProperty(input.element, "files", { value: [file], configurable: true });
    await input.trigger("change");
    await flushPromises();

    globalThis.FileReader = originalFR;

    expect(adminApi.uploadSkillBundle).toHaveBeenCalledWith(expect.anything(), "application/zip");
  });

  // ── CP12: file picker — .skill that is a zip sends application/zip ────────────
  // Regression: a .skill bundle is a zip archive; it must be routed to the zip
  // parser (application/zip), not treated as text — the latter produced
  // "invalid SKILL.md: missing frontmatter delimiter".

  it("file picker with a .skill zip bundle calls uploadSkillBundle with application/zip", async () => {
    vi.spyOn(adminApi, "listSkillBundles").mockResolvedValue({ bundles: [] });
    vi.spyOn(adminApi, "uploadSkillBundle").mockResolvedValue({ bundle: SAMPLE_BUNDLES[0] });
    const router = makeRouter();
    await router.push("/admin/skill-bundles");
    const w = mount(SkillBundles, { global: { plugins: [router] } });
    await flushPromises();

    const zipBytes = new Uint8Array([0x50, 0x4b, 0x03, 0x04, 0x14, 0x00]);
    const file = new File([zipBytes], "iso-27001.skill", { type: "" });

    const originalFR = globalThis.FileReader;
    globalThis.FileReader = class {
      readAsArrayBuffer() { Promise.resolve().then(() => { this.onload({ target: { result: zipBytes.buffer } }); }); }
      readAsText() {}
    };

    const input = w.find("[data-testid='file-input']");
    Object.defineProperty(input.element, "files", { value: [file], configurable: true });
    await input.trigger("change");
    await flushPromises();

    globalThis.FileReader = originalFR;

    expect(adminApi.uploadSkillBundle).toHaveBeenCalledWith(expect.anything(), "application/zip");
    // the binary file content must never leak into the paste textarea
    expect(w.find("[data-testid='upload-input']").element.value).toBe("");
  });

  // ── CP12: file picker — a non-zip file is rejected with a guard error ──────────
  // The file picker is the bundle path (zip only). A bare-text file picked here
  // is a mistake; surface the "paste it instead" guard and do not upload.

  it("file picker with a non-zip (text) file shows a guard error and does NOT upload", async () => {
    vi.spyOn(adminApi, "listSkillBundles").mockResolvedValue({ bundles: [] });
    vi.spyOn(adminApi, "uploadSkillBundle").mockResolvedValue({ bundle: SAMPLE_BUNDLES[0] });
    const router = makeRouter();
    await router.push("/admin/skill-bundles");
    const w = mount(SkillBundles, { global: { plugins: [router] } });
    await flushPromises();

    const textContent = "---\nname: test\n---\n# body";
    const textBuf = new TextEncoder().encode(textContent).buffer;
    const file = new File([textContent], "bundle.skill", { type: "" });

    const originalFR = globalThis.FileReader;
    globalThis.FileReader = class {
      readAsArrayBuffer() { Promise.resolve().then(() => { this.onload({ target: { result: textBuf } }); }); }
      readAsText() {}
    };

    const input = w.find("[data-testid='file-input']");
    Object.defineProperty(input.element, "files", { value: [file], configurable: true });
    await input.trigger("change");
    await flushPromises();

    globalThis.FileReader = originalFR;

    expect(adminApi.uploadSkillBundle).not.toHaveBeenCalled();
    // textarea untouched; user is told to paste instead
    expect(w.find("[data-testid='upload-input']").element.value).toBe("");
    expect(w.find("[data-testid='error-banner']").text()).toContain("paste");
  });

  // ── CP12: file picker in edit mode (.skill zip) replaces the bundle ───────────
  // In edit mode the file picker is still the zip-bundle path: picking a .skill
  // archive replaces the row via updateSkillBundle(application/zip).

  it("file picker in edit mode with a .skill zip calls updateSkillBundle with application/zip", async () => {
    vi.spyOn(adminApi, "listSkillBundles").mockResolvedValue({ bundles: [SAMPLE_BUNDLES[0]] });
    vi.spyOn(adminApi, "uploadSkillBundle").mockResolvedValue({});
    vi.spyOn(adminApi, "updateSkillBundle").mockResolvedValue({ bundle: SAMPLE_BUNDLES[0] });
    const router = makeRouter();
    await router.push("/admin/skill-bundles");
    const w = mount(SkillBundles, { global: { plugins: [router] } });
    await flushPromises();

    // enter edit mode
    await w.find("[data-testid='edit-uuid-1']").trigger("click");
    await flushPromises();

    const zipBytes = new Uint8Array([0x50, 0x4b, 0x03, 0x04, 0x14, 0x00]);
    const file = new File([zipBytes], "updated.skill", { type: "" });

    const originalFR = globalThis.FileReader;
    globalThis.FileReader = class {
      readAsArrayBuffer() { Promise.resolve().then(() => { this.onload({ target: { result: zipBytes.buffer } }); }); }
      readAsText() {}
    };

    const input = w.find("[data-testid='file-input']");
    Object.defineProperty(input.element, "files", { value: [file], configurable: true });
    await input.trigger("change");
    await flushPromises();

    globalThis.FileReader = originalFR;

    expect(adminApi.updateSkillBundle).toHaveBeenCalledWith(
      "uuid-1", expect.anything(), "application/zip", ["billing", "invoice"],
    );
    expect(adminApi.uploadSkillBundle).not.toHaveBeenCalled();
  });

  // ── CP3 (auto-skill-loading): keywords input ─────────────────────────────────

  it("renders existing keywords from the list response", async () => {
    vi.spyOn(adminApi, "listSkillBundles").mockResolvedValue({ bundles: [SAMPLE_BUNDLES[0]] });
    const router = makeRouter();
    await router.push("/admin/skill-bundles");
    const w = mount(SkillBundles, { global: { plugins: [router] } });
    await flushPromises();

    expect(w.text()).toContain("billing");
    expect(w.text()).toContain("invoice");
  });

  it("Edit prefills the keywords input from the bundle's keywords, and saving sends them", async () => {
    vi.spyOn(adminApi, "listSkillBundles").mockResolvedValue({ bundles: [SAMPLE_BUNDLES[0]] });
    vi.spyOn(adminApi, "updateSkillBundle").mockResolvedValue({ bundle: SAMPLE_BUNDLES[0] });
    const router = makeRouter();
    await router.push("/admin/skill-bundles");
    const w = mount(SkillBundles, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='edit-uuid-1']").trigger("click");
    await flushPromises();

    const keywordsInput = w.find("[data-testid='keywords-input']");
    expect(keywordsInput.element.value).toBe("billing, invoice");

    await keywordsInput.setValue("billing, invoice, refund");
    await w.find("[data-testid='upload-btn']").trigger("click");
    await flushPromises();

    expect(adminApi.updateSkillBundle).toHaveBeenCalledWith(
      "uuid-1", expect.any(String), "text/markdown", ["billing", "invoice", "refund"],
    );
  });

  it("clearing the keywords input sends an empty keywords list", async () => {
    vi.spyOn(adminApi, "listSkillBundles").mockResolvedValue({ bundles: [SAMPLE_BUNDLES[0]] });
    vi.spyOn(adminApi, "updateSkillBundle").mockResolvedValue({ bundle: SAMPLE_BUNDLES[0] });
    const router = makeRouter();
    await router.push("/admin/skill-bundles");
    const w = mount(SkillBundles, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='edit-uuid-1']").trigger("click");
    await flushPromises();

    await w.find("[data-testid='keywords-input']").setValue("");
    await w.find("[data-testid='upload-btn']").trigger("click");
    await flushPromises();

    expect(adminApi.updateSkillBundle).toHaveBeenCalledWith(
      "uuid-1", expect.any(String), "text/markdown", [],
    );
  });

  it("shows a 400 oversize keywords error via the existing error banner", async () => {
    vi.spyOn(adminApi, "listSkillBundles").mockResolvedValue({ bundles: [SAMPLE_BUNDLES[0]] });
    vi.spyOn(adminApi, "updateSkillBundle").mockRejectedValue(new Error("too many keywords (max 32)"));
    const router = makeRouter();
    await router.push("/admin/skill-bundles");
    const w = mount(SkillBundles, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='edit-uuid-1']").trigger("click");
    await flushPromises();
    await w.find("[data-testid='upload-btn']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='error-banner']").exists()).toBe(true);
    expect(w.find("[data-testid='error-banner']").text()).toContain("too many keywords");
  });
});
