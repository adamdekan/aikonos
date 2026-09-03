// Composer.vue — CP6 working-folder control.
// Entirely hidden when OneDrive isn't configured/available (dev/Keycloak
// invariant) or the workspace store hasn't loaded/errored; otherwise shows the
// active backend and wires the Local/OneDrive menu to workspaceStore.setBackend.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import Composer from "../components/Composer.vue";
import WorkspaceFolderPicker from "../components/WorkspaceFolderPicker.vue";
import { useWorkspaceStore } from "../store/workspace.js";

vi.mock("../api/workspace.js", () => ({
  getWorkspaceBackend: vi.fn(),
  setWorkspaceBackend: vi.fn(),
  listOneDriveFolders: vi.fn(),
}));

// setBackend refreshes the shared discovery store (item 2's spec invariant) —
// mock its api dependencies too so that refresh resolves cleanly instead of
// hitting the (unmocked, token-less) real client in this component test.
vi.mock("../api/admin.js", () => ({
  listUserSkillBundles: vi.fn().mockResolvedValue({ bundles: [] }),
}));
vi.mock("../api/delegation.js", () => ({
  listDelegatableUsers: vi.fn().mockResolvedValue({ users: [], groups: [] }),
}));
vi.mock("../api/files.js", () => ({
  listFiles:  vi.fn().mockResolvedValue({ files: [] }),
  uploadFile: vi.fn(),
  createDir:  vi.fn(),
}));

import * as workspaceApi from "../api/workspace.js";

function mountComposer(props = {}) {
  return mount(Composer, {
    props: { modelValue: "", ...props },
    attachTo: document.body,
  });
}

function seedWorkspace({
  backend = "local",
  onedriveFolderPath = "",
  onedriveAvailable = true,
  loaded = true,
  error = null,
} = {}) {
  const store = useWorkspaceStore();
  store.backend = backend;
  store.onedriveFolderPath = onedriveFolderPath;
  store.onedriveAvailable = onedriveAvailable;
  store.loaded = loaded;
  store.error = error;
  return store;
}

describe("Composer.vue — working-folder control", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    vi.clearAllMocks();
    // Default healthy load() response so any in-flight onMounted load() (fired
    // when a test seeds loaded=false) never throws on an unmocked resolution.
    workspaceApi.getWorkspaceBackend.mockResolvedValue({
      pref: { backend: "local", onedriveFolderPath: "" },
      onedriveAvailable: true,
      onedriveStatus: "",
    });
    workspaceApi.listOneDriveFolders.mockResolvedValue({ folders: [] });
  });

  it("is hidden when onedriveAvailable is false", () => {
    seedWorkspace({ onedriveAvailable: false });
    const w = mountComposer();
    expect(w.find("[data-testid='workspace-control']").exists()).toBe(false);
  });

  it("is hidden when the workspace store has not loaded yet", () => {
    seedWorkspace({ loaded: false });
    const w = mountComposer();
    expect(w.find("[data-testid='workspace-control']").exists()).toBe(false);
  });

  it("is hidden when the workspace store errored", () => {
    seedWorkspace({ error: "network down" });
    const w = mountComposer();
    expect(w.find("[data-testid='workspace-control']").exists()).toBe(false);
  });

  it("shows 'Local workspace' when backend is local", () => {
    seedWorkspace({ backend: "local" });
    const w = mountComposer();
    expect(w.find("[data-testid='workspace-control']").exists()).toBe(true);
    expect(w.find("[data-testid='workspace-control-btn']").text()).toBe("Working folder: Local ▾");
  });

  it("shows the OneDrive path when backend is onedrive", () => {
    seedWorkspace({ backend: "onedrive", onedriveFolderPath: "Apps/Aikonos" });
    const w = mountComposer();
    const label = w.find("[data-testid='workspace-control-btn']").text();
    expect(label).toContain("OneDrive");
    expect(label).toContain("Apps/Aikonos");
  });

  it("selecting 'Local workspace' calls setBackend with backend:local", async () => {
    workspaceApi.setWorkspaceBackend.mockResolvedValue({
      pref: { backend: "local", onedriveFolderPath: "" },
    });
    seedWorkspace({ backend: "onedrive", onedriveFolderPath: "Apps/Aikonos" });
    const w = mountComposer();

    await w.find("[data-testid='workspace-control-btn']").trigger("click");
    await w.find("[data-testid='workspace-menu-local']").trigger("click");
    await flushPromises();

    expect(workspaceApi.setWorkspaceBackend).toHaveBeenCalledWith({
      backend: "local",
      onedriveFolderPath: "",
    });
  });

  it("clicking outside the workspace control closes the menu", async () => {
    seedWorkspace({ backend: "local" });
    const w = mountComposer();

    await w.find("[data-testid='workspace-control-btn']").trigger("click");
    expect(w.find("[data-testid='workspace-menu']").exists()).toBe(true);

    // Dispatch a real document click outside the composer entirely — proves
    // the listener isn't scoped to (and doesn't rely on) the component tree.
    document.body.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    await flushPromises();

    expect(w.find("[data-testid='workspace-menu']").exists()).toBe(false);
  });

  it("'OneDrive folder…' opens the folder picker", async () => {
    seedWorkspace({ backend: "local" });
    const w = mountComposer();

    await w.find("[data-testid='workspace-control-btn']").trigger("click");
    await w.find("[data-testid='workspace-menu-onedrive']").trigger("click");

    expect(w.findComponent(WorkspaceFolderPicker).props("visible")).toBe(true);
  });

  it("picker select triggers setBackend with the chosen path", async () => {
    workspaceApi.setWorkspaceBackend.mockResolvedValue({
      pref: { backend: "onedrive", onedriveFolderPath: "Reports/2026" },
    });
    seedWorkspace({ backend: "local" });
    const w = mountComposer();

    await w.find("[data-testid='workspace-control-btn']").trigger("click");
    await w.find("[data-testid='workspace-menu-onedrive']").trigger("click");

    const picker = w.findComponent(WorkspaceFolderPicker);
    picker.vm.$emit("select", "Reports/2026");
    await flushPromises();

    expect(workspaceApi.setWorkspaceBackend).toHaveBeenCalledWith({
      backend: "onedrive",
      onedriveFolderPath: "Reports/2026",
    });
  });
});
