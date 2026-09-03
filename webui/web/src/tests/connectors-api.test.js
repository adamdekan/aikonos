// Tests for api/connectors.js in isolation (no view rendering).
import { describe, it, expect, vi, beforeEach } from "vitest";

vi.mock("../api/client.js", () => ({
  get:   vi.fn(),
  post:  vi.fn(),
  del:   vi.fn(),
  patch: vi.fn(),
}));

import * as clientMod from "../api/client.js";
import { listConnectors, beginConnectorAuth, completeConnectorAuth, revokeConnector } from "../api/connectors.js";

describe("api/connectors.js", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("listConnectors calls GET /connectors", async () => {
    clientMod.get.mockResolvedValue({ connectors: [] });
    await listConnectors();
    expect(clientMod.get).toHaveBeenCalledWith("/connectors");
  });

  it("beginConnectorAuth posts provider to /connectors/begin", async () => {
    clientMod.post.mockResolvedValue({ authorizeUrl: "https://accounts.google.com/oauth", state: "abc" });
    await beginConnectorAuth("google_drive");
    expect(clientMod.post).toHaveBeenCalledWith("/connectors/begin", { body: { provider: "google_drive" } });
  });

  it("completeConnectorAuth posts code+state to /connectors/complete", async () => {
    clientMod.post.mockResolvedValue({ connectorId: "c1", status: "active", scopes: [] });
    await completeConnectorAuth("CODE123", "STATE456");
    expect(clientMod.post).toHaveBeenCalledWith("/connectors/complete", { body: { code: "CODE123", state: "STATE456" } });
  });

  it("revokeConnector posts to /connectors/:id/revoke", async () => {
    clientMod.post.mockResolvedValue({ success: true });
    await revokeConnector("conn-abc");
    expect(clientMod.post).toHaveBeenCalledWith("/connectors/conn-abc/revoke");
  });
});
