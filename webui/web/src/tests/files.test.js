// View tests for Files.vue.
// api/files.js is fully mocked so no server is needed.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";

vi.mock("../api/files.js", () => ({
  listFiles:    vi.fn(),
  uploadFile:   vi.fn(),
  downloadFile: vi.fn(),
  deleteFile:   vi.fn(),
  moveFile:     vi.fn(),
  createDir:    vi.fn(),
}));

// Files.vue's onMounted unconditionally loads the workspace store (CP6,
// ) — mock it here so this suite doesn't leak
// a real (token-less) client.js call.
vi.mock("../api/workspace.js", () => ({
  getWorkspaceBackend: () => Promise.resolve({
    pref: { backend: "local", onedriveFolderPath: "" },
    onedriveAvailable: false,
    onedriveStatus: "",
  }),
  setWorkspaceBackend: () => Promise.resolve({}),
  listOneDriveFolders: () => Promise.resolve({}),
}));

import Files from "../views/Files.vue";
import * as filesApiView from "../api/files.js";

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/files", component: Files },
      { path: "/",      component: { template: "<div/>" } },
    ],
  });
}

// vue-test-utils' trigger() only assigns options onto properties with a prototype setter;
// `dataTransfer` is a getter-only Event property in jsdom, so it must be dispatched manually.
function dispatchDrop(el, files) {
  const event = new Event("drop", { bubbles: true, cancelable: true });
  event.dataTransfer = { files };
  el.dispatchEvent(event);
}

describe("Files.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    useUserStore().setFromProfile({ email: "alice@example.com", sub: "alice@example.com" });
    vi.clearAllMocks();
  });

  afterEach(() => vi.restoreAllMocks());

  it("renders folders and files for the current dir, folders sorted before files", async () => {
    filesApiView.listFiles.mockResolvedValue({
      files: [
        { path: "notes.txt", size: 120, modified: "2024-01-01T00:00:00Z", isDir: false },
        { path: "zzz-dir",   size: 0,   modified: "2024-01-01T00:00:00Z", isDir: true },
        { path: "data.csv",  size: 2048, modified: "2024-01-02T00:00:00Z", isDir: false },
      ],
    });
    const router = makeRouter();
    await router.push("/files");
    const w = mount(Files, { global: { plugins: [router] } });
    await flushPromises();

    const folderRows = w.findAll("[data-testid='folder-row']");
    const fileRows = w.findAll("[data-testid='file-row']");
    expect(folderRows.length).toBe(1);
    expect(fileRows.length).toBe(2);
    // folder row(s) render before file rows in DOM order
    const allRows = w.findAll("[data-testid='folder-row'], [data-testid='file-row']");
    expect(allRows[0].attributes("data-testid")).toBe("folder-row");
    expect(w.text()).toContain("zzz-dir");
    expect(w.text()).toContain("notes.txt");
    expect(w.text()).toContain("data.csv");
  });

  it("loads the current dir shallow, sending '.' for the workspace root", async () => {
    filesApiView.listFiles.mockResolvedValue({ files: [] });
    const router = makeRouter();
    await router.push("/files");
    mount(Files, { global: { plugins: [router] } });
    await flushPromises();

    expect(filesApiView.listFiles).toHaveBeenCalledWith({ dir: ".", recursive: false });
  });

  it("entering a folder shows its children and a breadcrumb, and the breadcrumb navigates back", async () => {
    // Per-directory responses: the server now scopes the listing to the
    // requested `dir`, so root and "docs" return different children.
    filesApiView.listFiles.mockImplementation(({ dir } = {}) => {
      if (dir === "docs") {
        return Promise.resolve({
          files: [{ path: "docs/readme.txt", size: 10, modified: null, isDir: false }],
        });
      }
      return Promise.resolve({
        files: [
          { path: "docs",    size: 0, modified: null, isDir: true },
          { path: "top.txt", size: 5, modified: null, isDir: false },
        ],
      });
    });
    const router = makeRouter();
    await router.push("/files");
    const w = mount(Files, { global: { plugins: [router] } });
    await flushPromises();

    // Root shows the folder + top-level file only
    expect(w.findAll("[data-testid='folder-row']").length).toBe(1);
    expect(w.findAll("[data-testid='file-row']").length).toBe(1);

    await w.find("[data-testid='folder-row']").trigger("click");
    await flushPromises();

    // Entering the folder fetched that dir's children, not the whole tree.
    expect(filesApiView.listFiles).toHaveBeenLastCalledWith({ dir: "docs", recursive: false });

    // Inside docs/: only readme.txt, and a breadcrumb trail exists
    expect(w.findAll("[data-testid='file-row']").length).toBe(1);
    expect(w.text()).toContain("readme.txt");
    expect(w.text()).not.toContain("top.txt");
    expect(w.find("[data-testid='breadcrumb-1']").exists()).toBe(true);

    // Clicking the root crumb (index 0) navigates back
    await w.find("[data-testid='breadcrumb-0']").trigger("click");
    await flushPromises();
    expect(w.findAll("[data-testid='file-row']").length).toBe(1);
    expect(w.text()).toContain("top.txt");
  });

  // Intent (F39): the folder name is a real <button> so it's Tab-reachable and
  // Enter/Space-activatable via native button semantics, not just mouse click.
  it("folder name is a focusable button that keyboard-activates openFolder", async () => {
    filesApiView.listFiles.mockImplementation(({ dir } = {}) => {
      if (dir === "docs") {
        return Promise.resolve({ files: [] });
      }
      return Promise.resolve({
        files: [{ path: "docs", size: 0, modified: null, isDir: true }],
      });
    });
    const router = makeRouter();
    await router.push("/files");
    const w = mount(Files, { global: { plugins: [router] } });
    await flushPromises();

    const folderBtn = w.find("[data-testid='folder-row'] .file-path-btn");
    expect(folderBtn.element.tagName).toBe("BUTTON");
    expect(folderBtn.attributes("type")).toBe("button");

    // Native button click (as fired by Enter/Space) bubbles to the row's @click handler.
    await folderBtn.trigger("click");
    await flushPromises();

    expect(filesApiView.listFiles).toHaveBeenLastCalledWith({ dir: "docs", recursive: false });
  });

  // Intent (F39): only folders are interactive/focusable — a file row's name
  // element must NOT be a button (no purposeless Tab stop for the majority case).
  it("a non-directory file row's name element is not a button", async () => {
    filesApiView.listFiles.mockResolvedValue({
      files: [{ path: "notes.txt", size: 120, modified: null, isDir: false }],
    });
    const router = makeRouter();
    await router.push("/files");
    const w = mount(Files, { global: { plugins: [router] } });
    await flushPromises();

    const nameEl = w.find("[data-testid='file-row'] .file-path-btn");
    expect(nameEl.exists()).toBe(true);
    expect(nameEl.element.tagName).not.toBe("BUTTON");
  });

  it("hides dot-prefixed entries (.agent/... never rendered)", async () => {
    filesApiView.listFiles.mockResolvedValue({
      files: [
        { path: ".agent",              size: 0,   modified: null, isDir: true },
        { path: ".agent/Sessions",     size: 0,   modified: null, isDir: true },
        { path: ".agent/Sessions/x.json", size: 1, modified: null, isDir: false },
        { path: "report.txt",          size: 50,  modified: null, isDir: false },
      ],
    });
    const router = makeRouter();
    await router.push("/files");
    const w = mount(Files, { global: { plugins: [router] } });
    await flushPromises();

    expect(w.findAll("[data-testid='folder-row']").length).toBe(0);
    expect(w.findAll("[data-testid='file-row']").length).toBe(1);
    expect(w.text()).toContain("report.txt");
    expect(w.text()).not.toContain(".agent");
  });

  it("upload sends base64 to the cwd-joined path", async () => {
    filesApiView.listFiles.mockResolvedValue({
      files: [{ path: "sub", size: 0, modified: null, isDir: true }],
    });
    filesApiView.uploadFile.mockResolvedValue({ file: { path: "sub/test.txt", size: 4, modified: null, isDir: false } });
    const router = makeRouter();
    await router.push("/files");
    const w = mount(Files, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='folder-row']").trigger("click");
    await flushPromises();

    const mockFile = new File(["test"], "test.txt", { type: "text/plain" });
    await w.vm.handleFileUpload(mockFile);
    await flushPromises();
    expect(filesApiView.uploadFile).toHaveBeenCalledWith("sub/test.txt", expect.any(String));
  });

  it("rejects a file over the 10 MiB upload limit with a clear error, without uploading", async () => {
    filesApiView.listFiles.mockResolvedValue({ files: [] });
    const router = makeRouter();
    await router.push("/files");
    const w = mount(Files, { global: { plugins: [router] } });
    await flushPromises();

    const oversized = new File(["test"], "huge.bin", { type: "application/octet-stream" });
    Object.defineProperty(oversized, "size", { value: 10 * 1024 * 1024 + 1 });

    await w.vm.handleFileUpload(oversized);
    await flushPromises();

    expect(filesApiView.uploadFile).not.toHaveBeenCalled();
    expect(w.find("[data-testid='error-banner']").exists()).toBe(true);
    expect(w.text()).toContain("huge.bin");
    expect(w.text()).toContain("10 MiB");
  });

  it("still uploads a normal-size file (size guard doesn't over-reject)", async () => {
    filesApiView.listFiles.mockResolvedValue({ files: [] });
    filesApiView.uploadFile.mockResolvedValue({ file: { path: "small.txt", size: 4, modified: null, isDir: false } });
    const router = makeRouter();
    await router.push("/files");
    const w = mount(Files, { global: { plugins: [router] } });
    await flushPromises();

    const mockFile = new File(["test"], "small.txt", { type: "text/plain" });
    await w.vm.handleFileUpload(mockFile);
    await flushPromises();

    expect(filesApiView.uploadFile).toHaveBeenCalledWith("small.txt", expect.any(String));
    expect(w.find("[data-testid='error-banner']").exists()).toBe(false);
  });

  it("a mixed batch upload skips the oversize file, uploads the valid one, and keeps the oversize error visible", async () => {
    filesApiView.listFiles.mockResolvedValue({ files: [] });
    filesApiView.uploadFile.mockResolvedValue({ file: { path: "valid.txt", size: 4, modified: null, isDir: false } });
    const router = makeRouter();
    await router.push("/files");
    const w = mount(Files, { global: { plugins: [router] } });
    await flushPromises();

    const oversized = new File(["test"], "huge.bin", { type: "application/octet-stream" });
    Object.defineProperty(oversized, "size", { value: 10 * 1024 * 1024 + 1 });
    const valid = new File(["test"], "valid.txt", { type: "text/plain" });

    dispatchDrop(w.find("[data-testid='dropzone']").element, [oversized, valid]);
    await flushPromises();
    await flushPromises();

    expect(filesApiView.uploadFile).toHaveBeenCalledWith("valid.txt", expect.any(String));
    expect(filesApiView.uploadFile).not.toHaveBeenCalledWith(expect.stringContaining("huge.bin"), expect.any(String));
    expect(w.find("[data-testid='error-banner']").exists()).toBe(true);
    expect(w.text()).toContain("huge.bin");
  });

  it("a multi-file drag-drop upload does exactly one refetch of cwd, not one per file", async () => {
    filesApiView.listFiles.mockResolvedValue({ files: [] });
    filesApiView.uploadFile.mockResolvedValue({ file: {} });
    const router = makeRouter();
    await router.push("/files");
    const w = mount(Files, { global: { plugins: [router] } });
    await flushPromises();
    expect(filesApiView.listFiles).toHaveBeenCalledTimes(1); // initial load()

    const fileA = new File(["a"], "a.txt", { type: "text/plain" });
    const fileB = new File(["b"], "b.txt", { type: "text/plain" });
    dispatchDrop(w.find("[data-testid='dropzone']").element, [fileA, fileB]);
    await flushPromises();
    await flushPromises();
    await flushPromises();
    await flushPromises();

    expect(filesApiView.uploadFile).toHaveBeenCalledTimes(2);
    // Exactly one additional listFiles call (the single cwd refetch), not two.
    expect(filesApiView.listFiles).toHaveBeenCalledTimes(2);
  });

  it("a drop event with dataTransfer.files triggers upload of the dropped file(s)", async () => {
    filesApiView.listFiles.mockResolvedValue({ files: [] });
    filesApiView.uploadFile.mockResolvedValue({ file: { path: "dropped.txt", size: 4, modified: null, isDir: false } });
    const router = makeRouter();
    await router.push("/files");
    const w = mount(Files, { global: { plugins: [router] } });
    await flushPromises();

    const mockFile = new File(["test"], "dropped.txt", { type: "text/plain" });
    dispatchDrop(w.find("[data-testid='dropzone']").element, [mockFile]);
    // FileReader's readAsDataURL callback needs an extra microtask tick beyond the drop handler's own await.
    await flushPromises();
    await flushPromises();
    expect(filesApiView.uploadFile).toHaveBeenCalledWith("dropped.txt", expect.any(String));
  });

  it("dropping onto a subfolder row uploads into that subfolder and skips the cwd refetch", async () => {
    filesApiView.listFiles.mockResolvedValue({
      files: [{ path: "sub", size: 0, modified: null, isDir: true }],
    });
    filesApiView.uploadFile.mockResolvedValue({ file: {} });
    const router = makeRouter();
    await router.push("/files");
    const w = mount(Files, { global: { plugins: [router] } });
    await flushPromises();
    expect(filesApiView.listFiles).toHaveBeenCalledTimes(1); // initial load()

    const mockFile = new File(["test"], "into-sub.txt", { type: "text/plain" });
    dispatchDrop(w.find("[data-testid='folder-row']").element, [mockFile]);
    await flushPromises();
    await flushPromises();

    // uploadFiles' targetDir param (bound to the row's own path via onDrop(e, f.path))
    // routes the upload into "sub", not cwd.
    expect(filesApiView.uploadFile).toHaveBeenCalledWith("sub/into-sub.txt", expect.any(String));
    // uploadFiles only refetches when targetDir === cwd.value; a subfolder drop target
    // never matches root cwd (""), so no second listFiles call should fire.
    expect(filesApiView.listFiles).toHaveBeenCalledTimes(1);
  });

  it("new-folder input calls createDir with the cwd-joined path", async () => {
    filesApiView.listFiles.mockResolvedValue({ files: [] });
    filesApiView.createDir.mockResolvedValue({ success: true });
    const router = makeRouter();
    await router.push("/files");
    const w = mount(Files, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='new-folder-btn']").trigger("click");
    await flushPromises();
    const input = w.find("[data-testid='new-folder-input']");
    expect(input.exists()).toBe(true);
    await input.setValue("newdir");
    await input.trigger("keyup.enter");
    await flushPromises();
    expect(filesApiView.createDir).toHaveBeenCalledWith("newdir");
  });

  it("rename calls moveFile with the same-dir target", async () => {
    filesApiView.listFiles.mockResolvedValue({
      files: [{ path: "old.txt", size: 5, modified: null, isDir: false }],
    });
    filesApiView.moveFile.mockResolvedValue({ file: { path: "new.txt", size: 5, modified: null, isDir: false } });
    const router = makeRouter();
    await router.push("/files");
    const w = mount(Files, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='rename-btn-old.txt']").trigger("click");
    await flushPromises();
    const input = w.find("[data-testid='rename-input']");
    expect(input.exists()).toBe(true);
    await input.setValue("new.txt");
    await input.trigger("keyup.enter");
    await flushPromises();
    expect(filesApiView.moveFile).toHaveBeenCalledWith("old.txt", "new.txt");
  });

  it("blurring the rename input confirms the rename like Enter", async () => {
    filesApiView.listFiles.mockResolvedValue({
      files: [{ path: "old.txt", size: 5, modified: null, isDir: false }],
    });
    filesApiView.moveFile.mockResolvedValue({ file: { path: "new.txt", size: 5, modified: null, isDir: false } });
    const router = makeRouter();
    await router.push("/files");
    const w = mount(Files, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='rename-btn-old.txt']").trigger("click");
    await flushPromises();
    const input = w.find("[data-testid='rename-input']");
    await input.setValue("new.txt");
    await input.trigger("blur");
    await flushPromises();
    expect(filesApiView.moveFile).toHaveBeenCalledWith("old.txt", "new.txt");
  });

  it("Escape cancels the rename without moving, even after editing the name", async () => {
    filesApiView.listFiles.mockResolvedValue({
      files: [{ path: "old.txt", size: 5, modified: null, isDir: false }],
    });
    const router = makeRouter();
    await router.push("/files");
    const w = mount(Files, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='rename-btn-old.txt']").trigger("click");
    await flushPromises();
    const input = w.find("[data-testid='rename-input']");
    await input.setValue("new.txt");
    // Esc nulls renamingPath (cancel); the input then unmounts, firing blur —
    // the confirmRename guard must make that trailing blur a no-op.
    await input.trigger("keyup.esc");
    await input.trigger("blur");
    await flushPromises();
    expect(filesApiView.moveFile).not.toHaveBeenCalled();
  });

  it("delete requires confirmation before calling deleteFile (works for a folder row too)", async () => {
    filesApiView.listFiles.mockResolvedValue({ files: [{ path: "adir", size: 0, modified: null, isDir: true }] });
    filesApiView.deleteFile.mockResolvedValue({ success: true });
    const router = makeRouter();
    await router.push("/files");
    const w = mount(Files, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='delete-adir']").trigger("click");
    await flushPromises();
    // First click only opens the confirmation — no API call yet.
    expect(filesApiView.deleteFile).not.toHaveBeenCalled();
    expect(w.find("[data-testid='confirm-delete']").exists()).toBe(true);

    await w.find("[data-testid='confirm-delete']").trigger("click");
    await flushPromises();
    expect(filesApiView.deleteFile).toHaveBeenCalledWith("adir");
  });

  it("a rejected delete keeps the confirmation modal open and shows the error", async () => {
    filesApiView.listFiles.mockResolvedValue({ files: [{ path: "adir", size: 0, modified: null, isDir: true }] });
    filesApiView.deleteFile.mockRejectedValue(new Error("Delete boom"));
    const router = makeRouter();
    await router.push("/files");
    const w = mount(Files, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='delete-adir']").trigger("click");
    await flushPromises();
    await w.find("[data-testid='confirm-delete']").trigger("click");
    await flushPromises();

    expect(filesApiView.deleteFile).toHaveBeenCalledWith("adir");
    expect(w.find("[data-testid='confirm-delete']").exists()).toBe(true);
    expect(w.find("[data-testid='error-banner']").text()).toContain("Delete boom");
    // A rejected delete recovers by refetching the current dir (Files.vue confirmDelete's
    // catch calls refetchCwd()) — pins the second listFiles call beyond the initial load().
    expect(filesApiView.listFiles).toHaveBeenCalledTimes(2);
    expect(filesApiView.listFiles).toHaveBeenLastCalledWith({ dir: ".", recursive: false });
  });

  it("a rejected rename shows the error and refetches the current dir", async () => {
    filesApiView.listFiles.mockResolvedValue({
      files: [{ path: "old.txt", size: 5, modified: null, isDir: false }],
    });
    filesApiView.moveFile.mockRejectedValue(new Error("Rename boom"));
    const router = makeRouter();
    await router.push("/files");
    const w = mount(Files, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='rename-btn-old.txt']").trigger("click");
    await flushPromises();
    const input = w.find("[data-testid='rename-input']");
    await input.setValue("new.txt");
    await input.trigger("keyup.enter");
    await flushPromises();

    expect(filesApiView.moveFile).toHaveBeenCalledWith("old.txt", "new.txt");
    expect(w.find("[data-testid='error-banner']").text()).toContain("Rename boom");
    // A rejected rename recovers by refetching the current dir (Files.vue confirmRename's
    // catch calls refetchCwd()) — pins the second listFiles call beyond the initial load().
    expect(filesApiView.listFiles).toHaveBeenCalledTimes(2);
    expect(filesApiView.listFiles).toHaveBeenLastCalledWith({ dir: ".", recursive: false });
  });

  it("cancelling the delete confirmation does not call deleteFile", async () => {
    filesApiView.listFiles.mockResolvedValue({ files: [{ path: "adir", size: 0, modified: null, isDir: true }] });
    const router = makeRouter();
    await router.push("/files");
    const w = mount(Files, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='delete-adir']").trigger("click");
    await flushPromises();
    await w.find("[data-testid='cancel-delete']").trigger("click");
    await flushPromises();
    expect(filesApiView.deleteFile).not.toHaveBeenCalled();
    expect(w.find("[data-testid='confirm-delete']").exists()).toBe(false);
  });

  it("a confirmed delete patches entries locally without a second full listFiles call", async () => {
    filesApiView.listFiles.mockResolvedValue({
      files: [
        { path: "adir",     size: 0, modified: null, isDir: true },
        { path: "keep.txt", size: 3, modified: null, isDir: false },
      ],
    });
    filesApiView.deleteFile.mockResolvedValue({ success: true });
    const router = makeRouter();
    await router.push("/files");
    const w = mount(Files, { global: { plugins: [router] } });
    await flushPromises();
    expect(filesApiView.listFiles).toHaveBeenCalledTimes(1);

    await w.find("[data-testid='delete-adir']").trigger("click");
    await flushPromises();
    await w.find("[data-testid='confirm-delete']").trigger("click");
    await flushPromises();

    expect(filesApiView.deleteFile).toHaveBeenCalledWith("adir");
    // No refetch of the whole tree — the deleted entry is patched out locally.
    expect(filesApiView.listFiles).toHaveBeenCalledTimes(1);
    expect(w.findAll("[data-testid='folder-row']").length).toBe(0);
    expect(w.text()).toContain("keep.txt");
  });

  it("renders a loading state (not the empty message) while the initial fetch is pending", async () => {
    let resolveList;
    filesApiView.listFiles.mockReturnValue(new Promise((resolve) => { resolveList = resolve; }));
    const router = makeRouter();
    await router.push("/files");
    const w = mount(Files, { global: { plugins: [router] } });
    await flushPromises();

    expect(w.find("[data-testid='loading']").exists()).toBe(true);
    expect(w.text()).not.toContain("This folder is empty.");

    resolveList({ files: [] });
    await flushPromises();

    expect(w.find("[data-testid='loading']").exists()).toBe(false);
    expect(w.text()).toContain("This folder is empty.");
  });

  it("shows error banner on API error", async () => {
    filesApiView.listFiles.mockRejectedValue(new Error("server error"));
    const router = makeRouter();
    await router.push("/files");
    const w = mount(Files, { global: { plugins: [router] } });
    await flushPromises();
    expect(w.find("[data-testid='error-banner']").exists()).toBe(true);
  });
});
