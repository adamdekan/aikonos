<script setup>
import { ref, onMounted, onUnmounted } from "vue";
import { useSessionsStore } from "../store/sessions.js";

const props = defineProps({
  id: { type: String, required: true },
  pinned: { type: Boolean, default: false },
});

const emit = defineEmits(["rename", "close"]);

const sessionsStore = useSessionsStore();
const confirmDelete = ref(false);

function handleRename() {
  emit("rename");
  emit("close");
}

function handlePin() {
  sessionsStore.togglePin(props.id);
  emit("close");
}

function handleDelete() {
  if (!confirmDelete.value) {
    confirmDelete.value = true;
    return;
  }
  sessionsStore.remove(props.id);
  emit("close");
}

function onKeydown(e) {
  if (e.key === "Escape") emit("close");
}

onMounted(() => {
  document.addEventListener("keydown", onKeydown);
});

onUnmounted(() => {
  document.removeEventListener("keydown", onKeydown);
});
</script>

<template>
  <div class="session-menu" role="menu">
    <button class="menu-item" @click="handleRename">Rename</button>
    <button class="menu-item" @click="handlePin">{{ pinned ? "Unpin" : "Pin" }}</button>
    <button
      class="menu-item menu-item--danger"
      @click="handleDelete"
    >{{ confirmDelete ? "Confirm?" : "Delete" }}</button>
  </div>
</template>

<style scoped>
.session-menu {
  position: absolute;
  right: 4px;
  top: 100%;
  z-index: 200;
  background: var(--bg-sidebar);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  box-shadow: 0 4px 12px rgba(0, 0, 0, 0.25);
  min-width: 120px;
  padding: 4px 0;
}

.menu-item {
  display: block;
  width: 100%;
  padding: 7px 14px;
  text-align: left;
  background: none;
  border: none;
  color: var(--text);
  font-size: 13px;
  cursor: pointer;
  white-space: nowrap;
}

.menu-item:hover {
  background: var(--bg-hover);
}

.menu-item--danger {
  color: var(--color-danger, #e05252);
}
</style>
