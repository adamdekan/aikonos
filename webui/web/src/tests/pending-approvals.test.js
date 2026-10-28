import { describe, it, expect, vi } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import PendingApprovals from "../views/inbox/PendingApprovals.vue";
import ArgsView from "../components/ArgsView.vue";
import { listPendingApprovals, decideApproval } from "../api/approvals.js";
import { stepUpSignin } from "../auth/oidc.js";

vi.mock("../api/approvals.js", async (orig) => ({
  ...(await orig()),
  listPendingApprovals: vi.fn(),
  decideApproval: vi.fn().mockResolvedValue({ newStatus: "APPROVED" }),
}));
vi.mock("../auth/oidc.js", () => ({ stepUpSignin: vi.fn().mockResolvedValue(undefined) }));

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

// AG-07 slice 2: the notification's deep link (/inbox?approval=<taskId>)
// marks that request.
describe("PendingApprovals.vue deep link", () => {
  it("marks the focused request", async () => {
    listPendingApprovals.mockResolvedValue({
      approvals: [
        { taskId: "t1", requesterId: "a", requiresN: 1, approvals: 0, argsDigest: "d1", steps: [] },
        { taskId: "t2", requesterId: "b", requiresN: 2, approvals: 1, argsDigest: "d2", steps: [] },
      ],
    });
    const w = mount(PendingApprovals, { props: { focusTask: "t2" } });
    await flushPromises();
    expect(w.find("[data-testid='approval-t2']").classes()).toContain("pa-focus");
    expect(w.find("[data-testid='approval-t1']").classes()).not.toContain("pa-focus");
  });
});

// AG-13: a step-up approval refused for want of a recent MFA sign-in offers a
// fresh sign-in with the tenant's acr, then retries the approval.
describe("PendingApprovals.vue step-up", () => {
  it("signs in again on step_up_required and retries the approval", async () => {
    listPendingApprovals.mockResolvedValue({
      approvals: [{ taskId: "t9", requesterId: "alice@example.com", requiresN: 1, approvals: 0, stepUp: true, argsDigest: "d9", steps: [] }],
    });
    const err = Object.assign(new Error("step_up_required"), { status: 401, data: { error: "step_up_required", acr: "c1" } });
    decideApproval.mockReset();
    decideApproval.mockRejectedValueOnce(err).mockResolvedValueOnce({ newStatus: "APPROVED" });
    const w = mount(PendingApprovals);
    await flushPromises();

    await w.find("[data-action='approve']").trigger("click");
    await flushPromises();
    expect(w.find("[role='alert']").text()).toContain("multi-factor");

    await w.find("[data-testid='approval-step-up-t9']").trigger("click");
    await flushPromises();
    expect(stepUpSignin).toHaveBeenCalledWith("c1");
    expect(decideApproval).toHaveBeenCalledTimes(2);
    expect(w.find("[data-testid='approval-t9']").exists()).toBe(false);
  });
});
