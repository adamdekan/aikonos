import { describe, it, expect, vi } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import PendingApprovals from "../views/inbox/PendingApprovals.vue";
import ArgsView from "../components/ArgsView.vue";
import { listPendingApprovals, decideApproval } from "../api/approvals.js";

vi.mock("../api/approvals.js", () => ({
  listPendingApprovals: vi.fn(),
  decideApproval: vi.fn().mockResolvedValue({ newStatus: "APPROVED" }),
}));

// AG-07: a broker-held approval (e.g. the second vote of a 2-of-n gate) is
// listed and decided with the digest the list returned.
describe("PendingApprovals.vue", () => {
  it("lists approvals and approves with the listed digest", async () => {
    listPendingApprovals.mockResolvedValue({
      approvals: [{
        taskId: "t1", requesterId: "alice@example.com", requiresN: 2, approvals: 1, stepUp: true,
        argsDigest: "d1", steps: [{ seq: 1, toolId: "email.send", args: { to: "bob@example.com" } }],
      }],
    });
    const w = mount(PendingApprovals);
    await flushPromises();

    expect(w.text()).toContain("email.send");
    expect(w.text()).toContain("1 of 2 approvals");
    await w.find("[data-action='approve']").trigger("click");
    await flushPromises();

    expect(decideApproval).toHaveBeenCalledWith("t1", true, "d1");
    expect(w.find("[data-testid='approval-t1']").exists()).toBe(false);
  });

  it("renders nothing when there is nothing to decide", async () => {
    listPendingApprovals.mockResolvedValue({ approvals: [] });
    const w = mount(PendingApprovals);
    await flushPromises();
    expect(w.find("[data-testid='pending-approvals']").exists()).toBe(false);
  });
});

// M-28: nested arguments are shown open, not collapsed behind a click.
describe("ArgsView.vue", () => {
  it("opens nested arguments by default", () => {
    const w = mount(ArgsView, { props: { args: { body: { text: "line1\nline2" } } } });
    expect(w.find("details").attributes("open")).toBeDefined();
  });
});
