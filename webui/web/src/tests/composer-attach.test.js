// Composer.vue — F7 "Attach image" reference-image upload tests.
// Covers: attach button triggers the hidden file input; selecting a file uploads
// base64 to references/<name> and inserts "#references/<name> " into the draft at
// the caret; upload failure surfaces via the shared useToast() error path.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount } from "@vue/test-utils";
import { createPinia } from "pinia";
import Composer from "../components/Composer.vue";
import { useToast } from "../components/ui/useToast.js";

vi.mock("../api/files.js", () => ({
  uploadFile: vi.fn(),
  createDir:  vi.fn(),
}));

// Composer's onMounted unconditionally loads the workspace store (CP6,
// ) — mock it here so this suite doesn't leak
// a real (token-less) client.js call.
vi.mock("../api/workspace.js", () => ({
  getWorkspaceBackend: vi.fn().mockResolvedValue({
    pref: { backend: "local", onedriveFolderPath: "" },
    onedriveAvailable: false,
    onedriveStatus: "",
  }),
  setWorkspaceBackend: vi.fn(),
  listOneDriveFolders: vi.fn(),
}));

import * as filesApi from "../api/files.js";

function mountComposer(props = {}) {
  return mount(Composer, {
    props: { modelValue: "", ...props },
    attachTo: document.body,
    global: { plugins: [createPinia()] },
  });
}

describe("Composer.vue — attach image", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useToast().toasts.splice(0);
  });

  afterEach(() => {
    document.body.innerHTML = "";
  });

  it("clicking the Attach image button triggers the hidden file input", async () => {
    const w = mountComposer();
    const input = w.find("[data-testid='attach-image-input']");
    expect(input.exists()).toBe(true);
    expect(input.attributes("type")).toBe("file");
    const accept = input.attributes("accept");
    expect(accept).toContain("image/*");
    expect(accept).toContain(".pdf");
    expect(accept).toContain(".docx");
    expect(accept).toContain(".csv");

    const clickSpy = vi.spyOn(input.element, "click");
    await w.find("[data-testid='attach-image-btn']").trigger("click");
    expect(clickSpy).toHaveBeenCalledTimes(1);
  });

  it("selecting a file uploads base64 to references/<name> and inserts #references/<name> at the caret", async () => {
    filesApi.uploadFile.mockResolvedValue({ file: { path: "references/cat.png", size: 4, isDir: false } });
    const w = mountComposer({ modelValue: "look at this " });

    const ta = w.find("textarea").element;
    ta.value = "look at this ";
    ta.selectionStart = ta.value.length;
    ta.selectionEnd = ta.value.length;

    const file = new File(["fake-bytes"], "cat.png", { type: "image/png" });
    const input = w.find("[data-testid='attach-image-input']").element;
    Object.defineProperty(input, "files", { value: [file], configurable: true });
    await w.find("[data-testid='attach-image-input']").trigger("change");

    // jsdom's FileReader.readAsDataURL resolves via two nested setImmediate hops
    // (an implementation detail, not a fixed tick count) — poll for the emit
    // instead of guessing how many macrotask ticks to await.
    await vi.waitFor(() => expect(w.emitted("update:modelValue")).toBeTruthy());

    expect(filesApi.uploadFile).toHaveBeenCalledWith("references/cat.png", expect.any(String));
    const emitted = w.emitted("update:modelValue");
    const last = emitted[emitted.length - 1][0];
    expect(last).toBe("look at this #references/cat.png ");
  });

  it("inserts the reference at the current caret position, not just at the end", async () => {
    filesApi.uploadFile.mockResolvedValue({ file: { path: "references/cat.png", size: 4, isDir: false } });
    const w = mountComposer({ modelValue: "before  after" });

    const ta = w.find("textarea").element;
    ta.value = "before  after";
    // caret sits between the two spaces (index 7), splitting "before " | " after"
    ta.selectionStart = 7;
    ta.selectionEnd = 7;

    const file = new File(["fake-bytes"], "cat.png", { type: "image/png" });
    const input = w.find("[data-testid='attach-image-input']").element;
    Object.defineProperty(input, "files", { value: [file], configurable: true });
    await w.find("[data-testid='attach-image-input']").trigger("change");
    await vi.waitFor(() => expect(w.emitted("update:modelValue")).toBeTruthy());

    const emitted = w.emitted("update:modelValue");
    const last = emitted[emitted.length - 1][0];
    expect(last).toBe("before #references/cat.png  after");
  });

  it("uploads a non-image document to the workspace root and inserts #<name> at the caret", async () => {
    filesApi.uploadFile.mockResolvedValue({ file: { path: "report.pdf", size: 4, isDir: false } });
    const w = mountComposer({ modelValue: "see " });

    const ta = w.find("textarea").element;
    ta.value = "see ";
    ta.selectionStart = ta.value.length;
    ta.selectionEnd = ta.value.length;

    const file = new File(["fake-bytes"], "report.pdf", { type: "application/pdf" });
    const input = w.find("[data-testid='attach-image-input']").element;
    Object.defineProperty(input, "files", { value: [file], configurable: true });
    await w.find("[data-testid='attach-image-input']").trigger("change");

    await vi.waitFor(() => expect(w.emitted("update:modelValue")).toBeTruthy());

    // Root path, not references/ — createDir must not be called for documents.
    expect(filesApi.uploadFile).toHaveBeenCalledWith("report.pdf", expect.any(String));
    expect(filesApi.createDir).not.toHaveBeenCalled();
    const emitted = w.emitted("update:modelValue");
    const last = emitted[emitted.length - 1][0];
    expect(last).toBe("see #report.pdf ");
  });

  it("upload failure surfaces an error toast and does not insert into the draft", async () => {
    filesApi.uploadFile.mockRejectedValue(new Error("upload failed (500)"));
    const w = mountComposer({ modelValue: "" });

    const ta = w.find("textarea").element;
    ta.value = "";
    ta.selectionStart = 0;
    ta.selectionEnd = 0;

    const file = new File(["fake-bytes"], "cat.png", { type: "image/png" });
    const input = w.find("[data-testid='attach-image-input']").element;
    Object.defineProperty(input, "files", { value: [file], configurable: true });
    await w.find("[data-testid='attach-image-input']").trigger("change");

    const { toasts } = useToast();
    await vi.waitFor(() => expect(toasts.length).toBe(1));
    expect(toasts[0].type).toBe("error");
    expect(toasts[0].msg).toContain("upload failed");
    // Draft must be untouched on failure.
    const emitted = w.emitted("update:modelValue");
    expect(emitted).toBeFalsy();
  });

  it("resets the file input value after handling so re-selecting the same file fires change again", async () => {
    filesApi.uploadFile.mockResolvedValue({ file: { path: "references/cat.png", size: 4, isDir: false } });
    const w = mountComposer({ modelValue: "" });

    const ta = w.find("textarea").element;
    ta.value = "";
    ta.selectionStart = 0;
    ta.selectionEnd = 0;

    const file = new File(["fake-bytes"], "cat.png", { type: "image/png" });
    const inputWrapper = w.find("[data-testid='attach-image-input']");
    const input = inputWrapper.element;
    Object.defineProperty(input, "files", { value: [file], configurable: true });
    await inputWrapper.trigger("change");

    expect(input.value).toBe("");
  });
});
