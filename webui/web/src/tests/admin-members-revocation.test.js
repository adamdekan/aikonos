// AG-06: Members roster shows blocked status and lets an admin block, unblock
// and revoke all sessions of a user, each with a required reason.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";

vi.mock("../api/admin.js", () => ({
  listMembers:        vi.fn(),
  setUserBlocked:     vi.fn(),
  revokeUserSessions: vi.fn(),
}));

import Members from "../views/admin/Members.vue";
import * as adminApi from "../api/admin.js";

const ROWS = [
  { subject: "alice", email: "alice@example.com", name: "Alice" },
  { subject: "bob", email: "bob@example.com", name: "Bob", blocked: true, blockReason: "left", blockedBy: "admin",
    blockedAt: "2026-10-28T10:00:00Z", sessionsRevokedAt: "2026-10-28T10:00:00Z" },
];

async function act(w, button, reason) {
  await w.find(`[data-testid='${button}']`).trigger("click");
  await flushPromises();
  expect(document.querySelector("[data-testid='revocation-confirm']").disabled).toBe(true);
  const ta = document.querySelector("[data-testid='revocation-reason']");
  ta.value = reason;
  ta.dispatchEvent(new Event("input"));
  await flushPromises();
  document.querySelector("[data-testid='revocation-confirm']").click();
  await flushPromises();
}

describe("Members.vue revocation (AG-06)", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    adminApi.listMembers.mockResolvedValue({ members: ROWS });
    adminApi.setUserBlocked.mockResolvedValue({ changed: true });
    adminApi.revokeUserSessions.mockResolvedValue({ notBefore: "2026-10-28T12:00:00Z" });
  });

  it("shows blocked status and the matching action", async () => {
    const w = mount(Members, { attachTo: document.body });
    await flushPromises();
    expect(w.find("[data-testid='member-status-alice']").text()).toBe("active");
    expect(w.find("[data-testid='member-status-bob']").text()).toBe("blocked");
    expect(w.find("[data-testid='member-revoked-at-bob']").exists()).toBe(true);
    expect(w.find("[data-testid='member-block-alice']").exists()).toBe(true);
    expect(w.find("[data-testid='member-unblock-bob']").exists()).toBe(true);
    expect(w.find("[data-testid='member-block-bob']").exists()).toBe(false);
    w.unmount();
  });

  it("block, unblock and revoke send the reason and reload", async () => {
    const w = mount(Members, { attachTo: document.body });
    await flushPromises();
    await act(w, "member-block-alice", "left the company");
    expect(adminApi.setUserBlocked).toHaveBeenCalledWith("alice", true, "left the company");
    await act(w, "member-unblock-bob", "back");
    expect(adminApi.setUserBlocked).toHaveBeenCalledWith("bob", false, "back");
    await act(w, "member-revoke-sessions-alice", "stolen laptop");
    expect(adminApi.revokeUserSessions).toHaveBeenCalledWith("alice", "stolen laptop");
    expect(adminApi.listMembers).toHaveBeenCalledTimes(4);
    w.unmount();
  });

  it("keeps the dialog open with the error when the broker refuses", async () => {
    adminApi.setUserBlocked.mockRejectedValue(new Error("you cannot block yourself"));
    const w = mount(Members, { attachTo: document.body });
    await flushPromises();
    await act(w, "member-block-alice", "x");
    expect(document.querySelector("[data-testid='revocation-error']").textContent).toContain("cannot block yourself");
    w.unmount();
  });
});
