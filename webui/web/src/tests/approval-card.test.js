import { describe, it, expect, vi, beforeEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import ApprovalCard from "../components/ApprovalCard.vue";

// AG-07: the inline approval card in the chat transcript.
const mockGet = vi.fn();
const mockPost = vi.fn();
vi.mock("../api/client.js", () => ({
  get: (...a) => mockGet(...a),
  post: (...a) => mockPost(...a),
  request: vi.fn(),
  del: vi.fn(),
  patch: vi.fn(),
}));

const base = {
  toolCallId: "tc-1",
  taskId: "task-1",
  toolId: "web.fetch",
  reason: "human approval required",
  args: { url: "https://example.com" },
  argsDigest: "digest-1",
  stepUp: false,
  requiresN: 1,
  approvals: 0,
  ownerMayApprove: true,
};

const card = (request = {}) => mount(ApprovalCard, { props: { request: { ...base, ...request } } });
const live = () => mockGet.mockResolvedValue({ approvals: [{ toolCallId: "tc-1" }] });

describe("ApprovalCard.vue", () => {
  beforeEach(() => {
    mockGet.mockReset();
    mockPost.mockReset();
    mockPost.mockResolvedValue({ newStatus: 5 });
  });

  it("shows the tool, the risk and collapsed arguments", () => {
    const w = card({ stepUp: true });
    expect(w.text()).toContain("web.fetch");
    expect(w.text()).toContain("STEP-UP");
    expect(w.find("details").exists()).toBe(true);
    expect(w.find("[data-testid='chat-approve-tc-1']").exists()).toBe(true);
    expect(w.find("[data-testid='chat-deny-tc-1']").exists()).toBe(true);
  });

  it("approve sends the digest to the broker and closes the card", async () => {
    live();
    const w = card();
    await w.find("[data-testid='chat-approve-tc-1']").trigger("click");
    await flushPromises();
    expect(mockPost).toHaveBeenCalledWith("/pending-approvals/task-1", { body: { approved: true, argsDigest: "digest-1" } });
    expect(w.emitted("done")[0]).toEqual(["tc-1"]);
  });

  it("deny sends the digest to the broker and closes the card", async () => {
    live();
    const w = card();
    await w.find("[data-testid='chat-deny-tc-1']").trigger("click");
    await flushPromises();
    expect(mockPost).toHaveBeenCalledWith("/pending-approvals/task-1", { body: { approved: false, argsDigest: "digest-1" } });
    expect(w.emitted("done")).toBeTruthy();
  });

  it("says so when the run that asked no longer exists", async () => {
    mockGet.mockResolvedValue({ approvals: [] });
    const w = card();
    await w.find("[data-testid='chat-approve-tc-1']").trigger("click");
    await flushPromises();
    expect(mockPost).toHaveBeenCalledWith("/pending-approvals/task-1", { body: { approved: true, argsDigest: "digest-1" } });
    expect(w.find("[data-testid='chat-approval-gone-tc-1']").text()).toContain("no longer running");
    expect(w.emitted("done")).toBeFalsy();
    expect(w.find("[data-testid='chat-approve-tc-1']").exists()).toBe(false);
  });

  it("n-of-m: the requester waits for the others and can only withdraw", async () => {
    mockPost.mockResolvedValue({ withdrawn: true });
    const w = card({ ownerMayApprove: false, requiresN: 3, approvals: 1 });
    expect(w.find("[data-testid='chat-approval-waiting-tc-1']").text()).toContain("Waiting for 2 more approvers");
    expect(w.find("[data-testid='chat-approve-tc-1']").exists()).toBe(false);
    expect(w.find("[data-testid='chat-deny-tc-1']").text()).toBe("Withdraw");
    await w.find("[data-testid='chat-deny-tc-1']").trigger("click");
    await flushPromises();
    // The broker closes the request and cancels the task, then the run's wait ends.
    expect(mockPost.mock.calls[0]).toEqual(["/pending-approvals/task-1/withdraw"]);
    expect(mockPost).toHaveBeenCalledWith("/approve/tc-1", { body: { approved: false, argsDigest: "digest-1" } });
    expect(w.emitted("done")).toBeTruthy();
  });

  it("n-of-m: after this user's vote the card waits for the rest", async () => {
    live();
    mockPost.mockResolvedValue({ newStatus: 4 }); // still AWAITING_APPROVAL
    const w = card({ requiresN: 2 });
    await w.find("[data-testid='chat-approve-tc-1']").trigger("click");
    await flushPromises();
    expect(w.find("[data-testid='chat-approval-waiting-tc-1']").text()).toContain("Waiting for 1 more approver.");
    expect(w.emitted("done")).toBeFalsy();
  });

  it("withdraw closes the broker request even when the run is gone", async () => {
    mockPost.mockImplementation((url) =>
      url.endsWith("/withdraw") ? Promise.resolve({ withdrawn: true }) : Promise.reject(new Error("gone")));
    const w = card({ ownerMayApprove: false, requiresN: 2 });
    await w.find("[data-testid='chat-deny-tc-1']").trigger("click");
    await flushPromises();
    expect(mockPost).toHaveBeenCalledWith("/pending-approvals/task-1/withdraw");
    expect(w.emitted("done")).toBeTruthy();
  });

  it("a refused withdraw keeps the card and shows why", async () => {
    mockPost.mockRejectedValue(new Error("only the task owner or a tenant admin may cancel this task"));
    const w = card({ ownerMayApprove: false, requiresN: 2 });
    await w.find("[data-testid='chat-deny-tc-1']").trigger("click");
    await flushPromises();
    expect(w.find("[role='alert']").text()).toContain("task owner");
    expect(w.emitted("done")).toBeFalsy();
  });

  it("a request with no broker task is answered in its run only", async () => {
    mockPost.mockResolvedValue({ resolved: false });
    const w = card({ taskId: "" });
    await w.find("[data-testid='chat-deny-tc-1']").trigger("click");
    await flushPromises();
    expect(mockPost).toHaveBeenCalledWith("/approve/tc-1", { body: { approved: false, argsDigest: "digest-1" } });
    expect(w.find("[data-testid='chat-approval-gone-tc-1']").exists()).toBe(true);
  });

  // A step-up gate needs two approvers other than the requester (M-11), so
  // the chat card only waits; the MFA re-sign-in lives in the Inbox.
  it("step-up: the requester waits, with no approve or sign-in path", () => {
    const w = card({ stepUp: true, ownerMayApprove: false, requiresN: 2 });
    expect(w.find("[data-testid='chat-approval-waiting-tc-1']").exists()).toBe(true);
    expect(w.find("[data-testid='chat-approve-tc-1']").exists()).toBe(false);
    expect(w.find("[data-testid='chat-approval-step-up-tc-1']").exists()).toBe(false);
  });

  it("n-of-m: the waiting count follows the votes as they come in", async () => {
    vi.useFakeTimers();
    try {
      mockGet.mockResolvedValue({ approvals: [{ toolCallId: "tc-1", approvals: 2 }] });
      const w = card({ ownerMayApprove: false, requiresN: 3, approvals: 0 });
      expect(w.find("[data-testid='chat-approval-waiting-tc-1']").text()).toContain("Waiting for 3 more approvers");
      await vi.advanceTimersByTimeAsync(5000);
      expect(mockGet).toHaveBeenCalledWith("/approvals");
      expect(w.find("[data-testid='chat-approval-waiting-tc-1']").text()).toContain("Waiting for 1 more approver.");
      w.unmount();
      mockGet.mockClear();
      await vi.advanceTimersByTimeAsync(10000);
      expect(mockGet).not.toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });
});
