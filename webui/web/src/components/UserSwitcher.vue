<script setup>
// Sidebar footer's user block: a compact row (display name + gear button)
// that opens the settings modal. Theme toggle + sign out both moved into
// UserSettingsModal.
import { ref } from "vue";
import Icon from "./Icon.vue";
import UserSettingsModal from "./UserSettingsModal.vue";
import { useUserStore } from "../store/user.js";

defineProps({
  collapsed: { type: Boolean, default: false },
});

const store = useUserStore();
const showSettings = ref(false);
</script>

<template>
  <div class="user-identity" :class="{ collapsed }">
    <span v-if="!collapsed" class="user-name" :title="store.user">{{ store.displayName || store.user }}</span>
    <button
      class="settings-btn"
      aria-label="User settings"
      title="User settings"
      @click="showSettings = true"
    >
      <Icon name="settings" :size="16" />
    </button>
    <UserSettingsModal :open="showSettings" @close="showSettings = false" />
  </div>
</template>

<style scoped>
.user-identity {
  padding: 12px 16px;
  display: flex;
  align-items: center;
  gap: 6px;
  width: 100%;
  min-width: 0;
}

.user-identity.collapsed {
  justify-content: center;
  padding: 12px 0;
}

.user-name {
  font-size: 12px;
  color: var(--text-muted);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  flex: 1;
  min-width: 0;
}

.settings-btn {
  background: none;
  border: none;
  color: var(--text-muted);
  cursor: pointer;
  padding: 4px;
  display: flex;
  align-items: center;
  border-radius: var(--radius-sm);
  flex-shrink: 0;
  transition: background 0.12s, color 0.12s;
}

.settings-btn:hover {
  background: var(--bg-hover);
  color: var(--text);
}
</style>
