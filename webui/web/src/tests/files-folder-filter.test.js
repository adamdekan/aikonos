// F63 (CP3): Files.vue current-directory filter — case-insensitive substring over
// the current dir's entries; cleared on navigation (a folder filter rarely means
// the next folder). Empty filter must render identically to today (parity pin).
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";

vi.mock("../api/files.js", () => ({
  listFiles: vi.fn(),
  uploadFile: vi.fn(),
  downloadFile: vi.fn(),
  deleteFile: vi.fn(),
  moveFile: vi.fn(),
  createDir: vi.fn(),
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
      { path: "/", component: { template: "<div/>" } },
    ],
  });
}

describe("Files.vue — current-directory filter", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    useUserStore().setFromProfile({ email: "alice@example.com", sub: "alice@example.com" });
    vi.clearAllMocks();
  });

  afterEach(() => vi.restoreAllMocks());

  it("empty filter renders identically to today (parity)", async () => {
    filesApiView.listFiles.mockResolvedValue({
      files: [
        { path: "notes.txt", size: 120, modified: null, isDir: false },
        { path: "zzz-dir", size: 0, modified: null, isDir: true },
      ],
    });
    const router = makeRouter();
    await router.push("/files");
    const w = mount(Files, { global: { plugins: [router] } });
    await flushPromises();

    expect(w.findAll("[data-testid='folder-row']").length).toBe(1);
    expect(w.findAll("[data-testid='file-row']").length).toBe(1);
  });

  it("filters entries by name case-insensitively", async () => {
    filesApiView.listFiles.mockResolvedValue({
      files: [
        { path: "notes.txt", size: 120, modified: null, isDir: false },
        { path: "report.csv", size: 20, modified: null, isDir: false },
      ],
    });
    const router = makeRouter();
    await router.push("/files");
    const w = mount(Files, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='folder-filter-input']").setValue("NOTES");
    expect(w.text()).toContain("notes.txt");
    expect(w.text()).not.toContain("report.csv");
  });

  it("clears the filter on navigation into a folder", async () => {
    filesApiView.listFiles.mockImplementation(({ dir } = {}) => {
      if (dir === "sub") {
        return Promise.resolve({
          files: [{ path: "sub/inner.txt", size: 1, modified: null, isDir: false }],
        });
      }
      return Promise.resolve({
        files: [
          { path: "sub", size: 0, modified: null, isDir: true },
          { path: "other.txt", size: 1, modified: null, isDir: false },
        ],
      });
    });
    const router = makeRouter();
    await router.push("/files");
    const w = mount(Files, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='folder-filter-input']").setValue("sub");
    expect(w.text()).toContain("sub");
    expect(w.text()).not.toContain("other.txt");

    await w.find("[data-testid='folder-row']").trigger("click");
    await flushPromises();

    // Filter cleared on navigation — inner.txt visible even though it doesn't match "sub".
    expect(w.find("[data-testid='folder-filter-input']").element.value).toBe("");
    expect(w.text()).toContain("inner.txt");
  });
});
