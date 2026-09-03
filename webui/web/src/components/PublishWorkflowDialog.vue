<script setup>
import { ref, computed, watch } from "vue";
import { storeToRefs } from "pinia";
import Icon from "./Icon.vue";
import ToggleSwitch from "./ui/ToggleSwitch.vue";
import { publishWorkflow } from "../api/workflows.js";
import { useDiscoveryStore } from "../store/discovery.js";

const props = defineProps({
  // The whole workflow row; only lineageId/version are read below.
  workflow: { type: Object, default: null },
  visible: { type: Boolean, default: false },
});

const emit = defineEmits(["close", "published"]);

// ── groups from the shared discovery store ───────────────────────────────────

const discoveryStore = useDiscoveryStore();
const { delegatableGroups: groups, loading: groupsLoading, errors: discoveryErrors } =
  storeToRefs(discoveryStore);
const groupsError = computed(() => discoveryErrors.value.delegatableGroups);

// Refresh on every open so the list is never stale (mirrors ApprovalModal pattern).
watch(
  () => props.visible,
  (val) => { if (val) discoveryStore.refresh(); },
  { immediate: true },
);

// ── selection + submit ────────────────────────────────────────────────────────

const selectedGroupIds = ref([]);
const pending = ref(false);
const error = ref(null);

// Reset selection when dialog opens.
watch(
  () => props.visible,
  (val) => {
    if (val) {
      selectedGroupIds.value = [];
      error.value = null;
    }
  },
);

function toggleGroup(groupId) {
  const idx = selectedGroupIds.value.indexOf(groupId);
  if (idx === -1) {
    selectedGroupIds.value = [...selectedGroupIds.value, groupId];
  } else {
    selectedGroupIds.value = selectedGroupIds.value.filter((id) => id !== groupId);
  }
}

// Display label: strip the "group:" FGA-object prefix the directory returns
// ("group:security-team" → "security-team"). The full id is still sent on submit;
// the broker normalizes either form.
function groupLabel(g) {
  const raw = g.displayName ?? g.groupId ?? "";
  return raw.replace(/^group:/, "");
}

const canSubmit = computed(() => selectedGroupIds.value.length > 0 && !pending.value);

async function submit() {
  if (!canSubmit.value || !props.workflow) return;
  pending.value = true;
  error.value = null;
  try {
    await publishWorkflow(props.workflow.lineageId, {
      version: props.workflow.version,
      groupIds: selectedGroupIds.value,
    });
    emit("published");
    emit("close");
  } catch (err) {
    error.value = err?.message ?? "Publish failed";
  } finally {
    pending.value = false;
  }
}

function close() {
  emit("close");
}
</script>

<template>
  <div v-if="visible && workflow" class="pub-backdrop">
    <div class="pub-modal" role="dialog" aria-modal="true" aria-labelledby="pub-modal-title">

      <header class="pub-header">
        <Icon name="send" :size="16" />
        <span id="pub-modal-title" class="pub-title">Publish Workflow</span>
        <button class="pub-close" aria-label="Close" @click="close">
          <Icon name="close" :size="14" />
        </button>
      </header>

      <hr class="pub-divider" />

      <p class="pub-hint">Select the groups to share this workflow with:</p>

      <section class="pub-groups" data-testid="group-list">
        <p v-if="groupsLoading" class="pub-loading">Loading groups…</p>
        <p v-else-if="groupsError" class="pub-error" role="alert">{{ groupsError }}</p>
        <p v-else-if="groups.length === 0" class="pub-empty">No groups available.</p>
        <template v-else>
          <label
            v-for="g in groups"
            :key="g.groupId"
            class="group-row"
            :data-testid="`group-${g.groupId}`"
          >
            <ToggleSwitch
              :model-value="selectedGroupIds.includes(g.groupId)"
              :aria-label="groupLabel(g)"
              @update:model-value="toggleGroup(g.groupId)"
            />
            <span class="group-name">{{ groupLabel(g) }}</span>
          </label>
        </template>
      </section>

      <p v-if="error" class="pub-error" role="alert" data-testid="pub-error">{{ error }}</p>

      <footer class="pub-footer">
        <button class="btn-cancel" :disabled="pending" @click="close">Cancel</button>
        <button
          class="btn-publish"
          :disabled="!canSubmit"
          :aria-busy="pending"
          data-testid="btn-publish"
          @click="submit"
        >
          <Icon v-if="pending" name="spinner" :size="14" class="spin" />
          <Icon v-else name="send" :size="14" />
          {{ pending ? "Publishing…" : "Publish" }}
        </button>
      </footer>

    </div>
  </div>
</template>

<style scoped>
.pub-backdrop {
  position: fixed;
  inset: 0;
  background: var(--overlay);
  display: flex;
  align-items: center;
  justify-content: center;
  z-index: 100;
}

.pub-modal {
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

.pub-header {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-weight: 500;
  color: var(--text);
  flex-shrink: 0;
}

.pub-title {
  flex: 1;
  font-size: 0.9375rem;
}

.pub-close {
  background: transparent;
  border: none;
  cursor: pointer;
  color: var(--text-muted);
  padding: var(--space-1);
  display: flex;
  align-items: center;
}

.pub-divider {
  border: none;
  border-top: 1px solid var(--border);
  margin: 0;
  flex-shrink: 0;
}

.pub-hint {
  margin: 0;
  font-size: 0.875rem;
  color: var(--text-muted);
  flex-shrink: 0;
}

.pub-groups {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  overflow-y: auto;
  flex: 1 1 auto;
  min-height: 0;
}

.pub-loading,
.pub-empty {
  font-size: 0.875rem;
  color: var(--text-faint);
  margin: 0;
}

.pub-error {
  margin: 0;
  font-size: 0.8125rem;
  color: var(--danger);
  flex-shrink: 0;
}

.group-row {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: 0.875rem;
  color: var(--text);
  cursor: pointer;
  padding: var(--space-1) 0;
}

.group-row .toggle {
  flex-shrink: 0;
  flex-shrink: 0;
}

.group-name {
  user-select: none;
}

.pub-footer {
  display: flex;
  gap: var(--space-2);
  justify-content: flex-end;
  padding-top: var(--space-4);
  border-top: 1px solid var(--border);
  flex-shrink: 0;
}

.btn-publish {
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

.btn-publish:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}

.btn-publish:not(:disabled):hover {
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
