// WorkspaceFolderPicker.vue — CP6 folder-browsing modal.
// Mirrors RunWorkflowModal's modal pattern: props {visible}, emits close/select.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";

vi.mock("../api/workspace.js", () => ({
  listOneDriveFolders: vi.fn(),
}));

import * as workspaceApi from "../api/workspace.js";
import WorkspaceFolderPicker from "../components/WorkspaceFolderPicker.vue";

function makePicker(props = {}) {
  return mount(WorkspaceFolderPicker, {
    props: { visible: true, ...props },
    global: { plugins: [createPinia()] },
  });
}

describe("WorkspaceFolderPicker.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    vi.clearAllMocks();
  });

  it("does not render when visible=false", () => {
    const w = makePicker({ visible: false });
    expect(w.find(".picker-modal").exists()).toBe(false);
  });

  it("loads drive-root folders on open", async () => {
    workspaceApi.listOneDriveFolders.mockResolvedValue({
      folders: [
        { name: "Reports", path: "Reports" },
        { name: "Apps", path: "Apps" },
      ],
    });
    const w = makePicker();
    await flushPromises();

    expect(workspaceApi.listOneDriveFolders).toHaveBeenCalledWith("");
    const rows = w.findAll("[data-testid='picker-folder']");
    expect(rows.length).toBe(2);
    expect(w.text()).toContain("Reports");
    expect(w.text()).toContain("Apps");
  });

  it("descending into a folder re-queries with that folder's path", async () => {
    workspaceApi.listOneDriveFolders.mockResolvedValueOnce({
      folders: [{ name: "Apps", path: "Apps" }],
    });
    const w = makePicker();
    await flushPromises();

    workspaceApi.listOneDriveFolders.mockResolvedValueOnce({
      folders: [{ name: "Aikonos", path: "Apps/Aikonos" }],
    });
    await w.find("[data-testid='picker-folder']").trigger("click");
    await flushPromises();

    expect(workspaceApi.listOneDriveFolders).toHaveBeenCalledWith("Apps");
    expect(w.text()).toContain("Aikonos");
  });

  it("breadcrumb ascend re-queries with the crumb's path", async () => {
    workspaceApi.listOneDriveFolders.mockResolvedValueOnce({
      folders: [{ name: "Apps", path: "Apps" }],
    });
    const w = makePicker();
    await flushPromises();

    workspaceApi.listOneDriveFolders.mockResolvedValueOnce({
      folders: [{ name: "Aikonos", path: "Apps/Aikonos" }],
    });
    await w.find("[data-testid='picker-folder']").trigger("click");
    await flushPromises();

    workspaceApi.listOneDriveFolders.mockResolvedValueOnce({
      folders: [{ name: "Apps", path: "Apps" }],
    });
    // crumb 0 = drive root
    await w.find("[data-testid='picker-breadcrumb-0']").trigger("click");
    await flushPromises();

    expect(workspaceApi.listOneDriveFolders).toHaveBeenCalledWith("");
  });

  it("confirm emits select with the currently-browsed path", async () => {
    workspaceApi.listOneDriveFolders.mockResolvedValueOnce({
      folders: [{ name: "Apps", path: "Apps" }],
    });
    const w = makePicker();
    await flushPromises();

    workspaceApi.listOneDriveFolders.mockResolvedValueOnce({ folders: [] });
    await w.find("[data-testid='picker-folder']").trigger("click");
    await flushPromises();

    await w.find("[data-testid='picker-confirm-btn']").trigger("click");
    expect(w.emitted("select")).toBeTruthy();
    expect(w.emitted("select")[0]).toEqual(["Apps"]);
  });

  it("cancel emits close", async () => {
    workspaceApi.listOneDriveFolders.mockResolvedValue({ folders: [] });
    const w = makePicker();
    await flushPromises();

    await w.find("[data-testid='picker-cancel-btn']").trigger("click");
    expect(w.emitted("close")).toBeTruthy();
  });

  it("Escape (document-level, focus outside the picker) emits close", async () => {
    workspaceApi.listOneDriveFolders.mockResolvedValue({ folders: [] });
    const w = makePicker();
    await flushPromises();

    // Nothing inside the picker is focused — dispatch on document, as a real
    // Esc keypress would arrive, to prove the listener isn't backdrop-scoped.
    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape" }));
    await flushPromises();

    expect(w.emitted("close")).toBeTruthy();
  });

  it("shows an ErrorBanner with a retry action on api failure", async () => {
    workspaceApi.listOneDriveFolders.mockRejectedValueOnce(new Error("boom"));
    const w = makePicker();
    await flushPromises();

    const banner = w.find("[data-testid='error-banner']");
    expect(banner.exists()).toBe(true);
    expect(banner.text()).toContain("boom");

    workspaceApi.listOneDriveFolders.mockResolvedValueOnce({ folders: [{ name: "Apps", path: "Apps" }] });
    await w.find("[data-testid='picker-retry-btn']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='error-banner']").exists()).toBe(false);
    expect(w.text()).toContain("Apps");
  });
});
