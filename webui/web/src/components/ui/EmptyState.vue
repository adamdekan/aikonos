<script setup>
import Icon from "../Icon.vue";

const props = defineProps({
  icon: { type: String, default: null },
  message: { type: String, default: null },
});
</script>

<template>
  <div class="empty-state">
    <Icon v-if="icon" :name="icon" :size="32" />
    <span class="empty-msg">
      <slot>{{ message }}</slot>
    </span>
  </div>
</template>

<style scoped>
.empty-state {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 8px;
  padding: 32px 16px;
  color: var(--text-faint);
}

.empty-msg {
  font-size: 0.9rem;
}
</style>
