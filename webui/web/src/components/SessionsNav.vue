<script setup>
import { ref, computed, onMounted, onUnmounted } from "vue";
import { useRoute } from "vue-router";
import { useSessionsStore } from "../store/sessions.js";
import SessionItem from "./SessionItem.vue";
import Icon from "./Icon.vue";
import { readCollapsed, writeCollapsed } from "../lib/collapsePref.js";

const SESSIONS_KEY = "aikonos.sidebar.sessionsCollapsed";

const sessionsCollapsed = ref(readCollapsed(SESSIONS_KEY));

function toggleSessions() {
  sessionsCollapsed.value = !sessionsCollapsed.value;
  writeCollapsed(SESSIONS_KEY, sessionsCollapsed.value);
}

const sessionsStore = useSessionsStore();
const route = useRoute();

const scheduledOnly = ref(false);
const titleFilter = ref("");

function matchesTitleFilter(s) {
  const q = titleFilter.value.trim().toLowerCase();
  if (!q) return true;
  return (s.title ?? "").toLowerCase().includes(q);
}

const hasTitleFilter = computed(() => titleFilter.value.trim().length > 0);

// Interplay decision (F63): scheduledOnly and the title filter both scan the
// full in-memory list (sessionsStore.sessions), not the cursor-paged slice —
// a cursor-limited page could hide a match that hasn't been paged into view
// yet. They compose (AND). Empty filter + scheduledOnly off falls back to
// sessionsStore.visible, reproducing today's exact paged/pinned rendering.
const visibleSessions = computed(() => {
  if (!scheduledOnly.value && !hasTitleFilter.value) return sessionsStore.visible;
  let base = sessionsStore.sessions;
  if (scheduledOnly.value) base = base.filter((s) => s.source === "schedule");
  if (hasTitleFilter.value) base = base.filter(matchesTitleFilter);
  return base;
});

const sentinel = ref(null);
let observer = null;

onMounted(() => {
  sessionsStore.load().catch((e) => { console.error("sessions load failed", e); });

  if (typeof IntersectionObserver !== "undefined") {
    observer = new IntersectionObserver(
      (entries) => {
        if (entries[0].isIntersecting && sessionsStore.hasMore && !sessionsStore.loading) {
          sessionsStore.fetchMore();
        }
      },
      { threshold: 0 }
    );
    if (sentinel.value) observer.observe(sentinel.value);
  }
});

onUnmounted(() => {
  if (observer) {
    observer.disconnect();
    observer = null;
  }
});

function activeSessionId() {
  return route.query.session || null;
}
</script>

<template>
  <div class="sessions-section">
    <div
      class="section-label section-label--collapsible"
      @click="toggleSessions"
    >
      Sessions
      <Icon
        name="chevron-down"
        :size="12"
        :class="['collapse-chevron', { 'collapse-chevron--closed': sessionsCollapsed }]"
      />
    </div>

    <div v-if="!sessionsCollapsed" class="sessions-list">
      <div class="sched-filter-row">
        <input
          v-model="titleFilter"
          type="text"
          class="title-filter-input"
          data-testid="session-filter-input"
          placeholder="Filter sessions…"
          aria-label="Filter sessions by title"
        />
        <button
          type="button"
          class="sched-filter"
          :class="{ 'sched-filter--active': scheduledOnly }"
          :aria-pressed="scheduledOnly"
          aria-label="Scheduled only"
          title="Scheduled only"
          @click="scheduledOnly = !scheduledOnly"
        >
          <Icon name="schedules" :size="12" />
        </button>
      </div>

      <div v-if="sessionsStore.loaded && visibleSessions.length === 0" class="sessions-empty">
        No conversations yet
      </div>

      <SessionItem
        v-for="s in visibleSessions"
        :key="s.id"
        :session="s"
        :active="s.id === activeSessionId()"
      />

      <!-- IntersectionObserver sentinel for lazy loading — hidden while a filter is active -->
      <div v-if="!scheduledOnly && !hasTitleFilter" ref="sentinel" class="sessions-sentinel" aria-hidden="true" />
    </div>
  </div>
</template>

<style scoped>
.sessions-section {
  padding: 4px 0;
}

/* The list grows to its natural height; the parent .sidebar-scroll is the
   single scroll container, so Sessions and Admin are both fully reachable. */
.sessions-list {
  display: flex;
  flex-direction: column;
}

.section-label {
  padding: 8px 16px 4px;
  font-size: 10px;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: 0.08em;
  color: var(--text-muted);
}

.section-label--collapsible {
  display: flex;
  align-items: center;
  justify-content: space-between;
  cursor: pointer;
  user-select: none;
}

.section-label--collapsible:hover {
  color: var(--text);
}

.collapse-chevron {
  flex-shrink: 0;
  transition: transform 0.2s ease;
}

.collapse-chevron--closed {
  transform: rotate(-90deg);
}

.sessions-empty {
  padding: 6px 16px;
  font-size: 12px;
  color: var(--text-muted);
  font-style: italic;
}

.sessions-sentinel {
  height: 1px;
}

.sched-filter-row {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 4px 16px;
}

.title-filter-input {
  flex: 1;
  min-width: 0;
  font-size: 11px;
  line-height: 1.4;
  padding: 3px 8px;
  color: var(--text);
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
}

.title-filter-input::placeholder {
  color: var(--text-muted);
}

.sched-filter {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
  padding: 3px 7px;
  color: var(--text-muted);
  background: transparent;
  border: 1px solid var(--border);
  border-radius: 999px;
  cursor: pointer;
  user-select: none;
  transition: background 0.12s, color 0.12s, border-color 0.12s;
}

.sched-filter:hover {
  color: var(--text);
  border-color: var(--text-muted);
}

/* pressed-in state = filter active */
.sched-filter--active {
  color: var(--text-on-accent);
  background: var(--accent);
  border-color: var(--accent);
}

.sched-filter--active:hover {
  color: var(--text-on-accent);
  background: var(--accent-hover);
  border-color: var(--accent-hover);
}
</style>
