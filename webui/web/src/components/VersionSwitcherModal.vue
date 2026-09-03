<script setup>
import { ref, watch } from "vue";
import Icon from "./Icon.vue";
import { listVersions, pinVersion, clearPin, decideVersion } from "../api/workflows.js";

const props = defineProps({
  // The whole workflow row; only lineageId/name/version (current/active
  // version)/isOwner are read below.
  workflow: { type: Object, default: null },
  visible: { type: Boolean, default: false },
});

const emit = defineEmits(["close", "changed"]);

const versions = ref([]);
const loading = ref(false);
const loadError = ref(null);

const pinPending = ref(false);
const clearPending = ref(false);
const decidePending = ref(false);
const actionError = ref(null);

// Load versions whenever the modal opens.
watch(
  () => [props.workflow, props.visible],
  async () => {
    if (!props.visible || !props.workflow) return;
    loading.value = true;
    loadError.value = null;
    actionError.value = null;
    try {
      const data = await listVersions(props.workflow.lineageId);
      versions.value = data.versions ?? [];
    } catch (err) {
      loadError.value = err?.message ?? "Failed to load versions";
    } finally {
      loading.value = false;
    }
  },
  { immediate: true },
);

async function pin(version) {
  if (pinPending.value || !props.workflow) return;
  pinPending.value = true;
  actionError.value = null;
  try {
    await pinVersion(props.workflow.lineageId, { version });
    emit("changed");
    emit("close");
  } catch (err) {
    actionError.value = err?.message ?? "Pin failed";
  } finally {
    pinPending.value = false;
  }
}

async function clear() {
  if (clearPending.value || !props.workflow) return;
  clearPending.value = true;
  actionError.value = null;
  try {
    await clearPin(props.workflow.lineageId);
    emit("changed");
    emit("close");
  } catch (err) {
    actionError.value = err?.message ?? "Clear pin failed";
  } finally {
    clearPending.value = false;
  }
}

async function decide(version, approved) {
  if (decidePending.value || !props.workflow) return;
  decidePending.value = true;
  actionError.value = null;
  try {
    await decideVersion(props.workflow.lineageId, { version, approved, reason: "" });
    // Refresh the list in-place so the owner sees the new state immediately.
    const data = await listVersions(props.workflow.lineageId);
    versions.value = data.versions ?? [];
  } catch (err) {
    actionError.value = err?.message ?? "Decision failed";
  } finally {
    decidePending.value = false;
  }
}

function close() {
  emit("close");
}
</script>

<template>
  <div v-if="visible && workflow" class="vsw-backdrop">
    <div class="vsw-modal" role="dialog" aria-modal="true" aria-labelledby="vsw-modal-title">

      <header class="vsw-header">
        <Icon name="code" :size="16" />
        <span id="vsw-modal-title" class="vsw-title">Versions — {{ workflow.name }}</span>
        <button class="vsw-close" aria-label="Close" @click="close">
          <Icon name="close" :size="14" />
        </button>
      </header>

      <hr class="vsw-divider" />

      <section class="vsw-body" data-testid="version-list">
        <p v-if="loading" class="vsw-loading">Loading versions…</p>
        <p v-else-if="loadError" class="vsw-error" role="alert" data-testid="load-error">{{ loadError }}</p>
        <p v-else-if="versions.length === 0" class="vsw-empty">No versions found.</p>
        <ul v-else class="version-list">
          <li
            v-for="v in versions"
            :key="v.version"
            class="version-item"
            :class="{ 'version-item--active': v.version === workflow.version }"
            data-testid="version-item"
          >
            <div class="version-info">
              <span class="version-num">v{{ v.version }}</span>
              <span
                class="version-state"
                :class="v.approvalState === 'approved' ? 'state-approved' : 'state-other'"
              >{{ v.approvalState }}</span>
              <span v-if="v.version === workflow.version" class="active-badge">active</span>
            </div>
            <div class="version-actions">
              <button
                v-if="v.approvalState === 'approved' && v.version !== workflow.version"
                class="btn-pin"
                :disabled="pinPending"
                :data-testid="`btn-pin-${v.version}`"
                @click="pin(v.version)"
              >Pin</button>
              <template v-if="v.approvalState === 'proposed' && workflow.isOwner">
                <button
                  class="btn-approve"
                  :disabled="decidePending"
                  :data-testid="`btn-approve-${v.version}`"
                  @click="decide(v.version, true)"
                >Approve</button>
                <button
                  class="btn-reject"
                  :disabled="decidePending"
                  :data-testid="`btn-reject-${v.version}`"
                  @click="decide(v.version, false)"
                >Reject</button>
              </template>
            </div>
          </li>
        </ul>
      </section>

      <p v-if="actionError" class="vsw-error" role="alert" data-testid="action-error">{{ actionError }}</p>

      <footer class="vsw-footer">
        <button
          class="btn-clear"
          :disabled="clearPending"
          data-testid="btn-clear-pin"
          @click="clear"
        >
          <Icon v-if="clearPending" name="spinner" :size="14" class="spin" />
          {{ clearPending ? "Clearing…" : "Clear pin (use latest)" }}
        </button>
        <button class="btn-cancel" @click="close">Close</button>
      </footer>

    </div>
  </div>
</template>

<style scoped>
.vsw-backdrop {
  position: fixed;
  inset: 0;
  background: var(--overlay);
  display: flex;
  align-items: center;
  justify-content: center;
  z-index: 100;
}

.vsw-modal {
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: var(--space-4) var(--space-5);
  min-width: 320px;
  max-width: 480px;
  width: 100%;
  max-height: min(80vh, 560px);
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
  overflow: hidden;
}

.vsw-header {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-weight: 500;
  color: var(--text);
  flex-shrink: 0;
}

.vsw-title {
  flex: 1;
  font-size: 0.9375rem;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.vsw-close {
  background: transparent;
  border: none;
  cursor: pointer;
  color: var(--text-muted);
  padding: var(--space-1);
  display: flex;
  align-items: center;
}

.vsw-divider {
  border: none;
  border-top: 1px solid var(--border);
  margin: 0;
  flex-shrink: 0;
}

.vsw-body {
  overflow-y: auto;
  /* basis auto (not 0): in the modal's auto-height flex column a 0 basis
     collapses the body to 0 height and clips the list. */
  flex: 1 1 auto;
  min-height: 0;
}

.vsw-loading,
.vsw-empty {
  font-size: 0.875rem;
  color: var(--text-faint);
  margin: 0;
}

.vsw-error {
  margin: 0;
  font-size: 0.8125rem;
  color: var(--danger);
  flex-shrink: 0;
}

.version-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}

.version-item {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-3);
  padding: var(--space-2) var(--space-3);
  border-radius: var(--radius-sm);
  border: 1px solid var(--border);
  background: var(--bg);
}

.version-item--active {
  border-color: var(--accent);
  background: var(--fill-accent);
}

.version-info {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  flex: 1;
}

.version-num {
  font-family: var(--font-mono);
  font-size: 0.875rem;
  font-weight: 500;
  color: var(--text);
}

.version-state {
  font-size: 0.6875rem;
  padding: 0.125rem 0.375rem;
  border-radius: 999px;
  font-weight: 500;
}

.state-approved {
  background: var(--fill-muted);
  color: var(--ok);
}

.state-other {
  background: var(--fill-muted);
  color: var(--text-muted);
}

.active-badge {
  font-size: 0.6875rem;
  padding: 0.125rem 0.375rem;
  border-radius: 999px;
  background: var(--fill-accent);
  color: var(--accent);
  font-weight: 600;
}

.version-actions {
  flex-shrink: 0;
}

.btn-pin {
  padding: 0.25rem 0.625rem;
  font-size: 0.8125rem;
  font-family: var(--font-sans);
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--text);
  cursor: pointer;
  transition: background 0.15s;
}

.btn-pin:not(:disabled):hover {
  background: var(--fill-muted);
}

.btn-pin:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}

.vsw-footer {
  display: flex;
  gap: var(--space-2);
  justify-content: space-between;
  padding-top: var(--space-4);
  border-top: 1px solid var(--border);
  flex-shrink: 0;
}

.btn-clear {
  display: inline-flex;
  align-items: center;
  gap: var(--space-2);
  padding: var(--space-2) var(--space-3);
  border-radius: var(--radius-sm);
  font-family: var(--font-sans);
  font-size: 0.8125rem;
  cursor: pointer;
  background: transparent;
  color: var(--text-muted);
  border: 1px solid var(--border);
  transition: background 0.15s;
}

.btn-clear:not(:disabled):hover {
  background: var(--fill-muted);
  color: var(--text);
}

.btn-clear:disabled {
  opacity: 0.5;
  cursor: not-allowed;
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

.btn-approve,
.btn-reject {
  padding: 0.25rem 0.625rem;
  font-size: 0.8125rem;
  font-family: var(--font-sans);
  border-radius: var(--radius-sm);
  cursor: pointer;
  transition: background 0.15s;
  border: 1px solid transparent;
}

.btn-approve {
  background: var(--fill-muted);
  color: var(--ok);
  border-color: var(--ok);
}

.btn-approve:not(:disabled):hover {
  background: var(--ok);
  color: #fff;
}

.btn-reject {
  background: var(--fill-muted);
  color: var(--danger);
  border-color: var(--danger);
}

.btn-reject:not(:disabled):hover {
  background: var(--danger);
  color: #fff;
}

.btn-approve:disabled,
.btn-reject:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}
</style>
