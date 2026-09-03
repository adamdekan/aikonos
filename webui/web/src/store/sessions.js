import { defineStore } from "pinia";
import { ref, computed } from "vue";
import {
  listSessionFiles,
  readSession,
  writeSession,
  deleteSession,
  readManifest,
  writeManifest,
  migrateLegacySessions,
} from "../api/sessions.js";

function sortSessions(list) {
  const pinned = list
    .filter((s) => s.pinned)
    .sort((a, b) => (b.pinned_at ?? "") > (a.pinned_at ?? "") ? 1 : -1);
  const unpinned = list
    .filter((s) => !s.pinned)
    .sort((a, b) => (b.updated_at ?? "") > (a.updated_at ?? "") ? 1 : -1);
  return [...pinned, ...unpinned];
}

function manifestEntry(record) {
  return {
    id: record.id,
    title: record.title,
    agent_id: record.agent_id ?? null,
    agent_name: record.agent_name ?? null,
    pinned: record.pinned ?? false,
    pinned_at: record.pinned_at ?? null,
    created_at: record.created_at,
    updated_at: record.updated_at,
    source: record.source ?? null,
    schedule_id: record.schedule_id ?? null,
  };
}

export const useSessionsStore = defineStore("sessions", () => {
  const sessions = ref([]);
  const cursor = ref(0);
  const pageSize = 10;
  const loading = ref(false);
  const loaded = ref(false);

  const hasMore = computed(() => {
    const pinnedCount = sessions.value.filter((s) => s.pinned).length;
    const visible = Math.max(cursor.value, pinnedCount);
    return visible < sessions.value.length;
  });

  // Visible slice: pinned always present + unpinned up to cursor.
  const visible = computed(() => {
    const pinnedCount = sessions.value.filter((s) => s.pinned).length;
    const end = Math.max(cursor.value, pinnedCount);
    return sessions.value.slice(0, end);
  });

  async function persistManifest(list) {
    try {
      await writeManifest(list.map(manifestEntry));
    } catch {
      // mirror chat.js swallow pattern
    }
  }

  async function load() {
    if (loaded.value) return;
    loading.value = true;
    try {
      await migrateLegacySessions();
      const [manifest, fileList] = await Promise.all([
        readManifest(),
        listSessionFiles(),
      ]);

      const fileIds = new Set(
        fileList.map((f) => {
          const name = f.path.split("/").pop();
          return name.replace(/\.json$/, "");
        })
      );

      const pruned = manifest.filter((e) => fileIds.has(e.id));
      const manifestIds = new Set(manifest.map((e) => e.id));

      // Additive: read files present on disk but absent from the manifest, concurrently.
      const missingIds = [...fileIds].filter((id) => !manifestIds.has(id));
      const results = await Promise.allSettled(missingIds.map((id) => readSession(id)));
      const discovered = results
        .filter((r) => r.status === "fulfilled" && r.value && r.value.id)
        .map((r) => manifestEntry(r.value));

      const merged = [...pruned, ...discovered];
      const changed = pruned.length !== manifest.length || discovered.length > 0;
      if (changed) {
        await persistManifest(merged);
      }

      sessions.value = sortSessions(merged);
      cursor.value = pageSize;
      loaded.value = true;
    } catch {
      sessions.value = [];
      loaded.value = true;
    } finally {
      loading.value = false;
    }
  }

  function fetchMore() {
    if (!hasMore.value || loading.value) return;
    cursor.value += pageSize;
  }

  async function prependNew(entry) {
    if (entry.pinned) {
      // Pinned entry: let sortSessions place it correctly among pinned peers.
      sessions.value = sortSessions([...sessions.value, entry]);
    } else {
      // Common case: insert at top of unpinned region without re-sorting the rest.
      const pinnedCount = sessions.value.filter((s) => s.pinned).length;
      sessions.value.splice(pinnedCount, 0, entry);
    }
    await persistManifest(sessions.value);
  }

  async function rename(id, title) {
    const idx = sessions.value.findIndex((s) => s.id === id);
    if (idx === -1) return;
    // Optimistic in-memory update.
    sessions.value[idx] = { ...sessions.value[idx], title };
    await persistManifest(sessions.value);
    // Patch per-session file (source of truth).
    try {
      const record = await readSession(id);
      if (record) {
        record.title = title;
        record.updated_at = new Date().toISOString();
        await writeSession(record);
        // Sync updated_at back to manifest entry.
        sessions.value[idx] = { ...sessions.value[idx], updated_at: record.updated_at };
        await persistManifest(sessions.value);
      }
    } catch {
      // swallow; in-memory already updated
    }
  }

  async function togglePin(id) {
    const idx = sessions.value.findIndex((s) => s.id === id);
    if (idx === -1) return;
    const entry = sessions.value[idx];
    const nowPinned = !entry.pinned;
    const pinned_at = nowPinned ? new Date().toISOString() : null;
    // Optimistic update + re-sort.
    const updated = { ...entry, pinned: nowPinned, pinned_at };
    const next = [...sessions.value];
    next.splice(idx, 1, updated);
    sessions.value = sortSessions(next);
    await persistManifest(sessions.value);
    // Patch session file.
    try {
      const record = await readSession(id);
      if (record) {
        record.pinned = nowPinned;
        record.pinned_at = pinned_at;
        record.updated_at = new Date().toISOString();
        await writeSession(record);
      }
    } catch {
      // swallow
    }
  }

  async function remove(id) {
    // Optimistic removal.
    sessions.value = sessions.value.filter((s) => s.id !== id);
    await persistManifest(sessions.value);
    try {
      await deleteSession(id);
    } catch {
      // swallow; manifest already pruned
    }
  }

  async function loadMessages(id) {
    return readSession(id);
  }

  // Returns whether the write succeeded — the caller (useSessionLifecycle) must
  // not bind activeSessionId to a session record that was never persisted.
  async function createSession(record) {
    try {
      await writeSession(record);
      await prependNew(manifestEntry(record));
      return true;
    } catch {
      return false;
    }
  }

  async function upsertFromRecord(record) {
    const entry = manifestEntry(record);
    const idx = sessions.value.findIndex((s) => s.id === record.id);
    if (idx === -1) {
      sessions.value.push(entry);
    } else {
      sessions.value[idx] = { ...sessions.value[idx], ...entry };
    }
    sessions.value = sortSessions(sessions.value);
    await persistManifest(sessions.value);
  }

  return {
    sessions,
    cursor,
    pageSize,
    hasMore,
    loading,
    loaded,
    visible,
    load,
    fetchMore,
    prependNew,
    rename,
    togglePin,
    remove,
    loadMessages,
    createSession,
    upsertFromRecord,
  };
});
