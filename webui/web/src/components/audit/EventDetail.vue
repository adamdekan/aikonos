<script setup>
import { ref } from "vue";
import Icon from "../Icon.vue";

const props = defineProps({
  ev: { type: Object, required: true },
});

const copied = ref(null); // key that was just copied, for 1s feedback

function copyVal(key, val) {
  const s = typeof val === "object" ? JSON.stringify(val) : String(val);
  navigator.clipboard?.writeText(s).then(() => {
    copied.value = key;
    setTimeout(() => { copied.value = null; }, 1000);
  }).catch(() => {});
}

function copyJson() {
  navigator.clipboard?.writeText(JSON.stringify(props.ev, null, 2)).then(() => {
    copied.value = "__json__";
    setTimeout(() => { copied.value = null; }, 1000);
  }).catch(() => {});
}

function fieldEntries(ev) {
  return Object.entries(ev).filter(([k]) => !k.startsWith("_") && k !== "context");
}
</script>

<template>
  <div class="event-detail">
    <div class="detail-header">
      <span class="detail-title">Event detail</span>
      <button class="copy-btn" @click="copyJson" :title="'Copy event JSON'">
        <Icon name="download" :size="13" />
        <span>{{ copied === '__json__' ? 'copied' : 'Copy JSON' }}</span>
      </button>
    </div>

    <div class="detail-fields">
      <div
        v-for="[key, val] in fieldEntries(ev)"
        :key="key"
        class="detail-field"
        @click="copyVal(key, val)"
        :title="'Click to copy'"
      >
        <span class="detail-key">{{ key }}</span>
        <span class="detail-val" :class="{ copied: copied === key }">
          {{ typeof val === 'object' ? JSON.stringify(val) : val }}
        </span>
      </div>
    </div>

    <div v-if="ev.context !== undefined" class="detail-context">
      <span class="detail-key">context</span>
      <pre class="detail-pre">{{ JSON.stringify(ev.context, null, 2) }}</pre>
    </div>
  </div>
</template>

<style scoped>
.event-detail {
  padding: var(--space-3) var(--space-4);
  font-size: 12px;
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}

.detail-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: var(--space-1);
}

.detail-title {
  font-size: 11px;
  font-weight: 600;
  text-transform: uppercase;
  letter-spacing: .05em;
  color: var(--text-muted);
}

.copy-btn {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: 11px;
  color: var(--text-muted);
  background: none;
  border: none;
  cursor: pointer;
  padding: 2px 6px;
  border-radius: var(--radius-sm);
}
.copy-btn:hover { color: var(--accent); }

.detail-fields {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
}

.detail-field {
  display: flex;
  gap: var(--space-3);
  cursor: pointer;
  border-radius: 3px;
  padding: 1px 3px;
}
.detail-field:hover {
  background: var(--bg-hover);
}

.detail-key {
  color: var(--text-muted);
  min-width: 140px;
  font-family: var(--font-mono);
  flex-shrink: 0;
}

.detail-val {
  color: var(--text);
  font-family: var(--font-mono);
  word-break: break-all;
  transition: color 0.15s;
}
.detail-val.copied {
  color: var(--ok);
}

.detail-context {
  display: flex;
  gap: var(--space-3);
  align-items: flex-start;
}

.detail-pre {
  margin: 0;
  padding: var(--space-2) var(--space-3);
  background: var(--bg);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  font-family: var(--font-mono);
  font-size: 12px;
  color: var(--text);
  white-space: pre;
  overflow-x: auto;
  max-height: 280px;
}
</style>
