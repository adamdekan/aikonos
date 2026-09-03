<script setup>
import { ref, watch } from "vue";
import Icon from "./Icon.vue";
import { forkWorkflow } from "../api/workflows.js";

const props = defineProps({
  // The whole workflow row; only lineageId/name are read below.
  workflow: { type: Object, default: null },
  visible: { type: Boolean, default: false },
});

const emit = defineEmits(["close", "forked"]);

const newName = ref("");
const pending = ref(false);
const error = ref(null);

// Pre-fill the name with "Fork of <original>" when the modal opens.
watch(
  () => [props.workflow, props.visible],
  () => {
    if (!props.visible || !props.workflow) return;
    newName.value = props.workflow.name ? `Fork of ${props.workflow.name}` : "";
    pending.value = false;
    error.value = null;
  },
  { immediate: true },
);

async function submit() {
  if (pending.value || !props.workflow || !newName.value.trim()) return;
  pending.value = true;
  error.value = null;
  try {
    await forkWorkflow(props.workflow.lineageId, { newName: newName.value.trim() });
    emit("forked");
    emit("close");
  } catch (err) {
    error.value = err?.message ?? "Fork failed";
  } finally {
    pending.value = false;
  }
}

function close() {
  emit("close");
}
</script>

<template>
  <div v-if="visible && workflow" class="fork-backdrop">
    <div class="fork-modal" role="dialog" aria-modal="true" aria-labelledby="fork-modal-title">

      <header class="fork-header">
        <Icon name="connections" :size="16" />
        <span id="fork-modal-title" class="fork-title">Fork Workflow</span>
        <button class="fork-close" aria-label="Close" @click="close">
          <Icon name="close" :size="14" />
        </button>
      </header>

      <hr class="fork-divider" />

      <section class="fork-body">
        <p class="fork-hint">
          Create a private copy of <strong>{{ workflow.name }}</strong> that you own.
        </p>
        <div class="field">
          <label for="fork-name" class="field-label">New name</label>
          <input
            id="fork-name"
            v-model="newName"
            class="field-input"
            type="text"
            placeholder="My fork"
            data-testid="input-fork-name"
            @keydown.enter="submit"
          />
        </div>
      </section>

      <p v-if="error" class="fork-error" role="alert" data-testid="fork-error">{{ error }}</p>

      <footer class="fork-footer">
        <button class="btn-cancel" :disabled="pending" @click="close">Cancel</button>
        <button
          class="btn-fork"
          :disabled="pending || !newName.trim()"
          :aria-busy="pending"
          data-testid="btn-fork"
          @click="submit"
        >
          <Icon v-if="pending" name="spinner" :size="14" class="spin" />
          <Icon v-else name="connections" :size="14" />
          {{ pending ? "Forking…" : "Fork" }}
        </button>
      </footer>

    </div>
  </div>
</template>

<style scoped>
.fork-backdrop {
  position: fixed;
  inset: 0;
  background: var(--overlay);
  display: flex;
  align-items: center;
  justify-content: center;
  z-index: 100;
}

.fork-modal {
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: var(--space-4) var(--space-5);
  min-width: 320px;
  max-width: 440px;
  width: 100%;
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
}

.fork-header {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-weight: 500;
  color: var(--text);
}

.fork-title {
  flex: 1;
  font-size: 0.9375rem;
}

.fork-close {
  background: transparent;
  border: none;
  cursor: pointer;
  color: var(--text-muted);
  padding: var(--space-1);
  display: flex;
  align-items: center;
}

.fork-divider {
  border: none;
  border-top: 1px solid var(--border);
  margin: 0;
}

.fork-body {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}

.fork-hint {
  margin: 0;
  font-size: 0.875rem;
  color: var(--text-muted);
}

.field {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
}

.field-label {
  font-size: 0.8125rem;
  font-weight: 500;
  color: var(--text-muted);
}

.field-input {
  font-size: 0.875rem;
  padding: var(--space-2) var(--space-3);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--bg);
  color: var(--text);
  font-family: var(--font-sans);
}

.field-input:focus {
  outline: none;
  border-color: var(--accent);
}

.fork-error {
  margin: 0;
  font-size: 0.8125rem;
  color: var(--danger);
}

.fork-footer {
  display: flex;
  gap: var(--space-2);
  justify-content: flex-end;
  padding-top: var(--space-4);
  border-top: 1px solid var(--border);
}

.btn-fork {
  display: inline-flex;
  align-items: center;
  gap: var(--space-2);
  padding: var(--space-2) var(--space-4);
  border-radius: var(--radius-sm);
  font-family: var(--font-sans);
  font-size: 0.875rem;
  font-weight: 500;
  cursor: pointer;
  background: var(--accent);
  color: var(--text-on-accent);
  border: 1px solid transparent;
  transition: background 0.15s, opacity 0.15s;
}

.btn-fork:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}

.btn-fork:not(:disabled):hover {
  filter: brightness(1.1);
}

.btn-cancel {
  display: inline-flex;
  align-items: center;
  padding: var(--space-2) var(--space-4);
  border-radius: var(--radius-sm);
  font-family: var(--font-sans);
  font-size: 0.875rem;
  font-weight: 500;
  cursor: pointer;
  background: transparent;
  color: var(--text);
  border: 1px solid var(--border);
  transition: background 0.15s;
}

.btn-cancel:hover {
  background: var(--bg-hover);
}

.spin {
  animation: spin 0.8s linear infinite;
}

@keyframes spin {
  to { transform: rotate(360deg); }
}
</style>
