<script setup>
// Personal skills — inbox transfer preview + accept modal. On open, fetches the
// preview and renders the FULL cross-user body as TEXT ({{ }} interpolation,
// never v-html — SKILL.md is untrusted, attacker-controlled markdown, same
// convention as MemorySettings.vue's concept body) plus the file manifest and
// any injection flags, displayed loud so the accept decision is informed.
import { computed, ref, watch } from "vue";
import Icon from "./Icon.vue";
import ErrorBanner from "./ui/ErrorBanner.vue";
import { getSkillTransferPreview, acceptSkillTransfer } from "../api/skills.js";

const props = defineProps({
  envelopeId: { type: String, default: "" },
  fromDisplayName: { type: String, default: "" },
  visible: { type: Boolean, default: false },
});

const emit = defineEmits(["close", "accepted"]);

const loading = ref(false);
const loadError = ref("");
const forbidden = ref(false);
const preview = ref(null); // { skillName, fromUserId, body, manifest, flags, contentHash, conflict }

const mode = ref("rename"); // "rename" | "replace" — replace only ever offered on a conflict.
const nameOverride = ref("");
const submitting = ref(false);
const submitError = ref("");

async function load() {
  loading.value = true;
  loadError.value = "";
  forbidden.value = false;
  preview.value = null;
  try {
    const data = await getSkillTransferPreview(props.envelopeId);
    if (data.forbidden) {
      forbidden.value = true;
      return;
    }
    preview.value = data;
    mode.value = "rename";
    // ponytail: the preview RPC reports only a conflict boolean, not the
    // recipient's actual free name — this is a client-side guess the user can
    // edit, not the broker's authority (AcceptSkillTransfer computes the real
    // first-free-<name>-N if left blank). Upgrade path: have the preview RPC
    // return a suggested name if a bad guess ever bites in practice.
    nameOverride.value = data.conflict ? `${data.skillName}-2` : "";
  } catch (e) {
    loadError.value = e.message || "Failed to load transfer preview";
  } finally {
    loading.value = false;
  }
}

// Reset + refetch on every open (ShareSkillModal precedent) — a stale preview
// or pick from a previous envelope must never survive into this one.
watch(
  () => props.visible,
  (val) => {
    if (!val) return;
    submitError.value = "";
    submitting.value = false;
    load();
  },
  { immediate: true },
);

const senderLabel = computed(() => props.fromDisplayName || preview.value?.fromUserId || "");

function formatSize(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

async function confirm() {
  if (submitting.value || !preview.value) return;
  submitting.value = true;
  submitError.value = "";
  try {
    const body =
      mode.value === "replace"
        ? { mode: "replace" }
        : preview.value.conflict
          ? { mode: "rename", name_override: nameOverride.value.trim() }
          : { mode: "rename" };
    await acceptSkillTransfer(props.envelopeId, body);
    emit("accepted");
  } catch (e) {
    submitError.value = e.message || "Accept failed";
  } finally {
    submitting.value = false;
  }
}

function close() {
  emit("close");
}
</script>

<template>
  <div v-if="visible" class="transfer-backdrop" data-testid="skill-transfer-modal">
    <div class="transfer-modal" role="dialog" aria-modal="true" aria-labelledby="transfer-modal-title">
      <header class="transfer-header">
        <Icon name="book" :size="16" />
        <span id="transfer-modal-title" class="transfer-title">Skill transfer</span>
        <button class="transfer-close" aria-label="Close" @click="close">
          <Icon name="close" :size="14" />
        </button>
      </header>

      <hr class="transfer-divider" />

      <p v-if="loading" class="transfer-loading">Loading…</p>

      <ErrorBanner v-else-if="loadError" :message="loadError">
        <template #action>
          <button class="btn-cancel" data-testid="transfer-retry" @click="load">Retry</button>
        </template>
      </ErrorBanner>

      <p v-else-if="forbidden" class="transfer-loading" data-testid="transfer-forbidden">
        You do not have access to this transfer.
      </p>

      <template v-else-if="preview">
        <p class="transfer-sender" data-testid="transfer-sender">
          From <strong>{{ senderLabel }}</strong> — <strong>{{ preview.skillName }}</strong>
        </p>

        <div v-if="preview.flags?.length" class="transfer-flags" data-testid="transfer-flags-warning" role="alert">
          <Icon name="close" :size="16" />
          <div>
            <strong>Flagged content — review before accepting</strong>
            <ul>
              <li v-for="f in preview.flags" :key="f">{{ f }}</li>
            </ul>
          </div>
        </div>

        <div class="transfer-section">
          <h4 class="transfer-section-title">Body</h4>
          <pre class="transfer-body" data-testid="transfer-body">{{ preview.body }}</pre>
        </div>

        <div class="transfer-section">
          <h4 class="transfer-section-title">Files</h4>
          <ul class="manifest-list" data-testid="transfer-manifest">
            <li v-for="f in preview.manifest" :key="f.path" class="manifest-row">
              <span class="manifest-path">{{ f.path }}</span>
              <span class="manifest-size">{{ formatSize(f.size) }}</span>
            </li>
          </ul>
        </div>

        <div class="transfer-section">
          <h4 class="transfer-section-title">Accept as</h4>
          <label class="mode-row">
            <input v-model="mode" type="radio" value="rename" data-testid="transfer-mode-rename" />
            Rename
          </label>
          <input
            v-if="mode === 'rename' && preview.conflict"
            v-model="nameOverride"
            type="text"
            class="name-input"
            data-testid="transfer-name-input"
            placeholder="New name"
          />
          <template v-if="preview.conflict">
            <label class="mode-row">
              <input v-model="mode" type="radio" value="replace" data-testid="transfer-mode-replace" />
              Replace
            </label>
            <p v-if="mode === 'replace'" class="transfer-danger-hint" data-testid="transfer-replace-hint">
              This deletes your existing Skills/{{ preview.skillName }}/ and replaces it with the transferred
              version. This cannot be undone.
            </p>
          </template>
        </div>

        <p v-if="submitError" class="transfer-error" role="alert" data-testid="transfer-error">{{ submitError }}</p>
      </template>

      <footer class="transfer-footer">
        <button class="btn-cancel" :disabled="submitting" data-testid="transfer-cancel-btn" @click="close">
          Cancel
        </button>
        <button
          v-if="preview"
          class="btn-confirm"
          :class="{ 'btn-danger-solid': mode === 'replace' }"
          :disabled="submitting"
          data-testid="transfer-confirm-btn"
          @click="confirm"
        >
          {{ submitting ? "Working…" : mode === "replace" ? "Replace" : "Accept" }}
        </button>
      </footer>
    </div>
  </div>
</template>

<style scoped>
.transfer-backdrop {
  position: fixed;
  inset: 0;
  background: var(--overlay);
  display: flex;
  align-items: center;
  justify-content: center;
  z-index: 100;
}

.transfer-modal {
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: var(--space-4) var(--space-5);
  min-width: 320px;
  max-width: 560px;
  width: 100%;
  max-height: min(85vh, 640px);
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
  overflow-y: auto;
}

.transfer-header {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-weight: 500;
  color: var(--text);
  flex-shrink: 0;
}

.transfer-title {
  flex: 1;
  font-size: 0.9375rem;
}

.transfer-close {
  background: transparent;
  border: none;
  cursor: pointer;
  color: var(--text-muted);
  padding: var(--space-1);
  display: flex;
  align-items: center;
}

.transfer-divider {
  border: none;
  border-top: 1px solid var(--border);
  margin: 0;
  flex-shrink: 0;
}

.transfer-loading {
  font-size: 0.875rem;
  color: var(--text-faint);
  margin: 0;
}

.transfer-sender {
  margin: 0;
  font-size: 0.875rem;
  color: var(--text);
}

.transfer-flags {
  display: flex;
  gap: var(--space-2);
  padding: 0.75rem 1rem;
  background: var(--fill-danger);
  border: 1px solid var(--danger);
  border-radius: var(--radius-sm);
  color: var(--danger);
  font-size: 0.875rem;
}

.transfer-flags ul {
  margin: var(--space-1) 0 0;
  padding-left: 1.25rem;
}

.transfer-section {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}

.transfer-section-title {
  font-size: 0.75rem;
  font-weight: 500;
  color: var(--text-muted);
  text-transform: uppercase;
  letter-spacing: 0.05em;
  margin: 0;
}

.transfer-body {
  margin: 0;
  max-height: 220px;
  overflow: auto;
  background: var(--fill-muted);
  border-radius: var(--radius-sm);
  padding: 0.75rem;
  font-family: var(--font-mono);
  font-size: 0.8125rem;
  white-space: pre-wrap;
  word-break: break-word;
}

.manifest-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
  max-height: 140px;
  overflow-y: auto;
}

.manifest-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-2);
  font-size: 0.8125rem;
  padding: 2px 0;
}

.manifest-path {
  color: var(--text);
  font-family: var(--font-mono);
  overflow-wrap: anywhere;
}

.manifest-size {
  color: var(--text-muted);
  flex-shrink: 0;
}

.mode-row {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: 0.875rem;
  color: var(--text);
  cursor: pointer;
}

.name-input {
  padding: 0.4375rem 0.75rem;
  background: var(--bg);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--text);
  font-family: var(--font-sans);
  font-size: 0.875rem;
  margin-left: 1.5rem;
}

.transfer-danger-hint {
  margin: 0 0 0 1.5rem;
  font-size: 0.8125rem;
  color: var(--danger);
}

.transfer-error {
  margin: 0;
  font-size: 0.8125rem;
  color: var(--danger);
}

.transfer-footer {
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

.btn-confirm {
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

.btn-confirm:not(:disabled):hover {
  filter: brightness(1.1);
}

.btn-confirm.btn-danger-solid {
  background: var(--danger);
}

.btn-cancel:disabled,
.btn-confirm:disabled {
  cursor: not-allowed;
  opacity: 0.5;
}
</style>
