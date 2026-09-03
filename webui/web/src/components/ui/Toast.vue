<script setup>
import { useToast } from "./useToast.js";

const { toasts, dismiss } = useToast();
</script>

<template>
  <Teleport to="body">
    <div class="toast-stack" aria-live="polite">
      <div
        v-for="t in toasts"
        :key="t.id"
        class="toast-item"
        :class="t.type === 'error' ? 'toast-error' : 'toast-ok'"
      >
        <span class="toast-msg">{{ t.msg }}</span>
        <button class="toast-close" @click="dismiss(t.id)" aria-label="Dismiss">×</button>
      </div>
    </div>
  </Teleport>
</template>

<style scoped>
.toast-stack {
  position: fixed;
  bottom: 24px;
  right: 24px;
  display: flex;
  flex-direction: column;
  gap: 8px;
  z-index: 2000;
}

.toast-item {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px 14px;
  border-radius: var(--radius-sm);
  font-size: 0.875rem;
  min-width: 240px;
  max-width: 380px;
  border: 1px solid var(--border);
  background: var(--bg-elevated);
  color: var(--text);
}

.toast-ok {
  border-left: 3px solid var(--ok);
}

.toast-error {
  border-left: 3px solid var(--danger);
}

.toast-msg {
  flex: 1;
}

.toast-close {
  background: none;
  border: none;
  color: var(--text-muted);
  cursor: pointer;
  font-size: 1rem;
  line-height: 1;
  padding: 0;
}

.toast-close:hover {
  color: var(--text);
}
</style>
