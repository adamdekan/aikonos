<script setup>
// Personal skills — Share modal.
// Reuses the shared delegation discovery store (users + groups, same dataset
// PublishWorkflowDialog/Composer's @-mention palette use) and the pick →
// confirm flow Chat.vue's delegate modal follows, but inline in one dialog
// instead of Composer-draft + separate confirm modal — there is no draft text
// here, only a target to pick and confirm.
import { ref, computed, watch } from "vue";
import { storeToRefs } from "pinia";
import Icon from "./Icon.vue";
import ErrorBanner from "./ui/ErrorBanner.vue";
import { useDiscoveryStore } from "../store/discovery.js";
import { shareSkill } from "../api/skills.js";

const props = defineProps({
  skillName: { type: String, default: "" },
  visible: { type: Boolean, default: false },
});

const emit = defineEmits(["close", "shared"]);

const discoveryStore = useDiscoveryStore();
const { delegatableUsers, delegatableGroups, loading, errors } = storeToRefs(discoveryStore);
const targetsError = computed(() => errors.value.delegatableUsers || errors.value.delegatableGroups);

// step: "pick" (choose a user/group) → "confirm" (verify target) → "result"
// (share sent — success text + group skip-report, if any).
const step = ref("pick");
const query = ref("");
const target = ref(null); // { kind: "user", userId, displayName } | { kind: "group", groupId, displayName, memberCount }
const submitting = ref(false);
const submitError = ref("");
const skippedCount = ref(0);

// Refresh discovery + reset all local state on every open (ApprovalModal/
// PublishWorkflowDialog precedent) — a stale pick from a previous open must
// never survive into this one.
watch(
  () => props.visible,
  (val) => {
    if (!val) return;
    discoveryStore.refresh();
    step.value = "pick";
    query.value = "";
    target.value = null;
    submitError.value = "";
    skippedCount.value = 0;
  },
  { immediate: true },
);

const filteredTargets = computed(() => {
  const q = query.value.trim().toLowerCase();
  const users = delegatableUsers.value.map((u) => ({ kind: "user", userId: u.userId, displayName: u.displayName }));
  const groups = delegatableGroups.value.map((g) => ({
    kind: "group",
    groupId: g.groupId,
    displayName: g.displayName,
    memberCount: g.memberCount,
  }));
  const all = [...users, ...groups];
  if (!q) return all;
  return all.filter((item) => item.displayName.toLowerCase().includes(q));
});

function selectTarget(item) {
  target.value = item;
  step.value = "confirm";
  submitError.value = "";
}

function backToPick() {
  step.value = "pick";
  target.value = null;
}

async function submit() {
  if (!target.value || submitting.value) return;
  submitting.value = true;
  submitError.value = "";
  try {
    const recipient =
      target.value.kind === "group" ? { group_id: target.value.groupId } : { user_id: target.value.userId };
    const resp = await shareSkill(props.skillName, recipient);
    skippedCount.value = (resp.skippedUserIds ?? []).length;
    step.value = "result";
    emit("shared", resp);
  } catch (e) {
    submitError.value = e.message || "Share failed";
  } finally {
    submitting.value = false;
  }
}

function close() {
  emit("close");
}
</script>

<template>
  <div v-if="visible" class="share-backdrop" data-testid="share-modal">
    <div class="share-modal" role="dialog" aria-modal="true" aria-labelledby="share-modal-title">
      <header class="share-header">
        <Icon name="send" :size="16" />
        <span id="share-modal-title" class="share-title">Share "{{ skillName }}"</span>
        <button class="share-close" aria-label="Close" @click="close">
          <Icon name="close" :size="14" />
        </button>
      </header>

      <hr class="share-divider" />

      <template v-if="step === 'pick'">
        <input
          v-model="query"
          type="text"
          class="share-search"
          data-testid="share-search"
          placeholder="Search people or groups…"
        />
        <ErrorBanner v-if="targetsError" :message="targetsError" />
        <p v-else-if="loading" class="share-loading">Loading…</p>
        <ul v-else class="share-target-list" data-testid="share-target-list">
          <li v-for="item in filteredTargets" :key="item.kind === 'group' ? `g:${item.groupId}` : `u:${item.userId}`">
            <button
              type="button"
              class="target-row"
              :data-testid="`share-target-${item.kind === 'group' ? item.groupId : item.userId}`"
              @click="selectTarget(item)"
            >
              <span class="target-name">{{ item.displayName }}</span>
              <span v-if="item.kind === 'group'" class="badge badge-muted">group · {{ item.memberCount }}</span>
            </button>
          </li>
        </ul>
        <p v-if="!loading && !targetsError && filteredTargets.length === 0" class="share-empty">No matches.</p>
      </template>

      <template v-else-if="step === 'confirm'">
        <p class="share-confirm-text">
          Share <strong>{{ skillName }}</strong> with
          <template v-if="target.kind === 'group'">
            group <strong>{{ target.displayName }}</strong> ({{ target.memberCount }} people)?
          </template>
          <template v-else><strong>{{ target.displayName }}</strong>?</template>
        </p>
        <p v-if="target.kind === 'group'" class="share-hint">
          Only members holding this skill's grant will receive it — others are reported after sending.
        </p>
        <p v-if="submitError" class="share-error" role="alert" data-testid="share-error">{{ submitError }}</p>
      </template>

      <template v-else-if="step === 'result'">
        <p class="share-confirm-text" data-testid="share-success">
          Shared <strong>{{ skillName }}</strong> with <strong>{{ target.displayName }}</strong>.
        </p>
        <p v-if="target.kind === 'group' && skippedCount > 0" class="share-hint" data-testid="share-skip-report">
          {{ skippedCount }} member{{ skippedCount === 1 ? "" : "s" }} skipped — lack the skill.
        </p>
      </template>

      <footer class="share-footer">
        <template v-if="step === 'pick'">
          <button class="btn-cancel" @click="close">Cancel</button>
        </template>
        <template v-else-if="step === 'confirm'">
          <button class="btn-cancel" :disabled="submitting" @click="backToPick">Back</button>
          <button class="btn-share" :disabled="submitting" data-testid="share-confirm-btn" @click="submit">
            {{ submitting ? "Sharing…" : "Share" }}
          </button>
        </template>
        <template v-else>
          <button class="btn-share" data-testid="share-done-btn" @click="close">Done</button>
        </template>
      </footer>
    </div>
  </div>
</template>

<style scoped>
.share-backdrop {
  position: fixed;
  inset: 0;
  background: var(--overlay);
  display: flex;
  align-items: center;
  justify-content: center;
  z-index: 100;
}

.share-modal {
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

.share-header {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-weight: 500;
  color: var(--text);
  flex-shrink: 0;
}

.share-title {
  flex: 1;
  font-size: 0.9375rem;
}

.share-close {
  background: transparent;
  border: none;
  cursor: pointer;
  color: var(--text-muted);
  padding: var(--space-1);
  display: flex;
  align-items: center;
}

.share-divider {
  border: none;
  border-top: 1px solid var(--border);
  margin: 0;
  flex-shrink: 0;
}

.share-search {
  padding: 0.4375rem 0.75rem;
  background: var(--bg);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--text);
  font-family: var(--font-sans);
  font-size: 0.875rem;
  flex-shrink: 0;
}

.share-loading,
.share-empty {
  font-size: 0.875rem;
  color: var(--text-faint);
  margin: 0;
}

.share-target-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
  overflow-y: auto;
  flex: 1 1 auto;
  min-height: 0;
}

.target-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-2);
  width: 100%;
  text-align: left;
  background: none;
  border: 1px solid transparent;
  border-radius: var(--radius-sm);
  color: var(--text);
  font-size: 0.875rem;
  padding: 0.5rem 0.625rem;
  cursor: pointer;
}

.target-row:hover {
  background: var(--fill-muted);
}

.badge {
  font-size: 0.6875rem;
  padding: 0.125rem 0.5rem;
  border-radius: 999px;
  font-weight: 500;
  flex-shrink: 0;
}

.badge-muted {
  background: var(--fill-muted);
  color: var(--text-muted);
}

.share-confirm-text {
  margin: 0;
  font-size: 0.875rem;
  color: var(--text);
}

.share-hint {
  margin: 0;
  font-size: 0.8125rem;
  color: var(--text-muted);
}

.share-error {
  margin: 0;
  font-size: 0.8125rem;
  color: var(--danger);
}

.share-footer {
  display: flex;
  gap: var(--space-2);
  justify-content: flex-end;
  padding-top: var(--space-4);
  border-top: 1px solid var(--border);
  flex-shrink: 0;
}

.btn-cancel {
  padding: var(--space-2) var(--space-4);
  border-radius: var(--radius-sm);
  font-family: var(--font-sans);
  font-size: 0.875rem;
  font-weight: 500;
  cursor: pointer;
  background: transparent;
  color: var(--text);
  border: 1px solid var(--border);
}

.btn-cancel:not(:disabled):hover {
  background: var(--fill-muted);
}

.btn-share {
  padding: var(--space-2) var(--space-4);
  border-radius: var(--radius-sm);
  font-family: var(--font-sans);
  font-size: 0.875rem;
  font-weight: 500;
  cursor: pointer;
  background: var(--accent);
  color: var(--text-on-accent);
  border: 1px solid transparent;
}

.btn-share:not(:disabled):hover {
  filter: brightness(1.1);
}

.btn-cancel:disabled,
.btn-share:disabled {
  cursor: not-allowed;
  opacity: 0.5;
}
</style>
