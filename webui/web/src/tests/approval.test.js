import { describe, it, expect, vi, beforeEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import ApprovalModal from "../components/ApprovalModal.vue";

// Mock api/client so tests don't hit the network.
const mockPost = vi.fn().mockResolvedValue({});
vi.mock("../api/client.js", () => ({
  post: (...args) => mockPost(...args),
  get: vi.fn().mockResolvedValue({ approvals: [] }),
  request: vi.fn(),
  del: vi.fn(),
  patch: vi.fn(),
}));

const baseInfo = {
  toolId: "web.fetch",
  toolCallId: "tc-99",
  stepUp: false,
  reason: "requires approval",
  args: { url: "https://example.com" },
};

function makeModal(props = {}) {
  return mount(ApprovalModal, {
    props: {
      request: baseInfo,
      visible: true,
      ...props,
    },
    global: { plugins: [createPinia()] },
  });
}

describe("ApprovalModal.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    mockPost.mockClear();
  });

  it("renders when visible=true", () => {
    const w = makeModal();
    expect(w.find(".approval-modal").exists()).toBe(true);
  });

  it("does not render when visible=false", () => {
    const w = makeModal({ visible: false });
    // Either no modal element, or it is hidden.
    const el = w.find(".approval-modal");
    expect(el.exists()).toBe(false);
  });

  it("shows the toolId", () => {
    const w = makeModal();
    expect(w.text()).toContain("web.fetch");
  });

  it("shows HUMAN pill when stepUp=false", () => {
    const w = makeModal({ request: { ...baseInfo, stepUp: false } });
    const text = w.text().toLowerCase();
    expect(text).toContain("human");
  });

  it("shows STEP-UP pill when stepUp=true", () => {
    const w = makeModal({ request: { ...baseInfo, stepUp: true } });
    const text = w.text().toUpperCase();
    expect(text).toContain("STEP");
  });

  it("Approve button POSTs {approved:true} and emits close", async () => {
    const w = makeModal();
    const approveBtn = w.find("[data-action='approve']");
    expect(approveBtn.exists()).toBe(true);
    await approveBtn.trigger("click");
    await flushPromises();
    expect(mockPost).toHaveBeenCalledWith(
      `/approve/${encodeURIComponent(baseInfo.toolCallId)}`,
      expect.objectContaining({ body: { approved: true } })
    );
    expect(w.emitted("close")).toBeTruthy();
  });

  it("Deny button POSTs {approved:false} and emits close", async () => {
    const w = makeModal();
    const denyBtn = w.find("[data-action='deny']");
    expect(denyBtn.exists()).toBe(true);
    await denyBtn.trigger("click");
    await flushPromises();
    expect(mockPost).toHaveBeenCalledWith(
      `/approve/${encodeURIComponent(baseInfo.toolCallId)}`,
      expect.objectContaining({ body: { approved: false } })
    );
    expect(w.emitted("close")).toBeTruthy();
  });

  it("deduplication: updating request prop to same toolCallId does not create a second modal", async () => {
    // The modal is a single component; duplicate suppression is handled by Chat.vue
    // (same toolCallId → reuse). Here we verify the modal doesn't double-render internals.
    const w = makeModal();
    await w.setProps({ request: { ...baseInfo, toolCallId: "tc-99" } });
    const buttons = w.findAll("[data-action='approve']");
    expect(buttons.length).toBe(1);
  });
});
