// Tests for api/workspace.js in isolation (no view rendering).
// Field names/shapes must match agent-gateway/src/routes/workspace-prefs.ts exactly.
import { describe, it, expect, vi, beforeEach } from "vitest";

vi.mock("../api/client.js", () => ({
  get: vi.fn(),
  put: vi.fn(),
}));

import * as clientMod from "../api/client.js";
import { getWorkspaceBackend, setWorkspaceBackend, listOneDriveFolders } from "../api/workspace.js";

describe("api/workspace.js", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("getWorkspaceBackend calls GET /workspace/backend", async () => {
    clientMod.get.mockResolvedValue({
      pref: { backend: "local", onedriveFolderPath: "" },
      onedriveAvailable: false,
      onedriveStatus: "",
    });
    await getWorkspaceBackend();
    expect(clientMod.get).toHaveBeenCalledWith("/workspace/backend");
  });

  it("setWorkspaceBackend PUTs {backend, onedriveFolderPath} to /workspace/backend", async () => {
    clientMod.put.mockResolvedValue({ pref: { backend: "onedrive", onedriveFolderPath: "Apps/aikonOS" } });
    await setWorkspaceBackend({ backend: "onedrive", onedriveFolderPath: "Apps/aikonOS" });
    expect(clientMod.put).toHaveBeenCalledWith("/workspace/backend", {
      body: { backend: "onedrive", onedriveFolderPath: "Apps/aikonOS" },
    });
  });

  it("listOneDriveFolders GETs /workspace/onedrive/folders?dir=<encoded>", async () => {
    clientMod.get.mockResolvedValue({ folders: [] });
    await listOneDriveFolders("Apps/aikonOS");
    expect(clientMod.get).toHaveBeenCalledWith("/workspace/onedrive/folders?dir=Apps%2FaikonOS");
  });

  it("listOneDriveFolders defaults to the drive root (empty dir)", async () => {
    clientMod.get.mockResolvedValue({ folders: [] });
    await listOneDriveFolders();
    expect(clientMod.get).toHaveBeenCalledWith("/workspace/onedrive/folders?dir=");
  });
});
