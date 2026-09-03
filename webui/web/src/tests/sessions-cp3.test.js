// CP3 tests: additive reconcile + scheduled badge.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";

// ---------------------------------------------------------------------------
// Module mocks (must appear before imports that trigger them)
// ---------------------------------------------------------------------------

vi.mock("../api/client.js", () => ({
  get: vi.fn(),
  post: vi.fn(),
  del: vi.fn(),
  patch: vi.fn(),
}));

vi.mock("../api/sessions.js", () => ({
  listSessionFiles: vi.fn(),
  readSession: vi.fn(),
  writeSession: vi.fn(),
  deleteSession: vi.fn(),
  readManifest: vi.fn(),
  writeManifest: vi.fn(),
  migrateLegacySessions: vi.fn().mockResolvedValue(undefined),
}));

import * as sessionsMod from "../api/sessions.js";
import { useSessionsStore } from "../store/sessions.js";
import SessionItem from "../components/SessionItem.vue";

// ---------------------------------------------------------------------------
// Helper: stub router for SessionItem (it calls useRouter)
// ---------------------------------------------------------------------------
function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [{ path: "/", component: { template: "<div/>" } }],
  });
}

beforeEach(() => {
  vi.clearAllMocks();
  setActivePinia(createPinia());
});

// ---------------------------------------------------------------------------
// (a) Additive reconcile
// ---------------------------------------------------------------------------
describe("sessions store — additive reconcile", () => {
  it("surfaces an on-disk session absent from the manifest", async () => {
    // Disk: two session files
    sessionsMod.listSessionFiles.mockResolvedValue([
      { path: ".agent/Sessions/known.json" },
      { path: ".agent/Sessions/sched-001.json" },
    ]);
    // Manifest: only the known entry
    sessionsMod.readManifest.mockResolvedValue([
      {
        id: "known",
        title: "Known",
        agent_id: null,
        agent_name: null,
        pinned: false,
        pinned_at: null,
        created_at: "2026-01-01T00:00:00Z",
        updated_at: "2026-01-01T00:00:00Z",
        source: null,
        schedule_id: null,
      },
    ]);
    // readSession for the unknown file returns a scheduled record
    sessionsMod.readSession.mockImplementation(async (id) => {
      if (id === "sched-001") {
        return {
          id: "sched-001",
          title: "Daily report",
          agent_id: null,
          agent_name: null,
          pinned: false,
          pinned_at: null,
          created_at: "2026-06-16T06:00:00Z",
          updated_at: "2026-06-16T06:05:00Z",
          source: "schedule",
          schedule_id: "sch-abc",
          run_at: "2026-06-16T06:00:00Z",
        };
      }
      return null;
    });
    sessionsMod.writeManifest.mockResolvedValue({});

    const store = useSessionsStore();
    await store.load();

    const ids = store.sessions.map((s) => s.id);
    expect(ids).toContain("sched-001");
    expect(ids).toContain("known");

    const schedEntry = store.sessions.find((s) => s.id === "sched-001");
    expect(schedEntry.source).toBe("schedule");
    expect(schedEntry.schedule_id).toBe("sch-abc");
  });

  it("prunes a manifest entry whose file is absent", async () => {
    // Disk: only one file
    sessionsMod.listSessionFiles.mockResolvedValue([
      { path: ".agent/Sessions/alive.json" },
    ]);
    // Manifest: two entries — one whose file exists, one orphaned
    sessionsMod.readManifest.mockResolvedValue([
      {
        id: "alive",
        title: "Alive",
        agent_id: null,
        agent_name: null,
        pinned: false,
        pinned_at: null,
        created_at: "2026-01-01T00:00:00Z",
        updated_at: "2026-01-01T00:00:00Z",
        source: null,
        schedule_id: null,
      },
      {
        id: "ghost",
        title: "Ghost",
        agent_id: null,
        agent_name: null,
        pinned: false,
        pinned_at: null,
        created_at: "2026-01-01T00:00:00Z",
        updated_at: "2026-01-01T00:00:00Z",
        source: null,
        schedule_id: null,
      },
    ]);
    sessionsMod.readSession.mockResolvedValue(null);
    sessionsMod.writeManifest.mockResolvedValue({});

    const store = useSessionsStore();
    await store.load();

    const ids = store.sessions.map((s) => s.id);
    expect(ids).toContain("alive");
    expect(ids).not.toContain("ghost");
  });

  it("preserves existing manifest entries when no new files are found", async () => {
    sessionsMod.listSessionFiles.mockResolvedValue([
      { path: ".agent/Sessions/a.json" },
      { path: ".agent/Sessions/b.json" },
    ]);
    sessionsMod.readManifest.mockResolvedValue([
      {
        id: "a",
        title: "A",
        agent_id: null,
        agent_name: null,
        pinned: false,
        pinned_at: null,
        created_at: "2026-01-01T00:00:00Z",
        updated_at: "2026-01-01T00:00:00Z",
        source: null,
        schedule_id: null,
      },
      {
        id: "b",
        title: "B",
        agent_id: null,
        agent_name: null,
        pinned: false,
        pinned_at: null,
        created_at: "2026-01-02T00:00:00Z",
        updated_at: "2026-01-02T00:00:00Z",
        source: null,
        schedule_id: null,
      },
    ]);
    sessionsMod.writeManifest.mockResolvedValue({});

    const store = useSessionsStore();
    await store.load();

    const ids = store.sessions.map((s) => s.id);
    expect(ids).toContain("a");
    expect(ids).toContain("b");
  });

  it("swallows parse failures for on-disk-only files and continues", async () => {
    sessionsMod.listSessionFiles.mockResolvedValue([
      { path: ".agent/Sessions/bad.json" },
      { path: ".agent/Sessions/good.json" },
    ]);
    sessionsMod.readManifest.mockResolvedValue([]);
    sessionsMod.readSession.mockImplementation(async (id) => {
      if (id === "bad") return null; // simulate corrupt/unreadable — null return path
      return {
        id: "good",
        title: "Good",
        agent_id: null,
        agent_name: null,
        pinned: false,
        pinned_at: null,
        created_at: "2026-06-16T00:00:00Z",
        updated_at: "2026-06-16T00:00:00Z",
        source: null,
        schedule_id: null,
      };
    });
    sessionsMod.writeManifest.mockResolvedValue({});

    const store = useSessionsStore();
    await store.load();

    const ids = store.sessions.map((s) => s.id);
    expect(ids).toContain("good");
    expect(ids).not.toContain("bad");
  });

  it("swallows readSession throws during additive reconcile and surfaces other sessions", async () => {
    sessionsMod.listSessionFiles.mockResolvedValue([
      { path: ".agent/Sessions/corrupt.json" },
      { path: ".agent/Sessions/valid.json" },
    ]);
    sessionsMod.readManifest.mockResolvedValue([]);
    sessionsMod.readSession.mockImplementation(async (id) => {
      if (id === "corrupt") throw new Error("corrupt");
      return {
        id: "valid",
        title: "Valid",
        agent_id: null,
        agent_name: null,
        pinned: false,
        pinned_at: null,
        created_at: "2026-06-16T00:00:00Z",
        updated_at: "2026-06-16T00:00:00Z",
        source: null,
        schedule_id: null,
      };
    });
    sessionsMod.writeManifest.mockResolvedValue({});

    const store = useSessionsStore();
    await store.load(); // must not throw

    const ids = store.sessions.map((s) => s.id);
    expect(ids).toContain("valid");
    expect(ids).not.toContain("corrupt");
  });

  it("persists merged manifest when new entries are discovered", async () => {
    sessionsMod.listSessionFiles.mockResolvedValue([
      { path: ".agent/Sessions/existing.json" },
      { path: ".agent/Sessions/new-one.json" },
    ]);
    sessionsMod.readManifest.mockResolvedValue([
      {
        id: "existing",
        title: "Existing",
        agent_id: null,
        agent_name: null,
        pinned: false,
        pinned_at: null,
        created_at: "2026-01-01T00:00:00Z",
        updated_at: "2026-01-01T00:00:00Z",
        source: null,
        schedule_id: null,
      },
    ]);
    sessionsMod.readSession.mockImplementation(async (id) => {
      if (id === "new-one") {
        return {
          id: "new-one",
          title: "New One",
          agent_id: null,
          agent_name: null,
          pinned: false,
          pinned_at: null,
          created_at: "2026-06-16T00:00:00Z",
          updated_at: "2026-06-16T00:00:00Z",
          source: "schedule",
          schedule_id: "sch-x",
          run_at: "2026-06-16T00:00:00Z",
        };
      }
      return null;
    });
    sessionsMod.writeManifest.mockResolvedValue({});

    const store = useSessionsStore();
    await store.load();

    // writeManifest must have been called (manifest changed)
    expect(sessionsMod.writeManifest).toHaveBeenCalled();
    const written = sessionsMod.writeManifest.mock.calls[0][0];
    const writtenIds = written.map((e) => e.id);
    expect(writtenIds).toContain("existing");
    expect(writtenIds).toContain("new-one");
  });
});

// ---------------------------------------------------------------------------
// (b) manifestEntry carries source + schedule_id
// ---------------------------------------------------------------------------
describe("manifestEntry — source and schedule_id fields", () => {
  it("source:schedule is present in the store entry after additive reconcile", async () => {
    sessionsMod.listSessionFiles.mockResolvedValue([
      { path: ".agent/Sessions/sched-99.json" },
    ]);
    sessionsMod.readManifest.mockResolvedValue([]);
    sessionsMod.readSession.mockResolvedValue({
      id: "sched-99",
      title: "Sched run",
      agent_id: null,
      agent_name: null,
      pinned: false,
      pinned_at: null,
      created_at: "2026-06-16T10:00:00Z",
      updated_at: "2026-06-16T10:05:00Z",
      source: "schedule",
      schedule_id: "sch-99",
      run_at: "2026-06-16T10:00:00Z",
    });
    sessionsMod.writeManifest.mockResolvedValue({});

    const store = useSessionsStore();
    await store.load();

    const entry = store.sessions.find((s) => s.id === "sched-99");
    expect(entry.source).toBe("schedule");
    expect(entry.schedule_id).toBe("sch-99");
  });

  it("source:null when record has no source", async () => {
    sessionsMod.listSessionFiles.mockResolvedValue([
      { path: ".agent/Sessions/plain.json" },
    ]);
    sessionsMod.readManifest.mockResolvedValue([]);
    sessionsMod.readSession.mockResolvedValue({
      id: "plain",
      title: "Plain session",
      agent_id: null,
      agent_name: null,
      pinned: false,
      pinned_at: null,
      created_at: "2026-06-16T00:00:00Z",
      updated_at: "2026-06-16T00:00:00Z",
    });
    sessionsMod.writeManifest.mockResolvedValue({});

    const store = useSessionsStore();
    await store.load();

    const entry = store.sessions.find((s) => s.id === "plain");
    expect(entry.source).toBeNull();
    expect(entry.schedule_id).toBeNull();
  });
});

// ---------------------------------------------------------------------------
// (c) SessionItem "Scheduled" badge
// ---------------------------------------------------------------------------
describe("SessionItem — Scheduled badge", () => {
  it("renders the scheduled badge when source is 'schedule'", async () => {
    const router = makeRouter();
    const session = {
      id: "sched-001",
      title: "Daily report",
      agent_id: null,
      agent_name: null,
      pinned: false,
      source: "schedule",
      schedule_id: "sch-abc",
    };
    const wrapper = mount(SessionItem, {
      props: { session },
      global: { plugins: [router, createPinia()] },
    });
    expect(wrapper.find(".sched-badge").exists()).toBe(true);
  });

  it("does not render the scheduled badge when source is not 'schedule'", async () => {
    const router = makeRouter();
    const session = {
      id: "normal-001",
      title: "Normal session",
      agent_id: null,
      agent_name: null,
      pinned: false,
      source: null,
      schedule_id: null,
    };
    const wrapper = mount(SessionItem, {
      props: { session },
      global: { plugins: [router, createPinia()] },
    });
    expect(wrapper.find(".sched-badge").exists()).toBe(false);
  });

  it("does not render the scheduled badge when source is absent", async () => {
    const router = makeRouter();
    const session = {
      id: "legacy-001",
      title: "Legacy session",
      agent_id: null,
      agent_name: null,
      pinned: false,
    };
    const wrapper = mount(SessionItem, {
      props: { session },
      global: { plugins: [router, createPinia()] },
    });
    expect(wrapper.find(".sched-badge").exists()).toBe(false);
  });

  it("scheduled badge does not collide with agent badge (both can coexist structurally)", async () => {
    const router = makeRouter();
    // Structural independence test only: agent_id non-null forces agent badge visible alongside
    // sched-badge. Agent-bound scheduled runs are out of scope per spec; this just verifies the
    // two badge elements don't share a slot that prevents co-rendering.
    const session = {
      id: "sched-agent",
      title: "Agent sched session",
      agent_id: "agent-007",
      agent_name: "Bond",
      pinned: false,
      source: "schedule",
      schedule_id: "sch-007",
    };
    const wrapper = mount(SessionItem, {
      props: { session },
      global: { plugins: [router, createPinia()] },
    });
    expect(wrapper.find(".sched-badge").exists()).toBe(true);
    expect(wrapper.find(".agent-badge").exists()).toBe(true);
  });
});
