<script setup>
// Convention: persistent load/view failures (a dataset failed to fetch, a
// directory failed to list) use this banner — visible until the underlying
// state clears. Transient action/mutation feedback (a save, delete, or rename
// failed) uses the toast queue (`useToast`) instead — it should not linger.
//
// The `action` slot is optional and lets callers attach a retry control
// without the component knowing about any particular retry mechanism.
import Icon from "../Icon.vue";

defineProps({
  message: { type: String, default: "" },
});
</script>

<template>
  <div class="error-banner" data-testid="error-banner">
    <Icon name="close" :size="14" />
    <span class="error-banner-msg"><slot>{{ message }}</slot></span>
    <slot name="action" />
  </div>
</template>

<style scoped>
.error-banner {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  padding: 0.75rem 1rem;
  background: var(--fill-danger);
  border: 1px solid var(--danger);
  border-radius: var(--radius-sm);
  color: var(--danger);
  font-size: 0.875rem;
}

.error-banner-msg {
  flex: 1;
}
</style>
