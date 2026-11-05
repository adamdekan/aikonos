// Pinia store for the workspace-backend preference (local vs OneDrive working
// folder) — CP6, . Mirrors discovery.js's
// load-once/refresh shape; setBackend additionally refreshes discovery so the
// #-mention file palette repopulates from the newly-active backend.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { createPinia, setActivePinia } from "pinia";

vi.mock("../api/workspace.js", () => ({
  getWorkspaceBackend: vi.fn(),
  setWorkspaceBackend: vi.fn(),
  listOneDriveFolders: vi.fn(),
}));
vi.mock("../api/admin.js", () => ({
  listUserSkillBundles: vi.fn().mockResolvedValue({ bundles: [] }),
}));
vi.mock("../api/delegation.js", () => ({
  listDelegatableUsers: vi.fn().mockResolvedValue({ users: [], groups: [] }),
}));
vi.mock("../api/files.js", () => ({
  listFiles: vi.fn().mockResolvedValue({ files: [] }),
}));

import * as workspaceApi from "../api/workspace.js";
import { useWorkspaceStore } from "../store/workspace.js";
import { useDiscoveryStore } from "../store/discovery.js";

describe("workspace store", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    vi.clearAllMocks();
  });

  it("load() populates state from the mocked api", async () => {
    workspaceApi.getWorkspaceBackend.mockResolvedValue({
      pref: { backend: "onedrive", onedriveFolderPath: "Apps/Aikonos" },
      onedriveAvailable: true,
      onedriveStatus: "connected",
    });
    const store = useWorkspaceStore();
    await store.load();

    expect(store.backend).toBe("onedrive");
    expect(store.onedriveFolderPath).toBe("Apps/Aikonos");
    expect(store.onedriveAvailable).toBe(true);
    expect(store.onedriveStatus).toBe("connected");
    expect(store.loaded).toBe(true);
    expect(store.error).toBeNull();
  });

  it("a second load() is a no-op — the api is called exactly once", async () => {
    workspaceApi.getWorkspaceBackend.mockResolvedValue({
      pref: { backend: "local", onedriveFolderPath: "" },
      onedriveAvailable: false,
      onedriveStatus: "",
    });
    const store = useWorkspaceStore();
    await store.load();
    await store.load();
    expect(workspaceApi.getWorkspaceBackend).toHaveBeenCalledTimes(1);
  });

  it("load() failure records the error and still marks loaded (non-fatal degradation)", async () => {
    workspaceApi.getWorkspaceBackend.mockRejectedValue(new Error("network down"));
    const store = useWorkspaceStore();
    await store.load();

    expect(store.error).toBe("network down");
    expect(store.loaded).toBe(true);
  });

  it("load() treats a 403 ({forbidden:true}) as unavailable — no throw, no console error path", async () => {
    workspaceApi.getWorkspaceBackend.mockResolvedValue({ forbidden: true, error: "not permitted" });
    const store = useWorkspaceStore();
    await expect(store.load()).resolves.toBeUndefined();

    expect(store.onedriveAvailable).toBe(false);
    expect(store.error).toBe("not permitted");
    expect(store.loaded).toBe(true);
  });

  it("refresh() re-fetches even after a prior load()", async () => {
    workspaceApi.getWorkspaceBackend.mockResolvedValue({
      pref: { backend: "local", onedriveFolderPath: "" },
      onedriveAvailable: false,
      onedriveStatus: "",
    });
    const store = useWorkspaceStore();
    await store.load();
    await store.refresh();
    expect(workspaceApi.getWorkspaceBackend).toHaveBeenCalledTimes(2);
  });

  it("setBackend PUTs, updates state from the response, and refreshes discovery", async () => {
    workspaceApi.setWorkspaceBackend.mockResolvedValue({
      pref: { backend: "onedrive", onedriveFolderPath: "Reports" },
    });
    const store = useWorkspaceStore();
    const discovery = useDiscoveryStore();
    const refreshSpy = vi.spyOn(discovery, "refresh");

    await store.setBackend({ backend: "onedrive", onedriveFolderPath: "Reports" });

    expect(workspaceApi.setWorkspaceBackend).toHaveBeenCalledWith({
      backend: "onedrive",
      onedriveFolderPath: "Reports",
    });
    expect(store.backend).toBe("onedrive");
    expect(store.onedriveFolderPath).toBe("Reports");
    expect(refreshSpy).toHaveBeenCalledOnce();
  });

  it("setBackend defaults onedriveFolderPath to empty string when switching to local", async () => {
    workspaceApi.setWorkspaceBackend.mockResolvedValue({
      pref: { backend: "local", onedriveFolderPath: "" },
    });
    const store = useWorkspaceStore();
    await store.setBackend({ backend: "local" });

    expect(workspaceApi.setWorkspaceBackend).toHaveBeenCalledWith({
      backend: "local",
      onedriveFolderPath: "",
    });
  });

  it("setBackend failure rethrows and leaves prior state untouched", async () => {
    workspaceApi.getWorkspaceBackend.mockResolvedValue({
      pref: { backend: "local", onedriveFolderPath: "" },
      onedriveAvailable: true,
      onedriveStatus: "",
    });
    const store = useWorkspaceStore();
    await store.load();

    workspaceApi.setWorkspaceBackend.mockRejectedValue(new Error("write failed"));
    await expect(
      store.setBackend({ backend: "onedrive", onedriveFolderPath: "X" }),
    ).rejects.toThrow("write failed");

    expect(store.backend).toBe("local");
    expect(store.onedriveFolderPath).toBe("");
  });

  it("setBackend throws on a 403 ({forbidden:true}) — a mutation must never silently no-op", async () => {
    workspaceApi.getWorkspaceBackend.mockResolvedValue({
      pref: { backend: "local", onedriveFolderPath: "" },
      onedriveAvailable: true,
      onedriveStatus: "",
    });
    const store = useWorkspaceStore();
    await store.load();

    workspaceApi.setWorkspaceBackend.mockResolvedValue({ forbidden: true, error: "not permitted" });
    const discovery = useDiscoveryStore();
    const refreshSpy = vi.spyOn(discovery, "refresh");

    await expect(
      store.setBackend({ backend: "onedrive", onedriveFolderPath: "X" }),
    ).rejects.toThrow("not permitted");

    expect(store.backend).toBe("local");
    expect(store.onedriveFolderPath).toBe("");
    expect(refreshSpy).not.toHaveBeenCalled();
  });
});
