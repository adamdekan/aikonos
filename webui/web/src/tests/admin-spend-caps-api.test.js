// Tests for api/admin.js spend-cap functions in isolation (no view rendering).
import { describe, it, expect, vi, beforeEach } from "vitest";

vi.mock("../api/client.js", () => ({
  get:  vi.fn(),
  post: vi.fn(),
  del:  vi.fn(),
}));

import * as clientMod from "../api/client.js";
import { listSpendCaps, setSpendCap, deleteSpendCap, getSpendSummary } from "../api/admin.js";

describe("api/admin.js — spend caps", () => {
  beforeEach(() => vi.clearAllMocks());

  it("listSpendCaps calls GET /admin/spend-caps", async () => {
    clientMod.get.mockResolvedValue({ caps: [] });
    await listSpendCaps();
    expect(clientMod.get).toHaveBeenCalledWith("/admin/spend-caps");
  });

  it("getSpendSummary calls GET /admin/spend-caps/summary", async () => {
    clientMod.get.mockResolvedValue({ orgSpendMicros: 0, orgCapMicros: 0, users: [], agents: [] });
    await getSpendSummary();
    expect(clientMod.get).toHaveBeenCalledWith("/admin/spend-caps/summary");
  });

  it("setSpendCap posts scope/subjectId/capMicros to /admin/spend-caps", async () => {
    clientMod.post.mockResolvedValue({ id: "cap-1" });
    await setSpendCap({ scope: "user", subjectId: "alice@example.com", capMicros: 5_000_000 });
    expect(clientMod.post).toHaveBeenCalledWith("/admin/spend-caps", {
      body: { scope: "user", subjectId: "alice@example.com", capMicros: 5_000_000 },
    });
  });

  it("deleteSpendCap calls DELETE /admin/spend-caps/:id", async () => {
    clientMod.del.mockResolvedValue({});
    await deleteSpendCap("cap-1");
    expect(clientMod.del).toHaveBeenCalledWith("/admin/spend-caps/cap-1");
  });
});
