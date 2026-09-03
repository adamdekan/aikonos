// Tests for views/admin/AuditHistory.vue
// api/client.js is mocked so no server is needed.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";

vi.mock("../api/client.js", () => ({
  get:   vi.fn(),
  post:  vi.fn(),
  del:   vi.fn(),
  patch: vi.fn(),
}));

import AuditHistory from "../views/admin/AuditHistory.vue";
import * as client from "../api/client.js";

const SAMPLE_EVENT = {
  event_id: "evt-001",
  event_type: "aikonos.broker.tool.invoked",
  actor_user_id: "alice@example.com",
  resource_ref: "aikonos:tool:web.fetch",
  decision: 1,
  occurred_at: { seconds: 1700000000, nanos: 0 },
};

const SAMPLE_EVENT_2 = {
  event_id: "evt-002",
  event_type: "aikonos.broker.plan.denied",
  actor_user_id: "bob@example.com",
  resource_ref: "aikonos:task:t2",
  decision: 2,
  occurred_at: { seconds: 1700000001, nanos: 0 },
};

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/admin/audit/history", component: AuditHistory },
      { path: "/", component: { template: "<div/>" } },
    ],
  });
}

describe("AuditHistory.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    useUserStore().setFromProfile({ email: "admin@example.com", sub: "admin@example.com" });
    vi.clearAllMocks();
  });
  afterEach(() => vi.restoreAllMocks());

  // ── Case 1: search form + rows + load more ─────────────────────────────────
  it("submitting the search form calls client.get with entered filters; rows render", async () => {
    client.get.mockResolvedValue({ events: [SAMPLE_EVENT], nextCursor: "", storeConfigured: true });
    const router = makeRouter();
    await router.push("/admin/audit/history");
    const w = mount(AuditHistory, { global: { plugins: [router] } });

    // Fill form fields
    await w.find("[data-testid='filter-actor']").setValue("alice@example.com");
    await w.find("[data-testid='filter-event-type']").setValue("aikonos.broker.tool.invoked");
    await w.find("[data-testid='filter-decision']").setValue("1");
    await w.find("[data-testid='search-btn']").trigger("click");
    await flushPromises();

    expect(client.get).toHaveBeenCalledWith(
      expect.stringContaining("/admin/audit/query"),
    );
    const url = client.get.mock.calls[0][0];
    expect(url).toContain("actor=alice%40example.com");
    expect(url).toContain("event_type=aikonos.broker.tool.invoked");
    expect(url).toContain("decision=1");

    expect(w.findAll("[data-testid='audit-history-row']").length).toBe(1);
    expect(w.text()).toContain("aikonos.broker.tool.invoked");
    expect(w.text()).toContain("alice@example.com");
    // ui-set: DataTable renders the table
    expect(w.find(".dt-table").exists()).toBe(true);
  });

  it("'Load more' button is shown when nextCursor is set; clicking it appends next page", async () => {
    client.get
      .mockResolvedValueOnce({ events: [SAMPLE_EVENT], nextCursor: "cursor-abc", storeConfigured: true })
      .mockResolvedValueOnce({ events: [SAMPLE_EVENT_2], nextCursor: "", storeConfigured: true });

    const router = makeRouter();
    await router.push("/admin/audit/history");
    const w = mount(AuditHistory, { global: { plugins: [router] } });

    await w.find("[data-testid='search-btn']").trigger("click");
    await flushPromises();

    // First page: 1 row + Load more visible
    expect(w.findAll("[data-testid='audit-history-row']").length).toBe(1);
    const loadMore = w.find("[data-testid='load-more-btn']");
    expect(loadMore.exists()).toBe(true);

    await loadMore.trigger("click");
    await flushPromises();

    // Second call must include the cursor
    const secondUrl = client.get.mock.calls[1][0];
    expect(secondUrl).toContain("cursor=cursor-abc");

    // Both rows now rendered
    expect(w.findAll("[data-testid='audit-history-row']").length).toBe(2);

    // No more Load more
    expect(w.find("[data-testid='load-more-btn']").exists()).toBe(false);
  });

  it("'Load more' is absent when nextCursor is empty", async () => {
    client.get.mockResolvedValue({ events: [SAMPLE_EVENT], nextCursor: "", storeConfigured: true });
    const router = makeRouter();
    await router.push("/admin/audit/history");
    const w = mount(AuditHistory, { global: { plugins: [router] } });

    await w.find("[data-testid='search-btn']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='load-more-btn']").exists()).toBe(false);
  });

  // ── Case 2: verify integrity ───────────────────────────────────────────────
  it("Verify integrity: ok:true shows intact banner", async () => {
    client.get.mockResolvedValue({
      total: 42, ok: true, signed: true, storeConfigured: true,
      headEventId: "head-001", tailEventId: "tail-042",
      breaks: [], signatureFailures: [],
    });
    const router = makeRouter();
    await router.push("/admin/audit/history");
    const w = mount(AuditHistory, { global: { plugins: [router] } });

    await w.find("[data-testid='verify-btn']").trigger("click");
    await flushPromises();

    expect(client.get).toHaveBeenCalledWith(
      expect.stringContaining("/admin/audit/verify"),
    );
    expect(w.find("[data-testid='verify-result']").exists()).toBe(true);
    expect(w.find("[data-testid='verify-result']").text()).toContain("intact");
  });

  it("Verify integrity: breaks + signature failures shows count and offending ids", async () => {
    client.get.mockResolvedValue({
      total: 10, ok: false, signed: true, storeConfigured: true,
      headEventId: "head-001", tailEventId: "tail-010",
      breaks: [{ eventId: "evt-005", detail: "hash mismatch" }],
      signatureFailures: [{ eventId: "evt-007", detail: "bad sig" }],
    });
    const router = makeRouter();
    await router.push("/admin/audit/history");
    const w = mount(AuditHistory, { global: { plugins: [router] } });

    await w.find("[data-testid='verify-btn']").trigger("click");
    await flushPromises();

    const result = w.find("[data-testid='verify-result']");
    expect(result.exists()).toBe(true);
    const txt = result.text();
    expect(txt).toContain("1");        // 1 break
    expect(txt).toContain("evt-005");  // offending id
    expect(txt).toContain("evt-007");  // sig failure id
  });

  // ── Case 3: forbidden ──────────────────────────────────────────────────────
  it("forbidden:true response shows admin empty-state without throwing", async () => {
    client.get.mockResolvedValue({ forbidden: true });
    const router = makeRouter();
    await router.push("/admin/audit/history");
    const w = mount(AuditHistory, { global: { plugins: [router] } });

    await w.find("[data-testid='search-btn']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='forbidden']").exists()).toBe(true);
    expect(w.find("[data-testid='audit-history-row']").exists()).toBe(false);
  });

  it("forbidden:true on verify shows admin empty-state without throwing", async () => {
    client.get.mockResolvedValue({ forbidden: true });
    const router = makeRouter();
    await router.push("/admin/audit/history");
    const w = mount(AuditHistory, { global: { plugins: [router] } });

    await w.find("[data-testid='verify-btn']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='forbidden']").exists()).toBe(true);
  });
});
