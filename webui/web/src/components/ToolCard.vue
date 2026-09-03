<script setup>
import Icon from "./Icon.vue";

const props = defineProps({
  id:      { type: String, required: true },
  name:    { type: String, required: true },
  // argsJson is the raw JSON string received from TOOL_CALL_ARGS delta.
  argsJson: { type: String, default: "" },
  result:  { type: String, default: null },
  isError: { type: Boolean, default: false },
  // null = in progress, false = done (ok), true = error
  done:    { type: Boolean, default: false },
});

function prettyArgs(raw) {
  if (!raw) return "";
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch {
    return raw;
  }
}

function prettyResult(raw) {
  if (raw === null || raw === undefined) return "";
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch {
    return raw;
  }
}
</script>

<template>
  <div class="tool-chip-wrapper">
    <div
      class="tool-chip"
      :class="{ 'tool-chip--error': isError, 'tool-chip--ok': done && !isError }"
      data-testid="tool-chip"
      tabindex="0"
    >
      <Icon :name="isError ? 'close' : done ? 'check' : 'code'" :size="12" />
      <span class="tool-chip-name">{{ name }}</span>
      <span class="tool-pill">governed</span>
    </div>

    <div class="tool-popover" data-testid="tool-popover">
      <div class="tool-popover-header">{{ name }}</div>

      <template v-if="argsJson">
        <div class="tool-popover-section-label">args</div>
        <pre class="tool-popover-pre">{{ prettyArgs(argsJson) }}</pre>
      </template>

      <template v-if="result !== null">
        <div class="tool-popover-section-label">result</div>
        <pre class="tool-popover-pre" :class="{ 'tool-popover-pre--error': isError }">{{ prettyResult(result) }}</pre>
      </template>
    </div>
  </div>
</template>

<style scoped>
.tool-chip-wrapper {
  position: relative;
  display: inline-flex;
}

.tool-chip {
  display: inline-flex;
  align-items: center;
  gap: 0.25rem;
  padding: 0.125rem 0.5rem;
  border-radius: 999px;
  border: 1px solid var(--border);
  font-size: 0.75rem;
  background: var(--bg-elevated);
  color: var(--text-muted);
  cursor: default;
  outline: none;
}

.tool-chip--ok {
  border-color: var(--ok);
  color: var(--ok);
}

.tool-chip--error {
  border-color: var(--danger);
  color: var(--danger);
}

.tool-chip-name {
  font-family: var(--font-mono);
  font-size: 0.75rem;
}

.tool-pill {
  font-size: 0.625rem;
  font-weight: 600;
  letter-spacing: 0.05em;
  padding: 0.0625rem 0.375rem;
  border-radius: 999px;
  background: var(--fill-muted);
  color: var(--text-muted);
  border: 1px solid var(--border);
  text-transform: uppercase;
}

/* Popover: hidden by default, shown on hover/focus */
.tool-popover {
  display: none;
  position: absolute;
  top: calc(100% + 4px);
  left: 0;
  z-index: 100;
  max-width: 560px;
  max-height: 320px;
  overflow-y: auto;
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  box-shadow: 0 4px 16px rgba(0, 0, 0, 0.3);
  padding: 0.75rem;
  flex-direction: column;
  gap: 0.375rem;
}

.tool-chip-wrapper:hover .tool-popover,
.tool-chip-wrapper:focus-within .tool-popover {
  display: flex;
}

.tool-popover-header {
  font-family: var(--font-mono);
  font-size: 0.8125rem;
  font-weight: 600;
  color: var(--text);
  margin-bottom: 0.25rem;
}

.tool-popover-section-label {
  font-size: 0.6875rem;
  font-weight: 600;
  letter-spacing: 0.05em;
  text-transform: uppercase;
  color: var(--text-muted);
}

.tool-popover-pre {
  margin: 0;
  font-family: var(--font-mono);
  font-size: 0.8125rem;
  color: var(--text-muted);
  background: var(--bg);
  border: 1px solid var(--border);
  border-radius: 6px;
  padding: 0.5rem;
  white-space: pre-wrap;
  overflow-x: auto;
}

.tool-popover-pre--error {
  color: var(--danger);
}
</style>
