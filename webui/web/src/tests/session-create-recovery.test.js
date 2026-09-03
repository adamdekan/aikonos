// CP4 test: a failed session-create write must not leave activeSessionId bound
// to a session that was never written (which would silently no-op every later
// persistSessionMessages() for the rest of the tab's life), and must surface a
// toast instead of a bare console.warn. A subsequent successful create must recover.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { computed, ref } from "vue";
import { createPinia, setActivePinia } from "pinia";

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
  writeManifest: vi.fn().mockResolvedValue(undefined),
  migrateLegacySessions: vi.fn().mockResolvedValue(undefined),
}));

import * as sessionsMod from "../api/sessions.js";
import { useSessionsStore } from "../store/sessions.js";
import { useChatStore } from "../store/chat.js";
import { useSessionLifecycle } from "../composables/useSessionLifecycle.js";
import { useToast } from "../components/ui/useToast.js";

beforeEach(() => {
  vi.clearAllMocks();
  setActivePinia(createPinia());
  useToast().toasts.splice(0); // drain the module-level toast queue between tests
});

function makeLifecycle(chatStore, sessionsStore) {
  const route = { query: {} };
  const agentId = computed(() => null);
  const agentName = () => null;
  const messages = computed(() => chatStore.currentBuffer().messages);
  return useSessionLifecycle({ route, chatStore, sessionsStore, agentId, agentName, messages });
}

describe("store/sessions.js — createSession failure signal", () => {
  it("returns false and leaves the manifest untouched when writeSession rejects", async () => {
    sessionsMod.writeSession.mockRejectedValueOnce(new Error("network down"));
    const sessionsStore = useSessionsStore();
    const ok = await sessionsStore.createSession({ id: "s1", title: "t", messages: [] });
    expect(ok).toBe(false);
    expect(sessionsStore.sessions.find((s) => s.id === "s1")).toBeUndefined();
  });

  it("returns true and adds a manifest entry when writeSession succeeds", async () => {
    sessionsMod.writeSession.mockResolvedValueOnce(undefined);
    const sessionsStore = useSessionsStore();
    const ok = await sessionsStore.createSession({ id: "s2", title: "t", messages: [] });
    expect(ok).toBe(true);
    expect(sessionsStore.sessions.find((s) => s.id === "s2")).toBeTruthy();
  });
});

describe("useSessionLifecycle — maybeCreateSession recovery (CP4)", () => {
  it("does not bind activeSessionId and toasts when the create write fails, then recovers on a later successful create", async () => {
    const chatStore = useChatStore();
    const sessionsStore = useSessionsStore();
    const { maybeCreateSession, persistSessionMessages } = makeLifecycle(chatStore, sessionsStore);

    // First turn: the write fails.
    sessionsMod.writeSession.mockRejectedValueOnce(new Error("disk full"));
    chatStore.currentBuffer().messages.push({ role: "user", text: "first message" });
    await maybeCreateSession("first message");

    expect(chatStore.activeSessionId).toBeNull();
    const toasts = useToast().toasts;
    expect(toasts.some((t) => t.type === "error")).toBe(true);

    // Second turn: activeSessionId is still null (not permanently stuck), so a new
    // attempt fires and this time the write succeeds.
    sessionsMod.writeSession.mockResolvedValueOnce(undefined);
    chatStore.currentBuffer().messages.push({ role: "user", text: "second message" });
    await maybeCreateSession("first message");

    expect(chatStore.activeSessionId).not.toBeNull();
    const boundId = chatStore.activeSessionId;
    expect(sessionsStore.sessions.find((s) => s.id === boundId)).toBeTruthy();

    // Persistence now works for the rest of the tab.
    sessionsMod.writeSession.mockResolvedValueOnce(undefined);
    await persistSessionMessages();
    expect(sessionsMod.writeSession).toHaveBeenCalled();
  });
});
