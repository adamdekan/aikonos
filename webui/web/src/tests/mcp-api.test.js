// Tests for MCP CRUD calls in api/admin.js (CP7).
import { describe, it, expect, vi, beforeEach } from "vitest";

vi.mock("../api/client.js", () => ({
  get:   vi.fn(),
  post:  vi.fn(),
  patch: vi.fn(),
  del:   vi.fn(),
}));

import * as clientMod from "../api/client.js";
import {
  listMcpConnections,
  addMcpConnection,
  updateMcpConnection,
  deleteMcpConnection,
} from "../api/admin.js";

describe("api/admin.js — MCP calls", () => {
  beforeEach(() => vi.clearAllMocks());

  it("listMcpConnections calls GET /admin/mcp", async () => {
    clientMod.get.mockResolvedValue({ connections: [] });
    await listMcpConnections();
    expect(clientMod.get).toHaveBeenCalledWith("/admin/mcp");
  });

  it("addMcpConnection posts to /admin/mcp with correct fields", async () => {
    clientMod.post.mockResolvedValue({ connection: null });
    const body = { name: "s", url: "https://x", transport: "sse", authType: "none" };
    await addMcpConnection(body);
    expect(clientMod.post).toHaveBeenCalledWith("/admin/mcp", { body });
  });

  it("updateMcpConnection patches /admin/mcp/:id with correct fields", async () => {
    clientMod.patch.mockResolvedValue({ connection: null });
    const body = { name: "s2", url: "https://x2", transport: "streamable_http", authType: "bearer", bearerToken: "tok" };
    await updateMcpConnection("conn-123", body);
    expect(clientMod.patch).toHaveBeenCalledWith("/admin/mcp/conn-123", { body });
  });

  it("deleteMcpConnection calls DELETE /admin/mcp/:id", async () => {
    clientMod.del.mockResolvedValue({ success: true });
    await deleteMcpConnection("conn-456");
    expect(clientMod.del).toHaveBeenCalledWith("/admin/mcp/conn-456");
  });
});
