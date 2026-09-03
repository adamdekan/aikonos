<script setup>
import { ref, nextTick, onMounted, onUnmounted } from "vue";
import { useRouter } from "vue-router";
import Icon from "./Icon.vue";
import SessionMenu from "./SessionMenu.vue";
import { useSessionsStore } from "../store/sessions.js";

const props = defineProps({
  session: { type: Object, required: true },
  active: { type: Boolean, default: false },
});

const router = useRouter();
const sessionsStore = useSessionsStore();

const menuOpen = ref(false);
const renaming = ref(false);
const renameValue = ref("");
const renameInput = ref(null);
const itemEl = ref(null);

function resume() {
  if (renaming.value) return;
  const q = { session: props.session.id };
  if (props.session.agent_id) q.agent = props.session.agent_id;
  router.push({ path: "/chat", query: q });
}

function openMenu(e) {
  e.stopPropagation();
  menuOpen.value = !menuOpen.value;
}

function closeMenu() {
  menuOpen.value = false;
}

function startRename() {
  renameValue.value = props.session.title || "";
  renaming.value = true;
  nextTick(() => {
    if (renameInput.value) {
      renameInput.value.focus();
      renameInput.value.select();
    }
  });
}

function commitRename() {
  if (!renaming.value) return;
  renaming.value = false;
  const val = renameValue.value.trim();
  if (val && val !== props.session.title) {
    sessionsStore.rename(props.session.id, val);
  }
}

function cancelRename() {
  renaming.value = false;
}

function onRenameKeydown(e) {
  if (e.key === "Enter") commitRename();
  else if (e.key === "Escape") cancelRename();
}

// Outside-click for the menu: close when click lands outside the item element.
function onDocClick(e) {
  if (menuOpen.value && itemEl.value && !itemEl.value.contains(e.target)) {
    closeMenu();
  }
}

onMounted(() => document.addEventListener("click", onDocClick, true));
onUnmounted(() => document.removeEventListener("click", onDocClick, true));
</script>

<template>
  <div
    ref="itemEl"
    :class="['session-item', { active, 'has-agent': !!session.agent_id }]"
    :title="session.title"
    @click="resume"
  >
    <!-- Scheduled badge: always visible for broker-written scheduled sessions -->
    <span v-if="session.source === 'schedule'" class="sched-badge" title="Scheduled run">
      <svg width="10" height="10" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
        <path d="M12 2a10 10 0 1 0 0 20A10 10 0 0 0 12 2zm0 18a8 8 0 1 1 0-16 8 8 0 0 1 0 16zm.5-13H11v6l5.25 3.15.75-1.23-4.5-2.67V7z"/>
      </svg>
    </span>

    <!-- Agent badge: always visible for agent-bound rows -->
    <span v-if="session.agent_id" class="agent-badge" :title="session.agent_name || session.agent_id">
      <Icon name="chat" :size="10" />
    </span>

    <!-- Title / hover-swap area -->
    <span v-if="!renaming" class="item-title">
      <span class="title-text">{{ session.title || "Untitled" }}</span>
      <span v-if="session.agent_id" class="agent-hover-name">{{ session.agent_name || session.agent_id }}</span>
    </span>
    <input
      v-else
      ref="renameInput"
      class="rename-input"
      v-model="renameValue"
      @keydown="onRenameKeydown"
      @blur="commitRename"
      @click.stop
    />

    <!-- Kebab trigger -->
    <button
      class="kebab-btn"
      :class="{ 'kebab-always': !!session.agent_id }"
      @click.stop="openMenu"
      title="Options"
      aria-label="Session options"
    >
      <svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
        <circle cx="12" cy="5" r="2"/><circle cx="12" cy="12" r="2"/><circle cx="12" cy="19" r="2"/>
      </svg>
    </button>

    <SessionMenu
      v-if="menuOpen"
      :id="session.id"
      :pinned="session.pinned"
      @rename="startRename"
      @close="closeMenu"
    />
  </div>
</template>

<style scoped>
.session-item {
  position: relative;
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 6px 10px 6px 12px;
  margin: 1px 6px;
  border-radius: var(--radius-sm);
  color: var(--text-muted);
  font-size: 13px;
  cursor: pointer;
  transition: background 0.12s, color 0.12s;
  min-width: 0;
}

.session-item:hover {
  background: var(--bg-hover);
  color: var(--text);
}

.session-item.active {
  background: var(--bg-active);
  color: var(--text);
  font-weight: 500;
}

.sched-badge {
  flex-shrink: 0;
  display: flex;
  align-items: center;
  color: var(--text-muted);
  opacity: 0.7;
}

.agent-badge {
  flex-shrink: 0;
  display: flex;
  align-items: center;
  color: var(--accent);
  opacity: 0.75;
}

.item-title {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  position: relative;
}

.title-text {
  display: block;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

/* On pointer devices, hover swaps the visible text to the agent name */
.has-agent .agent-hover-name {
  display: none;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  color: var(--accent);
  font-size: 12px;
}

@media (hover: hover) {
  .has-agent:hover .title-text {
    display: none;
  }
  .has-agent:hover .agent-hover-name {
    display: block;
  }
}

/* Kebab: hidden by default; shown on hover for pointer devices */
.kebab-btn {
  flex-shrink: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  width: 22px;
  height: 22px;
  background: none;
  border: none;
  border-radius: var(--radius-sm);
  color: var(--text-muted);
  cursor: pointer;
  opacity: 0;
  transition: opacity 0.12s, background 0.12s;
  /* On touch devices: always-dimmed so it's reachable */
}

@media (hover: hover) {
  .session-item:hover .kebab-btn {
    opacity: 1;
  }
}

/* Touch devices: always visible at reduced opacity */
@media (hover: none) {
  .kebab-btn {
    opacity: 0.4;
  }
}

/* Agent rows: kebab always rendered at low opacity (badge already signals agent) */
.kebab-always {
  opacity: 0.3;
}

.session-item:hover .kebab-btn {
  opacity: 1;
}

.kebab-btn:hover {
  background: var(--bg-hover);
  color: var(--text);
}

.rename-input {
  flex: 1;
  min-width: 0;
  background: var(--bg-hover);
  border: 1px solid var(--accent);
  border-radius: var(--radius-sm);
  color: var(--text);
  font-size: 13px;
  padding: 2px 6px;
  outline: none;
}
</style>
