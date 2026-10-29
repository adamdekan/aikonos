import { defineStore } from "pinia";
import { reactive, ref } from "vue";
import { uuid } from "../lib/uuid.js";
import { useToast } from "../components/ui/useToast.js";
import { usePrefsStore } from "./prefs.js";

// L-26: the stored value is { owner, buffers }, the buffers together with the
// OIDC subject they belong to, so no tab can store one user's buffers under
// another's name. Another subject's value, or one without an owner (older
// builds), is never shown.
export const CHAT_STORAGE_KEY = "aikonos.chatSessions";

// Persisted buffers keep a bounded slice of history — full transcripts (incl. tool
// results) live only in the in-memory reactive buffer and in the durable session file.
const PERSIST_MAX_RESULT_BYTES = 2048;
const TRUNCATED_RESULT_STUB = JSON.stringify({
  truncated: true,
  note: "tool result truncated for local storage",
});

// Build a truncated, storage-safe copy of `buffers`. Never mutates the source.
function serializeForStorage(buffers, maxMessages) {
  const out = {};
  for (const key of Object.keys(buffers)) {
    const buf = buffers[key];
    const messages = Array.isArray(buf.messages) ? buf.messages : [];
    out[key] = {
      threadId: buf.threadId,
      messages: messages.slice(-maxMessages).map((msg) => {
        if (!Array.isArray(msg.tools) || msg.tools.length === 0) return msg;
        return {
          ...msg,
          tools: msg.tools.map((tool) => {
            if (tool.result === null || tool.result === undefined) return tool;
            if (JSON.stringify(tool.result).length <= PERSIST_MAX_RESULT_BYTES) return tool;
            return { ...tool, result: TRUNCATED_RESULT_STUB };
          }),
        };
      }),
    };
  }
  return out;
}

// The stored { owner, buffers }, or null when there is none or it has another
// shape (an older build's bare map, the user::agentId schema).
function readStored() {
  try {
    const parsed = JSON.parse(localStorage.getItem(CHAT_STORAGE_KEY));
    if (typeof parsed?.owner === "string" && typeof parsed.buffers === "object" && parsed.buffers !== null) {
      return parsed;
    }
  } catch {
    /* unreadable or unavailable: nothing stored */
  }
  return null;
}

function removeStored() {
  try {
    localStorage.removeItem(CHAT_STORAGE_KEY);
  } catch (e) {
    console.warn("[chat] localStorage removeItem failed", e);
  }
}

function loadFromStorage() {
  const stored = readStored();
  if (stored) return stored;
  removeStored(); // an unowned leftover is nobody's to show
  return { owner: null, buffers: {} };
}

export const useChatStore = defineStore("chat", () => {
  const stored = loadFromStorage();
  // The subject the buffers belong to: the stored one until bindOwner runs,
  // null when nobody is signed in (then nothing is persisted).
  let owner = stored.owner;
  // buffers is a reactive map: sessionId | "draft" → { threadId, messages }
  const buffers = reactive(stored.buffers);

  // The workspace-file session id bound to the live buffer. Null for an unsaved new chat.
  const activeSessionId = ref(null);

  // Fires the quota toast at most once per store lifetime (~ once per session).
  let quotaToastShown = false;

  function persist() {
    const prefs = usePrefsStore();
    if (prefs.chatPersistEnabled === false) {
      removeStored();
      return;
    }
    if (owner === null) return;
    try {
      localStorage.setItem(
        CHAT_STORAGE_KEY,
        JSON.stringify({ owner, buffers: serializeForStorage(buffers, prefs.chatPersistTurns) })
      );
    } catch {
      // quota / availability — in-memory buffers are unaffected; durable copy is the
      // session file (F33), so this is degraded, not lost.
      if (!quotaToastShown) {
        quotaToastShown = true;
        useToast().push(
          "error",
          "chat buffer too large for local storage — history preserved in session file"
        );
      }
    }
  }

  // Remove all entries except the active session and "draft".
  function prune() {
    const keep = new Set();
    if (activeSessionId.value !== null) keep.add(activeSessionId.value);
    keep.add("draft");
    for (const key of Object.keys(buffers)) {
      if (!keep.has(key)) {
        delete buffers[key];
      }
    }
  }

  // Returns the live buffer for the current conversation, creating it lazily.
  // Key is activeSessionId when a session file is bound, otherwise "draft".
  function currentBuffer() {
    const key = activeSessionId.value ?? "draft";
    if (!buffers[key]) {
      buffers[key] = { threadId: uuid(), messages: [] };
      persist();
    }
    return buffers[key];
  }

  // Reset the draft buffer with a fresh threadId and empty messages.
  // Prunes all non-active, non-draft entries.
  function reset() {
    buffers["draft"] = { threadId: uuid(), messages: [] };
    prune();
    persist();
  }

  // Load a saved session's content into the live buffer.
  // Sets activeSessionId = sessionId and prunes stale entries.
  function hydrateBuffer(sessionId, { threadId, messages } = {}) {
    buffers[sessionId] = {
      threadId: threadId || uuid(),
      messages: Array.isArray(messages) ? messages : [],
    };
    activeSessionId.value = sessionId;
    prune();
    persist();
    return buffers[sessionId];
  }

  // Move the "draft" buffer object to buffers[sessionId], preserving threadId + messages.
  // Sets activeSessionId = sessionId and prunes stale entries.
  function promoteDraft(sessionId) {
    const draft = buffers["draft"] ?? { threadId: uuid(), messages: [] };
    buffers[sessionId] = draft;
    delete buffers["draft"];
    activeSessionId.value = sessionId;
    prune();
    persist();
  }

  function setActiveSession(id) {
    activeSessionId.value = id;
  }

  function clearActiveSession() {
    activeSessionId.value = null;
  }

  // Wipe all buffers on principal change (logout). Mutates in place so reactivity
  // holds. Removes the stored copy so a subsequent user cannot read a prior
  // user's draft on a shared browser, but not another user's that a second tab
  // stored since.
  function clearAll() {
    for (const key of Object.keys(buffers)) {
      delete buffers[key];
    }
    activeSessionId.value = null;
    const storedOwner = readStored()?.owner;
    if (owner === null || storedOwner === undefined || storedOwner === owner) removeStored();
    owner = null;
  }

  // Binds the buffers to the signed-in subject (user store, on sign-in and on
  // a reload). Runs before any view shows them.
  function bindOwner(sub) {
    if (owner === sub) return;
    if (owner !== null || Object.keys(buffers).length) clearAll();
    owner = sub || null;
  }

  return {
    currentBuffer,
    persist,
    reset,
    activeSessionId,
    setActiveSession,
    clearActiveSession,
    hydrateBuffer,
    promoteDraft,
    clearAll,
    bindOwner,
  };
});
