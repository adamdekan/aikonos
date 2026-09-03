// Tests for views/Connections.vue — tenant-managed OneDrive rendering
//: a managed entry shows a badge
// instead of Revoke, and an omitted provider means no Connect button.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";

vi.mock("../api/connectors.js", () => ({
  listConnectors: vi.fn(),
  listConnectorProviders: vi.fn(),
  beginConnectorAuth: vi.fn(),
  revokeConnector: vi.fn(),
}));

import Connections from "../views/Connections.vue";
import * as connectorsApi from "../api/connectors.js";

async function mountView() {
  const w = mount(Connections);
  await flushPromises();
  return w;
}

describe("Connections.vue — tenant-managed OneDrive", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });
  afterEach(() => {
    document.body.innerHTML = "";
  });

  it("shows a managed badge and hides Revoke for a managed onedrive entry", async () => {
    connectorsApi.listConnectors.mockResolvedValue({
      connectors: [{ connectorId: "onedrive", provider: 2, status: "connected", managed: true }],
    });
    connectorsApi.listConnectorProviders.mockResolvedValue({ providers: [] });

    const w = await mountView();

    expect(w.find("[data-testid='managed-badge']").exists()).toBe(true);
    expect(w.find("[data-testid='revoke-onedrive']").exists()).toBe(false);
  });

  it("shows Revoke and no badge for an unmanaged connector", async () => {
    connectorsApi.listConnectors.mockResolvedValue({
      connectors: [{ connectorId: "gdrive", provider: 1, status: "connected", managed: false }],
    });
    connectorsApi.listConnectorProviders.mockResolvedValue({ providers: [] });

    const w = await mountView();

    expect(w.find("[data-testid='managed-badge']").exists()).toBe(false);
    expect(w.find("[data-testid='revoke-gdrive']").exists()).toBe(true);
  });

  it("omits the Connect button for onedrive when the provider list excludes it", async () => {
    connectorsApi.listConnectors.mockResolvedValue({ connectors: [] });
    connectorsApi.listConnectorProviders.mockResolvedValue({
      providers: [{ key: "google_drive", displayName: "Google Drive" }],
    });

    const w = await mountView();

    expect(w.find("[data-testid='connect-onedrive']").exists()).toBe(false);
    expect(w.find("[data-testid='connect-google_drive']").exists()).toBe(true);
  });
});
