<script setup>
import Icon from "../Icon.vue";
import { DECISION } from "./audit-format.js";

const props = defineProps({
  // numeric decision value (1-4) or string
  decision: { type: [Number, String], default: null },
});

const info = props.decision != null ? DECISION[props.decision] ?? { label: String(props.decision), cls: "", glyph: null } : null;
</script>

<template>
  <span v-if="info" class="decision-badge" :class="info.cls">
    <Icon v-if="info.glyph" :name="info.glyph" :size="12" class="badge-glyph" />
    {{ info.label }}
  </span>
</template>

<style scoped>
.decision-badge {
  display: inline-flex;
  align-items: center;
  gap: var(--space-1);
  font-size: 11px;
  padding: 2px var(--space-2);
  border-radius: 999px;
  font-weight: 500;
  white-space: nowrap;
}

.decision-badge.ok {
  color: var(--ok);
  background: var(--fill-ok);
}
.decision-badge.warn {
  color: var(--accent);
  background: var(--fill-accent);
}
.decision-badge.deny {
  color: var(--danger);
  background: var(--fill-danger);
}
/* unknown decision — faint */
.decision-badge:not(.ok):not(.warn):not(.deny) {
  color: var(--text-faint);
  background: var(--fill-muted);
}

.badge-glyph {
  flex-shrink: 0;
}
</style>
