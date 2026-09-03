<script setup>
import Icon from "./Icon.vue";

defineProps({
  // [{ name, description, status: "loaded"|"suppressed", reason? }]
  skills: { type: Array, default: () => [] },
});
</script>

<template>
  <div class="skill-timeline" data-testid="skill-timeline">
    <div
      v-for="skill in skills"
      :key="`${skill.name}:${skill.status}`"
      class="skill-timeline-item"
      :class="{ 'skill-timeline-item--suppressed': skill.status === 'suppressed' }"
    >
      <Icon :name="skill.status === 'suppressed' ? 'close' : 'book'" :size="14" class="skill-timeline-icon" />
      <div class="skill-timeline-body">
        <span class="skill-timeline-name">{{ skill.name }}</span>
        <span v-if="skill.status === 'suppressed'" class="skill-timeline-detail">{{ skill.reason }}</span>
        <span v-else class="skill-timeline-detail">{{ skill.description }}</span>
      </div>
    </div>
  </div>
</template>

<style scoped>
.skill-timeline {
  display: flex;
  flex-direction: column;
  gap: 0.375rem;
  padding: 0.5rem 0.75rem;
  border-left: 2px solid var(--border);
}

.skill-timeline-item {
  display: flex;
  align-items: flex-start;
  gap: 0.5rem;
  color: var(--text-muted);
}

.skill-timeline-icon {
  flex-shrink: 0;
  margin-top: 1px;
  color: var(--ok);
}

.skill-timeline-item--suppressed .skill-timeline-icon {
  color: var(--danger);
}

.skill-timeline-body {
  display: flex;
  flex-direction: column;
}

.skill-timeline-name {
  font-family: var(--font-mono);
  font-size: 0.8125rem;
  color: var(--text);
}

.skill-timeline-detail {
  font-size: 0.75rem;
  color: var(--text-muted);
}

.skill-timeline-item--suppressed .skill-timeline-detail {
  color: var(--danger);
}
</style>
