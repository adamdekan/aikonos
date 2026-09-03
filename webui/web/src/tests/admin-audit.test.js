// Tests for views/admin/Audit.vue.
// EventSource is injectable — pass a factory prop to avoid needing a real server.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";
import { nextTick } from "vue";

import Audit from "../views/admin/Audit.vue";
import { toJson, toCsv } from "../views/admin/auditExport.js";

// ── Minimal fake EventSource ─────────────────────────────────────────────────
class FakeEventSource {
  constructor() {
    this._listeners = { message: [], open: [], error: [] };
    this.readyState = 0; // CONNECTING
  }
  addEventListener(type, fn) { (this._listeners[type] ??= []).push(fn); }
  set onmessage(fn) { this._listeners.message = [fn]; }
  set onopen(fn)    { this._listeners.open    = [fn]; }
  set onerror(fn)   { this._listeners.error   = [fn]; }
  close() { this.readyState = 2; }
  // test helpers
  emit(type, data) {
    const ev = { data: JSON.stringify(data), type };
    for (const fn of this._listeners[type] ?? []) fn(ev);
  }
  open() {
    this.readyState = 1;
    for (const fn of this._listeners.open ?? []) fn({});
  }
  error() {
    this.readyState = 0;
    for (const fn of this._listeners.error ?? []) fn({});
  }
}

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/admin/audit", component: Audit },
      { path: "/", component: { template: "<div/>" } },
    ],
  });
}

describe("Audit.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    useUserStore().setFromProfile({ email: "admin@example.com", sub: "admin@example.com" });
    vi.clearAllMocks();
  });
  afterEach(() => vi.restoreAllMocks());

  it("renders audit rows from injected SSE events", async () => {
    let fes;
    const factory = () => { fes = new FakeEventSource(); return fes; };
    const router = makeRouter();
    await router.push("/admin/audit");
    const w = mount(Audit, {
      global: { plugins: [router] },
      props: { eventSourceFactory: factory },
    });
    await flushPromises();

    fes.open();
    fes.emit("message", {
      event_id: "evt-1",
      event_type: "tool.invoked",
      actor_user_id: "alice@example.com",
      resource_ref: "task:t1",
      decision: 1,
      occurred_at: { seconds: 1700000000, nanos: 0 },
    });
    await nextTick();

    expect(w.findAll("[data-testid='audit-row']").length).toBe(1);
    expect(w.text()).toContain("tool.invoked");
    expect(w.text()).toContain("alice@example.com");
  });

  it("shows not-configured empty-state when no factory provided and configured=false", async () => {
    const router = makeRouter();
    await router.push("/admin/audit");
    // No factory prop — Audit.vue should detect unconfigured state
    const w = mount(Audit, {
      global: { plugins: [router] },
      props: { eventSourceFactory: null },
    });
    await flushPromises();
    expect(w.find("[data-testid='not-configured']").exists()).toBe(true);
    expect(w.find("[data-testid='audit-row']").exists()).toBe(false);
  });

  // CP2: the blanket "any pre-open error → notConfigured" behavior is superseded —
  // notConfigured is now set ONLY when the classification probe returns HTTP 501.
  it("shows not-configured empty-state when the classification probe returns 501", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ status: 501 }));
    let fes;
    const factory = () => { fes = new FakeEventSource(); return fes; };
    const router = makeRouter();
    await router.push("/admin/audit");
    const w = mount(Audit, {
      global: { plugins: [router] },
      props: { eventSourceFactory: factory },
    });
    await flushPromises();

    fes.error();
    await flushPromises();

    expect(w.find("[data-testid='not-configured']").exists()).toBe(true);
    expect(w.find("[data-testid='disconnected-banner']").exists()).toBe(false);
    vi.unstubAllGlobals();
  });

  // CP2: a pre-open failure that is NOT a 501 (network error, 5xx, etc.) must NOT
  // be classified as notConfigured — it retries with backoff instead.
  it("pre-open error with a non-501 probe result enters reconnecting, then a later successful open connects", async () => {
    vi.useFakeTimers();
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network error")));
    let fes;
    const factory = () => { fes = new FakeEventSource(); return fes; };
    const router = makeRouter();
    await router.push("/admin/audit");
    const w = mount(Audit, {
      global: { plugins: [router] },
      props: { eventSourceFactory: factory },
    });
    await flushPromises();

    fes.error();
    await flushPromises();

    expect(w.find("[data-testid='not-configured']").exists()).toBe(false);
    expect(w.find("[data-testid='reconnecting-banner']").exists()).toBe(true);

    // Advance past the first backoff delay (1s) so it reconnects.
    await vi.advanceTimersByTimeAsync(1000);
    await flushPromises();
    fes.open();
    await nextTick();

    expect(w.find("[data-testid='reconnecting-banner']").exists()).toBe(false);
    expect(w.find(".conn-status.live").exists()).toBe(true);

    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  // CP2: retries exhausted → disconnected state with a working manual Reconnect.
  it("retries exhausted → disconnected banner with a Reconnect button that resets backoff", async () => {
    vi.useFakeTimers();
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network error")));
    let fes;
    const factory = () => { fes = new FakeEventSource(); return fes; };
    const router = makeRouter();
    await router.push("/admin/audit");
    const w = mount(Audit, {
      global: { plugins: [router] },
      props: { eventSourceFactory: factory },
    });
    await flushPromises();

    fes.error();
    await flushPromises();

    // Drive through all retry attempts (1s,2s,4s,8s,16s — capped at 30s) by
    // letting each scheduled reconnect immediately fail again.
    for (let i = 0; i < 5; i++) {
      await vi.advanceTimersByTimeAsync(30000);
      await flushPromises();
      if (fes) fes.error();
      await flushPromises();
    }

    expect(w.find("[data-testid='disconnected-banner']").exists()).toBe(true);
    const reconnectBtn = w.find("[data-testid='reconnect-btn']");
    expect(reconnectBtn.exists()).toBe(true);

    // Manual reconnect resets backoff and reconnects successfully.
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ status: 200 }));
    await reconnectBtn.trigger("click");
    await flushPromises();
    fes.open();
    await nextTick();

    expect(w.find("[data-testid='disconnected-banner']").exists()).toBe(false);
    expect(w.find(".conn-status.live").exists()).toBe(true);

    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  // ── CP2: drill-down ─────────────────────────────────────────────────────────
  it("drill-down: click row expands audit-detail showing full context; click again collapses", async () => {
    let fes;
    const factory = () => { fes = new FakeEventSource(); return fes; };
    const router = makeRouter();
    await router.push("/admin/audit");
    const w = mount(Audit, {
      global: { plugins: [router] },
      props: { eventSourceFactory: factory },
    });
    await flushPromises();

    fes.open();
    fes.emit("message", {
      event_id: "evt-drilldown",
      event_type: "aikonos.broker.tool.denied",
      actor_user_id: "alice@example.com",
      resource_ref: "aikonos:tool:web.fetch",
      decision: 2,
      tenant_id: "tenant-a",
      occurred_at: { seconds: 1700000000, nanos: 0 },
      context: { reason: "capability_denied", scope: "web:read", extra_key: "should-be-visible" },
    });
    await nextTick();

    // Before click: no detail panel
    expect(w.find("[data-testid='audit-detail']").exists()).toBe(false);

    // Click the row
    await w.find("[data-testid='audit-row']").trigger("click");
    await nextTick();

    // After click: detail panel exists
    expect(w.find("[data-testid='audit-detail']").exists()).toBe(true);
    // Shows a context key NOT visible in the collapsed 120-char summary
    expect(w.find("[data-testid='audit-detail']").text()).toContain("extra_key");

    // Click again: collapses
    await w.find("[data-testid='audit-row']").trigger("click");
    await nextTick();
    expect(w.find("[data-testid='audit-detail']").exists()).toBe(false);
  });

  // ── CP2: CSV export (pure, no DOM) ──────────────────────────────────────────
  it("toCsv: first line is the header; a value containing a comma is quoted", () => {
    const ev1 = {
      event_type: "aikonos.broker.tool.denied",
      actor_user_id: "alice@example.com",
      resource_ref: "aikonos:tool:web.fetch",
      decision: 2,
      tenant_id: "tenant-a",
      occurred_at: { seconds: 1700000000, nanos: 0 },
    };
    const ev2 = {
      event_type: "tool.invoked",
      actor_user_id: "bob@example.com",
      resource_ref: "aikonos:tool:doc,write",  // contains a comma — must be quoted
      decision: 1,
      tenant_id: "tenant-b",
      occurred_at: { seconds: 1700000001, nanos: 0 },
    };
    const csv = toCsv([ev1, ev2]);
    const lines = csv.split("\n");
    expect(lines[0]).toBe("time,event_type,actor,resource_ref,decision,tenant_id");
    // The resource_ref with a comma must be wrapped in quotes
    expect(lines[2]).toContain('"aikonos:tool:doc,write"');
  });

  // ── actor_email precedence: human label preferred over the stable oid ───────
  it("toCsv actor column prefers actor_email over actor_user_id (oid)", () => {
    const ev = {
      event_type: "aikonos.broker.task.created",
      actor_user_id: "8f3c-oid-guid",        // stable oid
      actor_email: "alice@example.com",        // human label
      resource_ref: "aikonos:task:1",
      decision: 1,
      tenant_id: "t",
      occurred_at: { seconds: 1700000000, nanos: 0 },
    };
    const cols = toCsv([ev]).split("\n")[1].split(",");
    // header: time,event_type,actor,resource_ref,decision,tenant_id → actor is index 2
    expect(cols[2]).toBe("alice@example.com");
  });

  it("toCsv actor column falls back to actor_user_id when no email", () => {
    const ev = {
      event_type: "aikonos.broker.rpc",
      actor_user_id: "8f3c-oid-guid",
      resource_ref: "/x",
      decision: 1,
      tenant_id: "t",
      occurred_at: { seconds: 1700000000, nanos: 0 },
    };
    const cols = toCsv([ev]).split("\n")[1].split(",");
    expect(cols[2]).toBe("8f3c-oid-guid");
  });

  // ── CP2: JSON export (pure, no DOM) ─────────────────────────────────────────
  it("toJson: parses back to array of length 1 with the event's event_type", () => {
    const ev = {
      event_type: "aikonos.broker.admin.access.denied",
      actor_user_id: "bad@example.com",
      resource_ref: "aikonos:tenant:my-tenant",
      decision: 2,
      tenant_id: "my-tenant",
    };
    const parsed = JSON.parse(toJson([ev]));
    expect(parsed).toHaveLength(1);
    expect(parsed[0].event_type).toBe("aikonos.broker.admin.access.denied");
  });

  // ── CP2: tenant filter ───────────────────────────────────────────────────────
  it("tenant filter: selecting a tenant_id narrows rows; All tenants restores both", async () => {
    let fes;
    const factory = () => { fes = new FakeEventSource(); return fes; };
    const router = makeRouter();
    await router.push("/admin/audit");
    const w = mount(Audit, {
      global: { plugins: [router] },
      props: { eventSourceFactory: factory },
    });
    await flushPromises();

    fes.open();
    fes.emit("message", {
      event_id: "evt-ta",
      event_type: "tool.invoked",
      actor_user_id: "alice@example.com",
      resource_ref: "res:a",
      decision: 1,
      tenant_id: "tenant-alpha",
      occurred_at: { seconds: 1700000000, nanos: 0 },
    });
    fes.emit("message", {
      event_id: "evt-tb",
      event_type: "tool.invoked",
      actor_user_id: "bob@example.com",
      resource_ref: "res:b",
      decision: 1,
      tenant_id: "tenant-beta",
      occurred_at: { seconds: 1700000001, nanos: 0 },
    });
    await nextTick();

    // Both rows visible initially
    expect(w.findAll("[data-testid='audit-row']").length).toBe(2);

    // Select tenant-alpha
    const sel = w.find("[data-testid='tenant-filter']");
    expect(sel.exists()).toBe(true);
    await sel.setValue("tenant-alpha");
    await nextTick();
    expect(w.findAll("[data-testid='audit-row']").length).toBe(1);
    expect(w.text()).toContain("alice@example.com");
    expect(w.text()).not.toContain("bob@example.com");

    // Reset to All tenants
    await sel.setValue("");
    await nextTick();
    expect(w.findAll("[data-testid='audit-row']").length).toBe(2);
  });

  // CP2 (F44): selecting a tenant reconnects the SSE with ?tenant= via the
  // existing connect/backoff machinery; clearing it reconnects unfiltered.
  it("tenant filter: selecting a tenant reconnects the stream with ?tenant= in the URL", async () => {
    const urls = [];
    let fes;
    const factory = (url) => { urls.push(url); fes = new FakeEventSource(); return fes; };
    const router = makeRouter();
    await router.push("/admin/audit");
    const w = mount(Audit, {
      global: { plugins: [router] },
      props: { eventSourceFactory: factory },
    });
    await flushPromises();
    fes.open();
    fes.emit("message", {
      event_id: "evt-tf",
      event_type: "tool.invoked",
      actor_user_id: "alice@example.com",
      resource_ref: "res:a",
      decision: 1,
      tenant_id: "tenant-alpha",
      occurred_at: { seconds: 1700000000, nanos: 0 },
    });
    await nextTick();

    expect(urls.at(-1)).toBe("/audit/stream");

    const sel = w.find("[data-testid='tenant-filter']");
    await sel.setValue("tenant-alpha");
    await nextTick();
    await flushPromises();

    expect(urls.at(-1)).toBe("/audit/stream?tenant=tenant-alpha");

    await sel.setValue("");
    await nextTick();
    await flushPromises();

    expect(urls.at(-1)).toBe("/audit/stream");
  });

  // CP2 (F44 follow-up): a slow classifyAndHandleFailure() probe that resolves
  // AFTER a tenant-filter change has already reconnected must not disrupt the
  // fresh connection (stale-generation no-op).
  it("stale failure-classification probe does not disrupt a connection re-established by a tenant-filter change", async () => {
    let resolveProbe;
    vi.stubGlobal("fetch", vi.fn(() => new Promise((resolve) => { resolveProbe = resolve; })));
    const feses = [];
    const factory = () => { const f = new FakeEventSource(); feses.push(f); return f; };
    const router = makeRouter();
    await router.push("/admin/audit");
    const w = mount(Audit, {
      global: { plugins: [router] },
      props: { eventSourceFactory: factory },
    });
    await flushPromises();

    feses[0].open();
    // Populate the tenant <select> before the connection drops.
    feses[0].emit("message", {
      event_id: "evt-x",
      event_type: "tool.invoked",
      actor_user_id: "alice@example.com",
      resource_ref: "res:a",
      decision: 1,
      tenant_id: "tenant-alpha",
      occurred_at: { seconds: 1700000000, nanos: 0 },
    });
    await nextTick();

    // Connection drops — kicks off the (slow, still-pending) classification probe.
    feses[0].error();
    await flushPromises();

    // Before the probe resolves, a tenant-filter change reconnects.
    const sel = w.find("[data-testid='tenant-filter']");
    await sel.setValue("tenant-alpha");
    await nextTick();
    feses[1].open();
    await nextTick();

    expect(w.find(".conn-status.live").exists()).toBe(true);
    expect(w.find("[data-testid='reconnecting-banner']").exists()).toBe(false);

    // The stale probe now resolves with a non-501 status — must no-op.
    resolveProbe({ status: 200 });
    await flushPromises();

    expect(w.find(".conn-status.live").exists()).toBe(true);
    expect(w.find("[data-testid='reconnecting-banner']").exists()).toBe(false);
    expect(w.find("[data-testid='not-configured']").exists()).toBe(false);

    vi.unstubAllGlobals();
  });

  // CP2: "lost after open" now goes through the same retry path as a pre-open
  // failure (replaces the old permanent `permanentFailure` state) — it shows the
  // reconnecting banner first, and reconnects on the next successful open.
  it("lost after open: shows reconnecting (not not-configured), then reconnects on next open", async () => {
    vi.useFakeTimers();
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network error")));
    let fes;
    const factory = () => { fes = new FakeEventSource(); return fes; };
    const router = makeRouter();
    await router.push("/admin/audit");
    const w = mount(Audit, {
      global: { plugins: [router] },
      props: { eventSourceFactory: factory },
    });
    await flushPromises();

    // Successful open, then a message arrives
    fes.open();
    fes.emit("message", {
      event_id: "evt-2",
      event_type: "tool.invoked",
      actor_user_id: "bob@example.com",
      resource_ref: "task:t2",
      decision: 1,
      occurred_at: { seconds: 1700000001, nanos: 0 },
    });
    await nextTick();

    // Confirm rows are rendered before the disconnect
    expect(w.findAll("[data-testid='audit-row']").length).toBe(1);

    // Now the connection drops
    fes.error();
    await flushPromises();

    // Must NOT show not-configured; must show reconnecting, rows stay visible.
    expect(w.find("[data-testid='not-configured']").exists()).toBe(false);
    expect(w.findAll("[data-testid='audit-row']").length).toBe(1);
    expect(w.find("[data-testid='reconnecting-banner']").exists()).toBe(true);

    // A later successful open reconnects.
    await vi.advanceTimersByTimeAsync(1000);
    await flushPromises();
    fes.open();
    await nextTick();

    expect(w.find("[data-testid='reconnecting-banner']").exists()).toBe(false);
    expect(w.find(".conn-status.live").exists()).toBe(true);

    vi.unstubAllGlobals();
    vi.useRealTimers();
  });
});
