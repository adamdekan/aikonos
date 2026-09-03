// View tests for Inbox.vue (CP5 — three per-item actions, hidden form, scrollable intent).
// api/inbox.js is fully mocked so no server is needed.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";
import { useInboxStore } from "../store/inbox.js";
import { usePromptStore } from "../store/prompt.js";

vi.mock("../api/inbox.js", () => ({
  listInbox: vi.fn(),
  dismiss:   vi.fn(),
  delegate:  vi.fn(),
}));

// SkillTransferModal's own preview/accept behavior is covered by
// skill-transfer-modal.test.js — stubbed here (ShareSkillModal/skills-view
// precedent) so Inbox.vue tests only assert the row branch + wiring.
vi.mock("../components/SkillTransferModal.vue", () => ({
  default: {
    name: "SkillTransferModal",
    props: ["envelopeId", "fromDisplayName", "visible"],
    emits: ["close", "accepted"],
    template: "<div />",
  },
}));

import Inbox from "../views/Inbox.vue";
import * as inboxApi from "../api/inbox.js";

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/inbox", component: Inbox },
      { path: "/chat",  component: { template: "<div/>" } },
      { path: "/",      component: { template: "<div/>" } },
    ],
  });
}

const SAMPLE_ENV = {
  envelopeId: "env-1",
  fromUserId: "alice@example.com",
  task: { intent: "fetch market data", payloadRef: "", requiredSkills: [], priority: "normal" },
  status: "PENDING",
  createdAt: "2024-01-01T00:00:00Z",
};

// , "Transfer" — SendSkillTransfer fixes kind and
// the intent format; the row parses the skill name back out of the intent.
const TRANSFER_ENV = {
  envelopeId: "env-2",
  fromUserId: "alice@example.com",
  fromDisplayName: "Alice Smith",
  task: { intent: "Skill transfer: release-notes", kind: "skill_transfer" },
  status: "PENDING",
  createdAt: "2024-01-01T00:00:00Z",
};

describe("Inbox.vue — CP5", () => {
  let pinia;

  beforeEach(() => {
    pinia = createPinia();
    setActivePinia(pinia);
    useUserStore().setFromProfile({ email: "bob@example.com", sub: "bob@example.com" });
    vi.clearAllMocks();
  });

  afterEach(() => vi.restoreAllMocks());

  // ── helpers ──────────────────────────────────────────────────────────────

  async function mountInbox(envelopes = [SAMPLE_ENV]) {
    inboxApi.listInbox.mockResolvedValue({ envelopes });
    const router = makeRouter();
    await router.push("/inbox");
    const w = mount(Inbox, { global: { plugins: [router, pinia] } });
    await flushPromises();
    return { w, router };
  }

  // ── rendering ─────────────────────────────────────────────────────────────

  it("renders inbox list from API response", async () => {
    const { w } = await mountInbox([SAMPLE_ENV]);
    expect(w.findAll("[data-testid='envelope-row']").length).toBe(1);
    expect(w.text()).toContain("fetch market data");
  });

  it("shows error banner on API error", async () => {
    inboxApi.listInbox.mockRejectedValue(new Error("server error"));
    const router = makeRouter();
    await router.push("/inbox");
    const w = mount(Inbox, { global: { plugins: [router, pinia] } });
    await flushPromises();
    expect(w.find("[data-testid='error-banner']").exists()).toBe(true);
  });

  it("inboxStore.count equals loaded envelope count after load", async () => {
    await mountInbox([SAMPLE_ENV, SAMPLE_ENV]);
    expect(useInboxStore().count).toBe(2);
  });

  it("inboxStore.count updates to 0 after load with empty list", async () => {
    const inboxStore = useInboxStore();
    inboxStore.setCount(5);
    await mountInbox([]);
    expect(inboxStore.count).toBe(0);
  });

  // ── delegate form hidden ──────────────────────────────────────────────────

  it("delegate form is NOT rendered (SHOW_DELEGATE_FORM = false)", async () => {
    const { w } = await mountInbox([]);
    expect(w.find("[data-testid='delegate-form']").exists()).toBe(false);
  });

  // ── Send to agent ─────────────────────────────────────────────────────────

  it("Send to agent sets promptStore.pending with intent, dismisses, and navigates to /chat", async () => {
    inboxApi.dismiss.mockResolvedValue({ success: true });
    const { w, router } = await mountInbox([SAMPLE_ENV]);
    const promptStore = usePromptStore();

    await w.find("[data-testid='send-to-agent-env-1']").trigger("click");
    await flushPromises();

    expect(promptStore.pending).toBe("fetch market data");
    expect(inboxApi.dismiss).toHaveBeenCalledWith("env-1");
    expect(router.currentRoute.value.path).toBe("/chat");
  });

  it("Send to agent still navigates to /chat when dismiss rejects", async () => {
    inboxApi.dismiss.mockRejectedValue(new Error("network error"));
    const { w, router } = await mountInbox([SAMPLE_ENV]);
    const promptStore = usePromptStore();

    await w.find("[data-testid='send-to-agent-env-1']").trigger("click");
    await flushPromises();

    expect(promptStore.pending).toBe("fetch market data");
    expect(router.currentRoute.value.path).toBe("/chat");
  });

  it("Send to agent with empty intent does not navigate and does not call dismiss", async () => {
    inboxApi.dismiss.mockResolvedValue({ success: true });
    const emptyIntentEnv = { ...SAMPLE_ENV, task: { intent: "   " } };
    const { w, router } = await mountInbox([emptyIntentEnv]);

    await w.find("[data-testid='send-to-agent-env-1']").trigger("click");
    await flushPromises();

    expect(inboxApi.dismiss).not.toHaveBeenCalled();
    expect(router.currentRoute.value.path).toBe("/inbox");
  });

  it("Send to agent with missing task does not navigate and does not call dismiss", async () => {
    inboxApi.dismiss.mockResolvedValue({ success: true });
    const noTaskEnv = { ...SAMPLE_ENV, task: null };
    const { w, router } = await mountInbox([noTaskEnv]);

    await w.find("[data-testid='send-to-agent-env-1']").trigger("click");
    await flushPromises();

    expect(inboxApi.dismiss).not.toHaveBeenCalled();
    expect(router.currentRoute.value.path).toBe("/inbox");
  });

  // ── Start a new session ───────────────────────────────────────────────────

  it("Start a new session calls setPrefill(intent) and navigates to /chat", async () => {
    const { w, router } = await mountInbox([SAMPLE_ENV]);
    const promptStore = usePromptStore();

    await w.find("[data-testid='start-session-env-1']").trigger("click");
    await flushPromises();

    expect(promptStore.prefill).toBe("fetch market data");
    expect(router.currentRoute.value.path).toBe("/chat");
    expect(inboxApi.dismiss).not.toHaveBeenCalled();
  });

  it("Start a new session with no task.intent prefills empty string", async () => {
    const envNoIntent = { ...SAMPLE_ENV, task: null };
    const { w } = await mountInbox([envNoIntent]);
    const promptStore = usePromptStore();

    await w.find("[data-testid='start-session-env-1']").trigger("click");
    await flushPromises();

    expect(promptStore.prefill).toBe("");
  });

  // ── OK (dismiss) ──────────────────────────────────────────────────────────

  it("OK calls dismiss(id) and reloads", async () => {
    inboxApi.dismiss.mockResolvedValue({ success: true });
    inboxApi.listInbox
      .mockResolvedValueOnce({ envelopes: [SAMPLE_ENV] })
      .mockResolvedValue({ envelopes: [] });

    const router = makeRouter();
    await router.push("/inbox");
    const w = mount(Inbox, { global: { plugins: [router, pinia] } });
    await flushPromises();

    await w.find("[data-testid='dismiss-env-1']").trigger("click");
    await flushPromises();

    expect(inboxApi.dismiss).toHaveBeenCalledWith("env-1");
  });

  // ── Scrollable intent container ───────────────────────────────────────────

  it("intent renders inside the scrollable container", async () => {
    const { w } = await mountInbox([SAMPLE_ENV]);
    const scrollEl = w.find("[data-testid='intent-scroll']");
    expect(scrollEl.exists()).toBe(true);
    expect(scrollEl.text()).toContain("fetch market data");
  });

  it("scrollable container is present for a long intent", async () => {
    const longEnv = {
      ...SAMPLE_ENV,
      task: { intent: "A".repeat(500) },
    };
    const { w } = await mountInbox([longEnv]);
    const scrollEl = w.find("[data-testid='intent-scroll']");
    expect(scrollEl.exists()).toBe(true);
    expect(scrollEl.text()).toContain("A".repeat(500));
  });

  // ── Skill transfer row ────────────

  it("a skill_transfer envelope renders a Review action, not Send to agent / New session", async () => {
    const { w } = await mountInbox([TRANSFER_ENV]);

    expect(w.find("[data-testid='review-env-2']").exists()).toBe(true);
    expect(w.find("[data-testid='send-to-agent-env-2']").exists()).toBe(false);
    expect(w.find("[data-testid='start-session-env-2']").exists()).toBe(false);
    expect(w.text()).toContain("Skill transfer");
    expect(w.text()).toContain("release-notes");
    expect(w.text()).toContain("Alice Smith");
  });

  it("Review opens the SkillTransferModal for the clicked envelope", async () => {
    const { w } = await mountInbox([TRANSFER_ENV]);

    await w.find("[data-testid='review-env-2']").trigger("click");
    await flushPromises();

    const modal = w.findComponent({ name: "SkillTransferModal" });
    expect(modal.props("visible")).toBe(true);
    expect(modal.props("envelopeId")).toBe("env-2");
    expect(modal.props("fromDisplayName")).toBe("Alice Smith");
  });

  it("Dismiss still works on a skill_transfer row", async () => {
    inboxApi.dismiss.mockResolvedValue({ success: true });
    const { w } = await mountInbox([TRANSFER_ENV]);

    await w.find("[data-testid='dismiss-env-2']").trigger("click");
    await flushPromises();

    expect(inboxApi.dismiss).toHaveBeenCalledWith("env-2");
  });

  it("an accepted transfer closes the modal, toasts, and reloads the inbox", async () => {
    inboxApi.listInbox
      .mockResolvedValueOnce({ envelopes: [TRANSFER_ENV] })
      .mockResolvedValue({ envelopes: [] });
    const { w } = await mountInbox([TRANSFER_ENV]);

    await w.find("[data-testid='review-env-2']").trigger("click");
    w.findComponent({ name: "SkillTransferModal" }).vm.$emit("accepted");
    await flushPromises();

    expect(w.findComponent({ name: "SkillTransferModal" }).props("visible")).toBe(false);
    expect(inboxApi.listInbox).toHaveBeenCalledTimes(2);
  });
});
