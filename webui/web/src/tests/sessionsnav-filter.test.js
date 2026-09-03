// F63 (CP3): SessionsNav title filter — case-insensitive substring, composes with
// scheduledOnly, and bypasses pagination (scans the full loaded list, not just the
// cursor-limited page). Empty filter must render identically to today (parity pin).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { mount } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useSessionsStore } from "../store/sessions.js";
import SessionsNav from "../components/SessionsNav.vue";

vi.mock("../api/sessions.js", () => ({
  listSessionFiles: vi.fn().mockResolvedValue([]),
  readSession: vi.fn().mockResolvedValue(null),
  writeSession: vi.fn().mockResolvedValue({}),
  deleteSession: vi.fn().mockResolvedValue({}),
  readManifest: vi.fn().mockResolvedValue([]),
  writeManifest: vi.fn().mockResolvedValue({}),
  migrateLegacySessions: vi.fn().mockResolvedValue(undefined),
}));

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [{ path: "/", component: { template: "<div/>" } }],
  });
}

function makeSession(id, title, opts = {}) {
  return {
    id,
    title,
    pinned: false,
    pinned_at: null,
    updated_at: "2026-01-01T00:00:00Z",
    source: null,
    schedule_id: null,
    agent_id: null,
    agent_name: null,
    ...opts,
  };
}

describe("SessionsNav — title filter", () => {
  let store;

  beforeEach(() => {
    setActivePinia(createPinia());
    store = useSessionsStore();
    // Pre-seeded + loaded=true so onMounted's load() call is a no-op guard hit
    // and doesn't overwrite the fixture with the mocked (empty) API response.
    store.loaded = true;
  });

  function seed(sessions, cursor = 10) {
    store.sessions = sessions;
    store.cursor = cursor;
  }

  async function mountNav() {
    const router = makeRouter();
    await router.push("/");
    const w = mount(SessionsNav, { global: { plugins: [router] } });
    await w.vm.$nextTick();
    return w;
  }

  it("empty filter renders today's paged list unchanged (parity)", async () => {
    // Fixture must exceed the cursor so an empty filter that accidentally scanned
    // the full list (instead of falling back to sessionsStore.visible) would be
    // caught: "Beyond" (item 14) sits past cursor=10 and must stay hidden.
    const many = Array.from({ length: 15 }, (_, i) =>
      makeSession(`s${i}`, i === 14 ? "Beyond" : `Session ${i}`)
    );
    seed(many, 10);
    const w = await mountNav();
    expect(w.text()).toContain("Session 0");
    expect(w.text()).toContain("Session 9");
    expect(w.text()).not.toContain("Beyond");

    await w.find("[data-testid='session-filter-input']").setValue("beyond");
    expect(w.text()).toContain("Beyond");
  });

  it("filters by title case-insensitively", async () => {
    seed([makeSession("a", "Weather report"), makeSession("b", "Recipe idea")]);
    const w = await mountNav();
    await w.find("[data-testid='session-filter-input']").setValue("WEATH");
    expect(w.text()).toContain("Weather report");
    expect(w.text()).not.toContain("Recipe idea");
  });

  it("filter bypasses pagination — matches beyond the cursor-limited page are shown", async () => {
    const many = Array.from({ length: 15 }, (_, i) =>
      makeSession(`s${i}`, i === 14 ? "Needle" : `Session ${i}`)
    );
    seed(many, 10); // cursor=10; "Needle" is item 14, beyond the paged slice
    const w = await mountNav();
    expect(w.text()).not.toContain("Needle"); // sanity: paged view hides it

    await w.find("[data-testid='session-filter-input']").setValue("needle");
    expect(w.text()).toContain("Needle");
  });

  it("filter composes with the scheduled-only toggle", async () => {
    seed([
      makeSession("a", "Weather", { source: "schedule" }),
      makeSession("b", "Weather manual", { source: null }),
      makeSession("c", "Other scheduled", { source: "schedule" }),
    ]);
    const w = await mountNav();
    await w.find(".sched-filter").trigger("click");
    await w.find("[data-testid='session-filter-input']").setValue("weather");

    expect(w.text()).toContain("Weather");
    expect(w.text()).not.toContain("Weather manual");
    expect(w.text()).not.toContain("Other scheduled");
  });
});
