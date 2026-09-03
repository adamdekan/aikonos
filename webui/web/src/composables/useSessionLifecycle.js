import { watch } from "vue";
import { uuid } from "../lib/uuid.js";
import { writeSession } from "../api/sessions.js";
import { useToast } from "../components/ui/useToast.js";

// Session-file lifecycle: creating a session record on first send, persisting
// the live buffer to disk, resuming from ?session= on mount/navigation, and
// the route watcher that re-resumes when the session/agent query changes
// (component instance is reused across chat navigations, so this can't rely
// on remount).
//
// route/chatStore/sessionsStore: the usual store/router instances.
// agentId: computed<string> — the ?agent= query, read for record fields.
// agentName: fn() => string — resolves the display name for agentId (via agentsStore).
// messages: computed<Array> — the active buffer's live message list.
export function useSessionLifecycle({ route, chatStore, sessionsStore, agentId, agentName, messages }) {
  function titleFrom(text) {
    const words = text.trim().split(/\s+/);
    const sixWords = words.slice(0, 6).join(" ");
    return sixWords.length > 40 ? sixWords.slice(0, 40) : sixWords;
  }

  async function maybeCreateSession(firstMessage) {
    if (chatStore.activeSessionId !== null) return;
    // Concurrent safety depends on the caller's synchronous running guard, since promoteDraft
    // is called after the await (activeSessionId stays null across the await).
    const id = uuid();
    const now = new Date().toISOString();
    // Build the record from the still-unpromoted draft buffer — only bind
    // activeSessionId to `id` (via promoteDraft) once the write has actually
    // succeeded, so a failure never leaves the tab pointed at a session file
    // that doesn't exist (which would silently no-op persistSessionMessages
    // for the rest of the tab's life).
    const buf = chatStore.currentBuffer();
    const record = {
      id,
      title: titleFrom(firstMessage),
      agent_id: agentId.value || null,
      agent_name: agentName(),
      pinned: false,
      pinned_at: null,
      created_at: now,
      updated_at: now,
      thread_id: buf.threadId,
      first_message: firstMessage,
      messages: [...buf.messages],
    };
    const ok = await sessionsStore.createSession(record);
    if (ok) {
      // Move the draft buffer to buffers[id] (preserves threadId + messages identity).
      chatStore.promoteDraft(id);
    } else {
      useToast().push("error", "couldn't save this conversation — will retry on your next message");
    }
  }

  function firstUserMessageText() {
    const first = messages.value.find((m) => m.role === "user");
    return first ? first.text : "";
  }

  async function persistSessionMessages() {
    const id = chatStore.activeSessionId;
    if (!id) return;
    // Common case: manifest already has the entry — no read. Rare case: a fresh
    // resume-by-query (see resumeFromQuery) can activate a session before the
    // manifest array is populated (or after a failed concurrent load() left it
    // empty); retry via load() (memoized), then fall back to reading the
    // session file directly rather than silently no-op'ing forever.
    let entry = sessionsStore.sessions.find((s) => s.id === id);
    if (!entry) {
      await sessionsStore.load();
      entry = sessionsStore.sessions.find((s) => s.id === id);
    }
    let base;
    if (entry) {
      base = {
        id: entry.id,
        title: entry.title,
        agent_id: entry.agent_id,
        agent_name: entry.agent_name,
        pinned: entry.pinned,
        pinned_at: entry.pinned_at,
        created_at: entry.created_at,
        source: entry.source,
        schedule_id: entry.schedule_id,
      };
    } else {
      let record = null;
      try {
        record = await sessionsStore.loadMessages(id);
      } catch {
        record = null;
      }
      if (!record) {
        console.warn("session persist skipped: no session record", id);
        return;
      }
      base = {
        id: record.id,
        title: record.title,
        agent_id: record.agent_id,
        agent_name: record.agent_name,
        pinned: record.pinned,
        pinned_at: record.pinned_at,
        created_at: record.created_at,
        source: record.source,
        schedule_id: record.schedule_id,
      };
    }
    const buf = chatStore.currentBuffer();
    const updated = {
      ...base,
      updated_at: new Date().toISOString(),
      thread_id: buf.threadId,
      first_message: firstUserMessageText(),
      messages: [...messages.value],
    };
    try {
      await writeSession(updated);
      await sessionsStore.upsertFromRecord(updated);
    } catch (err) {
      console.warn("session persist failed", err);
    }
  }

  // Load a session's messages into the live buffer. Shared by the mount path and the
  // route-watcher: navigating between sessions reuses this component instance (no remount),
  // so the watcher is what re-resumes on a query change — without it the view keeps showing
  // the previous conversation until a full page reload.
  async function resumeFromQuery(sessionQueryId) {
    if (!sessionQueryId) return;
    try {
      const rec = await sessionsStore.loadMessages(sessionQueryId);
      if (rec) {
        // hydrateBuffer also sets activeSessionId = sessionQueryId.
        chatStore.hydrateBuffer(sessionQueryId, {
          threadId: rec.thread_id ?? rec.threadId,
          messages: rec.messages,
        });
      }
    } catch {
      // Resume failure must not break the view; leave fresh buffer.
    }
  }

  // Tracks the session id currently hydrated into the live buffer so an agent-only
  // switch that keeps the same ?session= doesn't re-hydrate and clobber unsaved edits.
  let resumedSessionId = null;

  // Watch both session and agent: an agent-only switch (/chat?agent=A → /chat?agent=B)
  // does not change `session`, so watching session alone would miss it and leave a stale
  // activeSessionId pointing at the previously-resumed session file.
  watch(() => [route.query.session, route.query.agent], ([id]) => {
    const sessionId = id || null;
    if (sessionId) {
      // Only re-resume when the session id actually changed; an agent-only change
      // with the same session must not reload and overwrite in-progress buffer edits.
      if (sessionId !== resumedSessionId) {
        resumedSessionId = sessionId;
        resumeFromQuery(sessionId);
      }
    } else {
      // Sessionless navigation (New-chat button, agent nav, bare /chat) — start fresh.
      resumedSessionId = null;
      chatStore.clearActiveSession();
      chatStore.reset();
    }
  });

  // Initial resume-or-reset on mount, mirroring the watcher's branches.
  async function initResume() {
    if (route.query.session) {
      // Resume session from route query if present.
      resumedSessionId = route.query.session;
      await resumeFromQuery(route.query.session);
    } else {
      // Direct load with no ?session= → start clean (mirrors New-chat button).
      resumedSessionId = null;
      chatStore.clearActiveSession();
      chatStore.reset();
    }
  }

  return {
    maybeCreateSession,
    persistSessionMessages,
    resumeFromQuery,
    initResume,
  };
}
