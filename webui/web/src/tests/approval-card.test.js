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
const mockStepUpSignin = vi.fn().mockResolvedValue(undefined);
vi.mock("../auth/oidc.js", () => ({ stepUpSignin: (...a) => mockStepUpSignin(...a) }));

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
    mockStepUpSignin.mockClear();
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
    mockPost.mockResolvedValue({ resolved: true });
    const w = card({ ownerMayApprove: false, requiresN: 3, approvals: 1 });
    expect(w.find("[data-testid='chat-approval-waiting-tc-1']").text()).toContain("Waiting for 2 more approvers");
    expect(w.find("[data-testid='chat-approve-tc-1']").exists()).toBe(false);
    await w.find("[data-testid='chat-deny-tc-1']").trigger("click");
    await flushPromises();
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

  it("withdraw on a run that is gone says so", async () => {
    mockPost.mockResolvedValue({ resolved: false });
    const w = card({ ownerMayApprove: false, requiresN: 2 });
    await w.find("[data-testid='chat-deny-tc-1']").trigger("click");
    await flushPromises();
    expect(w.find("[data-testid='chat-approval-gone-tc-1']").exists()).toBe(true);
  });

  // AG-13: approve stays locked until reviewed; a refusal for want of MFA
  // offers a fresh sign-in, then retries.
  it("step-up: signs in again on step_up_required and retries", async () => {
    live();
    const stepUpErr = Object.assign(new Error("step_up_required"), { status: 401, data: { error: "step_up_required", acr: "mfa" } });
    mockPost.mockRejectedValueOnce(stepUpErr).mockResolvedValueOnce({ newStatus: 5 });
    const w = card({ stepUp: true });
    expect(w.find("[data-testid='chat-approve-tc-1']").attributes("disabled")).toBeDefined();
    await w.find("[data-testid='chat-approval-reviewed-tc-1']").trigger("click");
    await w.find("[data-testid='chat-approve-tc-1']").trigger("click");
    await flushPromises();
    expect(w.find("[role='alert']").text()).toContain("multi-factor");

    await w.find("[data-testid='chat-approval-step-up-tc-1']").trigger("click");
    await flushPromises();
    expect(mockStepUpSignin).toHaveBeenCalledWith("mfa");
    expect(mockPost).toHaveBeenCalledTimes(2);
    expect(w.emitted("done")).toBeTruthy();
  });
});
