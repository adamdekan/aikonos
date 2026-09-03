// Tests for api/admin.js in isolation (no view rendering).
import { describe, it, expect, vi, beforeEach } from "vitest";

vi.mock("../api/client.js", () => ({
  get:  vi.fn(),
  post: vi.fn(),
}));

import * as clientMod from "../api/client.js";
import {
  listAssignments,
  assignRole,
  revokeRole,
  listNetworkRules,
  addNetworkRule,
  deleteNetworkRule,
  listAdminRuns,
} from "../api/admin.js";

describe("api/admin.js", () => {
  beforeEach(() => vi.clearAllMocks());

  // ── assignments ──────────────────────────────────────────────────────────

  it("listAssignments calls GET /admin/assignments", async () => {
    clientMod.get.mockResolvedValue({ tuples: [], principals: [], fgaEnabled: true, warnings: [] });
    await listAssignments();
    expect(clientMod.get).toHaveBeenCalledWith("/admin/assignments");
  });

  it("assignRole posts tuple to /admin/assignments", async () => {
    clientMod.post.mockResolvedValue({ success: true });
    const tuple = { user: "user:alice@example.com", relation: "member", object: "group:ops" };
    await assignRole(tuple);
    expect(clientMod.post).toHaveBeenCalledWith("/admin/assignments", { body: { tuple } });
  });

  it("revokeRole posts tuple to /admin/assignments/revoke", async () => {
    clientMod.post.mockResolvedValue({ success: true });
    const tuple = { user: "user:alice@example.com", relation: "member", object: "group:ops" };
    await revokeRole(tuple);
    expect(clientMod.post).toHaveBeenCalledWith("/admin/assignments/revoke", { body: { tuple } });
  });

  // ── network rules ────────────────────────────────────────────────────────

  it("listNetworkRules calls GET /admin/network", async () => {
    clientMod.get.mockResolvedValue({ rules: [] });
    await listNetworkRules();
    expect(clientMod.get).toHaveBeenCalledWith("/admin/network");
  });

  it("addNetworkRule posts to /admin/network with correct fields", async () => {
    clientMod.post.mockResolvedValue({ rule: null });
    const rule = { scopeKind: "TENANT", scopeValue: "", action: "DENY", hostPattern: "*", note: "block all" };
    await addNetworkRule(rule);
    expect(clientMod.post).toHaveBeenCalledWith("/admin/network", { body: rule });
  });

  it("deleteNetworkRule posts id to /admin/network/delete", async () => {
    clientMod.post.mockResolvedValue({ success: true });
    await deleteNetworkRule("rule-123");
    expect(clientMod.post).toHaveBeenCalledWith("/admin/network/delete", { body: { id: "rule-123" } });
  });

  // ── scheduled runs (admin oversight) ─────────────────────────────────────

  it("listAdminRuns calls GET /admin/scheduled-runs without owner filter", async () => {
    clientMod.get.mockResolvedValue({ schedules: [], fgaEnabled: true });
    await listAdminRuns();
    expect(clientMod.get).toHaveBeenCalledWith("/admin/scheduled-runs");
  });

  it("listAdminRuns passes owner query param when provided", async () => {
    clientMod.get.mockResolvedValue({ schedules: [], fgaEnabled: true });
    await listAdminRuns("alice@example.com");
    expect(clientMod.get).toHaveBeenCalledWith("/admin/scheduled-runs?owner=alice%40example.com");
  });
});
