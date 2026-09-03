<script setup>
const props = defineProps({
  args: { type: Object, default: null },
  depth: { type: Number, default: 0 },
});

const MAX_DEPTH = 4;

function isScalar(val) {
  return val === null || typeof val !== "object";
}

function countLabel(val) {
  if (Array.isArray(val)) return `[ ${val.length} item${val.length !== 1 ? "s" : ""} ]`;
  return `{ ${Object.keys(val).length} key${Object.keys(val).length !== 1 ? "s" : ""} }`;
}

function stringify(val) {
  if (val === null) return "null";
  if (typeof val === "boolean") return val ? "true" : "false";
  return String(val);
}
</script>

<template>
  <!-- Empty / absent state -->
  <p v-if="!args || Object.keys(args).length === 0" class="args-empty">
    No arguments
  </p>

  <dl v-else class="args-list">
    <template v-for="(val, key) in args" :key="key">
      <!-- Scalar row -->
      <div v-if="isScalar(val)" class="arg-row">
        <dt class="arg-key">{{ key }}</dt>
        <dd class="arg-val">{{ stringify(val) }}</dd>
      </div>

      <!-- Depth cap: render as compact pre rather than recurse endlessly -->
      <div v-else-if="depth >= MAX_DEPTH" class="arg-row arg-row--deep">
        <dt class="arg-key">{{ key }}</dt>
        <dd class="arg-val arg-val--pre">{{ JSON.stringify(val) }}</dd>
      </div>

      <!-- Nested object/array — collapsible via native <details> -->
      <div v-else class="arg-row arg-row--nested">
        <details class="arg-details">
          <summary class="arg-summary">
            <span class="arg-key">{{ key }}</span>
            <span class="arg-count">{{ countLabel(val) }}</span>
          </summary>
          <div class="arg-nested">
            <ArgsView :args="val" :depth="depth + 1" />
          </div>
        </details>
      </div>
    </template>
  </dl>
</template>

<style scoped>
.args-empty {
  margin: 0;
  font-size: 0.8125rem;
  color: var(--text-faint);
  font-style: italic;
}

.args-list {
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}

.arg-row {
  display: grid;
  grid-template-columns: minmax(80px, 30%) 1fr;
  column-gap: var(--space-3);
  align-items: baseline;
}

.arg-key {
  font-family: var(--font-sans);
  font-size: 0.8125rem;
  color: var(--text-muted);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  /* dt reset */
  margin: 0;
  padding: 0;
}

.arg-val {
  font-family: var(--font-mono);
  font-size: 0.8125rem;
  color: var(--text);
  overflow-wrap: anywhere;
  word-break: break-word;
  /* dd reset */
  margin: 0;
  padding: 0;
}

.arg-val--pre {
  white-space: pre-wrap;
  background: var(--bg);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  padding: var(--space-2);
}

/* Nested row spans full width; no grid columns */
.arg-row--nested {
  display: block;
}

.arg-details {
  width: 100%;
}

.arg-summary {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  cursor: pointer;
  list-style: none; /* hide default triangle in some browsers */
  font-size: 0.8125rem;
  padding: var(--space-1) 0;
  user-select: none;
}

/* Custom expand marker */
.arg-summary::before {
  content: "▸";
  display: inline-block;
  font-size: 0.625rem;
  color: var(--text-faint);
  transition: transform 0.15s;
  flex-shrink: 0;
}

.arg-details[open] > .arg-summary::before {
  transform: rotate(90deg);
}

/* Remove default <details> marker in WebKit */
.arg-summary::-webkit-details-marker {
  display: none;
}

.arg-count {
  color: var(--text-faint);
  font-family: var(--font-mono);
  font-size: 0.75rem;
}

.arg-nested {
  padding-left: var(--space-4);
  padding-top: var(--space-1);
}
</style>
