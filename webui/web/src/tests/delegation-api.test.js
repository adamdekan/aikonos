// Tests for api/delegation.js in isolation (no view rendering).
// Intent: verify the module issues the right GET and surfaces the response body,
// mirroring the files-api.test.js harness pattern.
import { describe, it, expect, vi, beforeEach } from "vitest";

vi.mock("../api/client.js", () => ({
  get:   vi.fn(),
  post:  vi.fn(),
  del:   vi.fn(),
  patch: vi.fn(),
}));

import * as clientMod from "../api/client.js";
import { listDelegatableUsers } from "../api/delegation.js";

describe("api/delegation.js", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("listDelegatableUsers calls GET /delegatable-users", async () => {
    // Confirms the module routes to the correct endpoint — wrong path = silent
    // 404 in production with no hint.
    clientMod.get.mockResolvedValue({ users: [] });
    await listDelegatableUsers();
    expect(clientMod.get).toHaveBeenCalledWith("/delegatable-users");
  });

  it("listDelegatableUsers returns the parsed response body", async () => {
    // Confirms the caller receives the full shape, not a wrapper or undefined.
    const body = { users: [{ userId: "alice", displayName: "Alice" }] };
    clientMod.get.mockResolvedValue(body);
    const result = await listDelegatableUsers();
    expect(result).toEqual(body);
  });

  it("listDelegatableUsers surfaces forbidden responses from client.js", async () => {
    // client.js maps 403 → { forbidden: true }; callers (Chat.vue loadDelegatableUsers)
    // must see it to treat it as empty rather than a thrown error.
    clientMod.get.mockResolvedValue({ forbidden: true });
    const result = await listDelegatableUsers();
    expect(result).toEqual({ forbidden: true });
  });
});
