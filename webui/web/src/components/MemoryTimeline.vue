<script setup>
// Per-turn auto-recall chip: which memory concepts' frontmatter was injected as
// the prompt preamble. Display only —
// the injection already happened server-side.
import Icon from "./Icon.vue";

defineProps({
  // [{ id, scope, groupId?, title, status, trustTier, stale }]
  concepts: { type: Array, default: () => [] },
});

// scope="group" carries the group slug; every other scope is its bare name.
function scopeLabel(c) {
  return c.scope === "group" && c.groupId ? `${c.scope}:${c.groupId}` : c.scope;
}
</script>

<template>
  <div class="memory-timeline" data-testid="memory-timeline">
    <div
      v-for="c in concepts"
      :key="`${c.id}:${c.scope}`"
      class="memory-timeline-item"
      :class="{ 'memory-timeline-item--stale': c.stale }"
    >
      <Icon name="book" :size="14" class="memory-timeline-icon" />
      <div class="memory-timeline-body">
        <span class="memory-timeline-title">
          <span class="memory-timeline-scope">({{ scopeLabel(c) }})</span>
          {{ c.title || c.id }}
        </span>
        <span class="memory-timeline-detail">
          {{ c.id }} · {{ c.status }} · {{ c.trustTier }}<template v-if="c.stale">, stale</template>
        </span>
      </div>
    </div>
  </div>
</template>

<style scoped>
.memory-timeline {
  display: flex;
  flex-direction: column;
  gap: 0.375rem;
  padding: 0.5rem 0.75rem;
  border-left: 2px solid var(--border);
}

.memory-timeline-item {
  display: flex;
  align-items: flex-start;
  gap: 0.5rem;
  color: var(--text-muted);
}

.memory-timeline-icon {
  flex-shrink: 0;
  margin-top: 1px;
  color: var(--accent);
}

.memory-timeline-item--stale .memory-timeline-icon {
  color: var(--text-faint);
}

.memory-timeline-body {
  display: flex;
  flex-direction: column;
}

.memory-timeline-title {
  font-size: 0.8125rem;
  color: var(--text);
}

.memory-timeline-scope {
  font-family: var(--font-mono);
  color: var(--text-muted);
}

.memory-timeline-detail {
  font-family: var(--font-mono);
  font-size: 0.75rem;
  color: var(--text-muted);
}
</style>
