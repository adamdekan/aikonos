// Files.vue — CP6 active-backend indicator + reconnect banner
//. api/files.js and api/workspace.js are
// both mocked so no server is needed.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";
import { useWorkspaceStore } from "../store/workspace.js";

vi.mock("../api/files.js", () => ({
  listFiles:    vi.fn(),
  uploadFile:   vi.fn(),
  downloadFile: vi.fn(),
  deleteFile:   vi.fn(),
  moveFile:     vi.fn(),
  createDir:    vi.fn(),
}));

vi.mock("../api/workspace.js", () => ({
  getWorkspaceBackend: vi.fn(),
  setWorkspaceBackend: vi.fn(),
  listOneDriveFolders: vi.fn(),
}));

import Files from "../views/Files.vue";
import * as filesApiView from "../api/files.js";
import * as workspaceApi from "../api/workspace.js";

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/files", component: Files },
      { path: "/",      component: { template: "<div/>" } },
    ],
  });
}

function seedWorkspace({ backend = "local", onedriveFolderPath = "", onedriveStatus = "", loaded = true } = {}) {
  const store = useWorkspaceStore();
  store.backend = backend;
  store.onedriveFolderPath = onedriveFolderPath;
  store.onedriveStatus = onedriveStatus;
  store.loaded = loaded;
  return store;
}

async function mountFiles() {
  const router = makeRouter();
  await router.push("/files");
  const w = mount(Files, { global: { plugins: [router] } });
  await flushPromises();
  return w;
}

describe("Files.vue — backend indicator + reconnect banner", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    useUserStore().setFromProfile({ email: "alice@example.com", sub: "alice@example.com" });
    vi.clearAllMocks();
    filesApiView.listFiles.mockResolvedValue({ files: [] });
    workspaceApi.getWorkspaceBackend.mockResolvedValue({
      pref: { backend: "local", onedriveFolderPath: "" },
      onedriveAvailable: true,
      onedriveStatus: "",
    });
  });

  afterEach(() => vi.restoreAllMocks());

  it("shows 'Local workspace' when backend is local", async () => {
    seedWorkspace({ backend: "local" });
    const w = await mountFiles();
    expect(w.find("[data-testid='backend-indicator']").text()).toBe("Local workspace");
  });

  it("shows 'OneDrive · /<path>' when backend is onedrive", async () => {
    seedWorkspace({ backend: "onedrive", onedriveFolderPath: "Apps/Aikonos" });
    const w = await mountFiles();
    expect(w.find("[data-testid='backend-indicator']").text()).toBe("OneDrive · /Apps/Aikonos");
  });

  it("does not render the indicator before the workspace store has loaded", async () => {
    // Never-resolving load() — proves the indicator is gated on `loaded`,
    // not just rendered unconditionally once mounted.
    workspaceApi.getWorkspaceBackend.mockImplementation(() => new Promise(() => {}));
    const w = await mountFiles();
    expect(w.find("[data-testid='backend-indicator']").exists()).toBe(false);
  });

  it("shows the reconnect banner exactly when backend=onedrive and status=reconnect_needed", async () => {
    seedWorkspace({ backend: "onedrive", onedriveFolderPath: "Apps/Aikonos", onedriveStatus: "reconnect_needed" });
    const w = await mountFiles();
    const banner = w.find("[data-testid='reconnect-banner']");
    expect(banner.exists()).toBe(true);
    expect(banner.text()).toContain("reconnect");
  });

  it("no reconnect banner when status=reconnect_needed but backend is local", async () => {
    seedWorkspace({ backend: "local", onedriveStatus: "reconnect_needed" });
    const w = await mountFiles();
    expect(w.find("[data-testid='reconnect-banner']").exists()).toBe(false);
  });

  it("no reconnect banner when backend=onedrive but status is connected", async () => {
    seedWorkspace({ backend: "onedrive", onedriveFolderPath: "Apps/Aikonos", onedriveStatus: "connected" });
    const w = await mountFiles();
    expect(w.find("[data-testid='reconnect-banner']").exists()).toBe(false);
  });
});
