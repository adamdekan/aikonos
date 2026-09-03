<script setup>
import { Markdown } from "vue-stream-markdown";
import "vue-stream-markdown/index.css";

defineProps({
  text: { type: String, default: "" },
});
</script>

<template>
  <div class="markdown-message">
    <Markdown :content="text" />
  </div>
</template>

<style scoped>
.markdown-message {
  color: var(--text);
  word-break: break-word;
  line-height: 1.6;
}

.markdown-message :deep(p) {
  margin: 0 0 0.5rem;
}

.markdown-message :deep(p:last-child) {
  margin-bottom: 0;
}

.markdown-message :deep(ul),
.markdown-message :deep(ol) {
  margin: 0 0 0.5rem 1.25rem;
  padding: 0;
}

.markdown-message :deep(li) {
  margin-bottom: 0.25rem;
}

.markdown-message :deep(code) {
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  padding: 0.1em 0.35em;
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 0.875em;
}

.markdown-message :deep(pre) {
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  padding: 0.75rem 1rem;
  overflow-x: auto;
  max-width: 100%;
  margin: 0.5rem 0;
}

.markdown-message :deep(pre code) {
  background: none;
  border: none;
  border-radius: 0;
  padding: 0;
  font-size: 0.875em;
}

.markdown-message :deep(table) {
  border-collapse: collapse;
  width: 100%;
  margin: 0.5rem 0;
}

.markdown-message :deep(th),
.markdown-message :deep(td) {
  border: 1px solid var(--border);
  padding: 0.375rem 0.625rem;
  text-align: left;
}

.markdown-message :deep(a) {
  color: var(--accent);
  text-decoration: none;
}

.markdown-message :deep(a:hover) {
  color: var(--accent-hover);
}
</style>
