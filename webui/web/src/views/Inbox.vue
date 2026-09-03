<script setup>
import { ref, onMounted, nextTick } from "vue";
import { useRouter } from "vue-router";
import { useUserStore } from "../store/user.js";
import { useInboxStore } from "../store/inbox.js";
import { usePromptStore } from "../store/prompt.js";
import Icon from "../components/Icon.vue";
import Spinner from "../components/ui/Spinner.vue";
import EmptyState from "../components/ui/EmptyState.vue";
import Toast from "../components/ui/Toast.vue";
import { useToast } from "../components/ui/useToast.js";
import EnvelopeRow from "./inbox/EnvelopeRow.vue";
import SkillTransferModal from "../components/SkillTransferModal.vue";
import { listInbox, delegate, dismiss } from "../api/inbox.js";

// Gate: set to true when the delegate form should be visible again.
const SHOW_DELEGATE_FORM = false;

const router      = useRouter();
const userStore   = useUserStore();
const inboxStore  = useInboxStore();
const promptStore = usePromptStore();
const { push }    = useToast();

const envelopes = ref([]);
const error     = ref("");
const loading   = ref(false);

// Delegate form state (kept for future use — not currently shown).
const delegateTo      = ref("");
const delegateIntent  = ref("");
const delegateScopes  = ref("");
const delegateMaxCost = ref(50);
const delegateResult  = ref("");

// Ref for the list container — used for post-dismiss focus management.
const listRef = ref(null);

async function load() {
  error.value = "";
  try {
    const data = await listInbox();
    envelopes.value = data.envelopes ?? [];
    inboxStore.setCount(envelopes.value.length);
  } catch (e) {
    error.value = e.message || "Failed to load inbox";
  }
}

async function sendToAgent(env) {
  const intent = env.task?.intent ?? "";
  if (!intent.trim()) {
    push("error", "Nothing to send");
    return;
  }
  promptStore.set(intent);
  try {
    await dismiss(env.envelopeId);
  } catch (e) {
    // Dismiss failure must not block opening the chat — surface it and proceed.
    push("error", e.message || "Failed to remove delegation");
  }
  router.push("/chat");
}

function startSession(env) {
  promptStore.setPrefill(env.task?.intent ?? "");
  router.push("/chat");
}

// ── Skill transfer review ──────────
const reviewTarget  = ref(null); // the envelope being reviewed
const reviewVisible = ref(false);

function reviewTransfer(env) {
  reviewTarget.value  = env;
  reviewVisible.value = true;
}

function closeReview() {
  reviewVisible.value = false;
}

async function onTransferAccepted() {
  reviewVisible.value = false;
  push("ok", "Skill installed.");
  await load();
}

async function okDismiss(id) {
  error.value = "";

  // Optimistic remove.
  const idx = envelopes.value.findIndex((e) => e.envelopeId === id);
  const removed = idx !== -1 ? envelopes.value.splice(idx, 1)[0] : null;
  inboxStore.setCount(envelopes.value.length);

  // Move focus before the row is torn down.
  await nextTick();
  if (listRef.value) {
    const nextBtn = listRef.value.querySelector(`[data-testid^="send-to-agent-"]`);
    if (nextBtn) {
      nextBtn.focus();
    }
  }

  try {
    await dismiss(id);
    push("ok", "Dismissed");
  } catch (e) {
    // Restore on failure.
    if (removed !== null) {
      envelopes.value.splice(idx, 0, removed);
      inboxStore.setCount(envelopes.value.length);
    }
    error.value = e.message || "Failed to dismiss";
    push("error", e.message || "Failed to dismiss");
  }
}

async function submitDelegate() {
  error.value = "";
  delegateResult.value = "";
  try {
    const scopes = delegateScopes.value.split(",").map(s => s.trim()).filter(Boolean);
    const result = await delegate({
      to:      delegateTo.value.trim(),
      intent:  delegateIntent.value.trim(),
      scopes,
      maxCost: Number(delegateMaxCost.value),
    });
    delegateResult.value = `Delegated — envelope ${result.envelopeId ?? "sent"}`;
    delegateTo.value     = "";
    delegateIntent.value = "";
    delegateScopes.value = "";
  } catch (e) {
    error.value = e.message || "Delegation failed";
  }
}

onMounted(async () => {
  loading.value = true;
  try {
    await load();
  } finally {
    loading.value = false;
  }
});
</script>

<template>
  <div class="view">
    <header class="view-header">
      <Icon name="inbox" :size="20" />
      <h2 class="view-title">Inbox</h2>
      <span
        v-if="envelopes.length > 0"
        class="count-chip"
        :aria-label="`${envelopes.length} pending delegations`"
      >{{ envelopes.length }}</span>
    </header>

    <div v-if="error" class="error-banner" data-testid="error-banner">
      <Icon name="close" :size="14" />
      {{ error }}
    </div>

    <!-- Body states: loading > empty > list -->
    <div v-if="loading" class="body-center">
      <Spinner size="lg" />
    </div>

    <template v-else>
      <section class="section">
        <h3 class="section-title">Pending delegations</h3>

        <EmptyState
          v-if="envelopes.length === 0 && !error"
          icon="inbox"
          message="No pending delegations."
        />

        <ul v-else class="envelope-list" ref="listRef">
          <EnvelopeRow
            v-for="env in envelopes"
            :key="env.envelopeId"
            :envelope="env"
            @send-to-agent="sendToAgent"
            @start-session="startSession"
            @dismiss="okDismiss"
            @review="reviewTransfer"
          />
        </ul>
      </section>
    </template>

    <SkillTransferModal
      :envelope-id="reviewTarget?.envelopeId ?? ''"
      :from-display-name="reviewTarget?.fromDisplayName ?? reviewTarget?.fromUserId ?? ''"
      :visible="reviewVisible"
      @close="closeReview"
      @accepted="onTransferAccepted"
    />

    <!-- Delegate form (hidden until SHOW_DELEGATE_FORM is enabled) -->
    <section v-if="SHOW_DELEGATE_FORM" class="section">
      <h3 class="section-title">Delegate a task</h3>
      <form class="delegate-form" data-testid="delegate-form" @submit.prevent="submitDelegate">
        <input
          v-model="delegateTo"
          class="field"
          placeholder="Delegate to (user@example.com)"
          data-testid="delegate-to"
          required
        />
        <input
          v-model="delegateIntent"
          class="field"
          placeholder="Task intent"
          data-testid="delegate-intent"
          required
        />
        <input
          v-model="delegateScopes"
          class="field"
          placeholder="Scopes (comma-separated, e.g. siem:read)"
          data-testid="delegate-scopes"
        />
        <div class="form-inline">
          <label class="field-label">Max cost units</label>
          <input
            v-model.number="delegateMaxCost"
            class="field field-narrow"
            type="number"
            min="0"
            data-testid="delegate-max-cost"
          />
        </div>
        <div class="form-actions">
          <button type="submit" class="btn-accent">
            <Icon name="send" :size="14" />
            Delegate
          </button>
        </div>
        <p v-if="delegateResult" class="delegate-result">{{ delegateResult }}</p>
      </form>
    </section>

    <Toast />
  </div>
</template>

<style scoped>
.view {
  padding: var(--space-8);
  max-width: 720px;
  margin: 0 auto;
  display: flex;
  flex-direction: column;
  gap: var(--space-8);
}

@media (max-width: 640px) {
  .view {
    padding: var(--space-6);
  }
}

.view-header {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  color: var(--text);
}

.view-title {
  font-family: var(--font-sans);
  font-size: 1.25rem;
  font-weight: 500;
  margin: 0;
}

.count-chip {
  margin-left: var(--space-2);
  background: var(--fill-muted);
  color: var(--text-muted);
  font-size: 0.75rem;
  font-weight: 600;
  padding: 2px var(--space-2);
  border-radius: 999px;
  line-height: 1.4;
}

.error-banner {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  padding: var(--space-3) var(--space-4);
  background: var(--fill-danger);
  border: 1px solid var(--danger);
  border-radius: var(--radius-sm);
  color: var(--danger);
  font-size: 0.875rem;
}

.body-center {
  display: flex;
  justify-content: center;
  padding: var(--space-8) 0;
}

.section {
  display: flex;
  flex-direction: column;
  gap: var(--space-6);
}

.section-title {
  font-size: 0.8125rem;
  font-weight: 500;
  color: var(--text-muted);
  text-transform: uppercase;
  letter-spacing: 0.05em;
  margin: 0;
}

.envelope-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}

/* Delegate form */
.delegate-form {
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: 1.25rem;
  display: flex;
  flex-direction: column;
  gap: 0.625rem;
}

.field {
  background: var(--bg);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--text);
  font-family: var(--font-sans);
  font-size: 0.875rem;
  padding: 0.5rem 0.75rem;
  outline: none;
  width: 100%;
}

.field:focus {
  border-color: var(--accent);
}

.field-label {
  font-size: 0.8125rem;
  color: var(--text-muted);
  flex-shrink: 0;
}

.field-narrow {
  width: 6rem;
}

.form-inline {
  display: flex;
  align-items: center;
  gap: 0.75rem;
}

.form-actions {
  display: flex;
  justify-content: flex-end;
  margin-top: 0.25rem;
}

.btn-accent {
  display: inline-flex;
  align-items: center;
  gap: 0.375rem;
  padding: 0.5rem 1rem;
  background: var(--accent);
  border: none;
  border-radius: var(--radius-sm);
  color: var(--text-on-accent);
  font-family: var(--font-sans);
  font-size: 0.875rem;
  cursor: pointer;
  transition: background 0.15s;
}

.btn-accent:hover {
  background: var(--accent-hover);
}

.delegate-result {
  font-size: 0.875rem;
  color: var(--ok);
  margin: 0;
}
</style>
