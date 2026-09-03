<script setup>
import { ref, computed, watch, nextTick, onMounted, onBeforeUnmount } from "vue";
import { post } from "../api/client.js";
import Icon from "./Icon.vue";
import ArgsView from "./ArgsView.vue";
import ToggleSwitch from "./ui/ToggleSwitch.vue";

const props = defineProps({
  // The approval info object from aikonos.approval.request CUSTOM event.
  request: { type: Object, default: null },
  // Controls modal visibility. Parent sets false after close emitted.
  visible: { type: Boolean, default: false },
  // Total pending approvals (including this one) — F37 FIFO queue. 1 = no badge shown.
  pendingCount: { type: Number, default: 1 },
});

const emit = defineEmits(["close"]);

// ── state ──
const pending = ref(false);
const pendingAction = ref(null); // "approve" | "deny"
const error = ref(null);
const reviewed = ref(false);

// Whether the request is high-risk (step-up or future risk:"high" field).
// Approve is locked until reviewed when true.
const highRisk = computed(
  () => !!(props.request?.stepUp || props.request?.risk === "high")
);

const approveDisabled = computed(
  () => pending.value || (highRisk.value && !reviewed.value)
);

// ── DOM refs ──
const denyBtn = ref(null);
const approveBtn = ref(null);
const dialogEl = ref(null);

// Focus management
let previouslyFocused = null;

function trapFocus(e) {
  if (!dialogEl.value) return;
  const focusable = dialogEl.value.querySelectorAll(
    'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'
  );
  if (!focusable.length) return;
  const first = focusable[0];
  const last = focusable[focusable.length - 1];

  if (e.shiftKey) {
    if (document.activeElement === first) {
      e.preventDefault();
      last.focus();
    }
  } else {
    if (document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  }
}

function handleKeydown(e) {
  if (!props.visible) return;
  if (e.key === "Tab") {
    trapFocus(e);
  } else if (e.key === "Escape") {
    // Esc = fail-closed: deny/cancel without approving.
    respondCancel();
  }
}

async function openModal() {
  error.value = null;
  reviewed.value = false;
  previouslyFocused = document.activeElement;
  await nextTick();
  // Auto-focus Deny — the safe default.
  denyBtn.value?.focus();
}

function closeModal() {
  if (previouslyFocused && typeof previouslyFocused.focus === "function") {
    previouslyFocused.focus();
  }
  previouslyFocused = null;
}

watch(
  () => props.visible,
  (val) => {
    if (val) {
      openModal();
    } else {
      closeModal();
    }
  }
);

// Reset reviewed state when request changes (new approval prompt).
watch(
  () => props.request?.toolCallId,
  () => {
    reviewed.value = false;
    error.value = null;
  }
);

onMounted(() => {
  document.addEventListener("keydown", handleKeydown);
  if (props.visible) openModal();
});

onBeforeUnmount(() => {
  document.removeEventListener("keydown", handleKeydown);
});

// ── actions ──
async function respond(approved) {
  if (!props.request || pending.value) return;
  pending.value = true;
  pendingAction.value = approved ? "approve" : "deny";
  error.value = null;
  try {
    await post(`/approve/${encodeURIComponent(props.request.toolCallId)}`, {
      body: { approved },
    });
    emit("close");
  } catch (err) {
    // Keep modal open; surface inline error so the decision is not silently lost.
    error.value = err?.message ?? "Request failed. Please try again.";
  } finally {
    pending.value = false;
    pendingAction.value = null;
  }
}

function respondCancel() {
  // Esc / backdrop — treat as deny (fail-closed).
  if (!pending.value) respond(false);
}
</script>

<template>
  <div
    v-if="visible && request"
    class="approval-backdrop"
    aria-hidden="false"
  >
    <!-- Backdrop is intentionally inert — mis-click must not dismiss a security prompt. -->
    <div
      ref="dialogEl"
      class="approval-modal"
        role="dialog"
        aria-modal="true"
        aria-labelledby="approval-title"
        aria-describedby="approval-reason"
      >
        <!-- HEADER: what + risk -->
        <header class="approval-header">
          <Icon name="tool" :size="16" />
          <span id="approval-title" class="approval-tool-id">{{ request.toolId }}</span>
          <span
            class="approval-pill"
            :class="request.stepUp ? 'pill-stepup' : 'pill-human'"
            :aria-label="request.stepUp ? 'Step-up authentication required' : 'Human approval required'"
          >
            {{ request.stepUp ? "STEP-UP" : "HUMAN" }}
          </span>
          <span v-if="pendingCount > 1" class="approval-queue-count">1 of {{ pendingCount }}</span>
        </header>

        <hr class="approval-divider" />

        <!-- META: who (defensive — only rendered if request.agent present) -->
        <section v-if="request.agent" class="approval-meta">
          <div class="meta-row">
            <Icon name="bot" :size="14" />
            <span class="meta-label">Agent</span>
            <span class="meta-value">{{ request.agent }}</span>
          </div>
        </section>

        <!-- REASON: why -->
        <p v-if="request.reason" id="approval-reason" class="approval-reason">
          {{ request.reason }}
        </p>

        <hr class="approval-divider" />

        <!-- ARGS: structured k/v view, scrollable -->
        <section class="approval-args-section scroll-min">
          <h3 class="section-label">Arguments</h3>
          <ArgsView :args="request.args ?? null" />
        </section>

        <!-- Risk gate: checkbox unlocks Approve for high-risk / step-up calls -->
        <label v-if="highRisk && !reviewed" class="review-gate">
          <ToggleSwitch v-model="reviewed" aria-label="I have reviewed the arguments above" />
          <span>I have reviewed the arguments above</span>
        </label>

        <!-- Inline error (kept visible; modal stays open on failure) -->
        <p v-if="error" class="approval-error" role="alert">{{ error }}</p>

        <!-- FOOTER: decide — Deny first (ghost, auto-focused), Approve last (solid) -->
        <footer class="approval-actions">
          <button
            ref="denyBtn"
            class="btn-deny"
            data-action="deny"
            :disabled="pending"
            :aria-busy="pending && pendingAction === 'deny'"
            aria-label="Deny"
            @click="respond(false)"
          >
            <Icon
              v-if="pending && pendingAction === 'deny'"
              name="spinner"
              :size="14"
              class="spin"
            />
            <Icon v-else name="close" :size="14" />
            {{ pending && pendingAction === "deny" ? "Denying…" : "Deny" }}
          </button>

          <button
            ref="approveBtn"
            class="btn-approve"
            data-action="approve"
            :disabled="approveDisabled"
            :aria-busy="pending && pendingAction === 'approve'"
            aria-label="Approve"
            @click="respond(true)"
          >
            <Icon
              v-if="pending && pendingAction === 'approve'"
              name="spinner"
              :size="14"
              class="spin"
            />
            <Icon v-else name="check" :size="14" />
            {{ pending && pendingAction === "approve" ? "Approving…" : "Approve" }}
          </button>
        </footer>
    </div>
  </div>
</template>

<style scoped>
/* ── Backdrop ── */
.approval-backdrop {
  position: fixed;
  inset: 0;
  background: var(--overlay);
  display: flex;
  align-items: center;
  justify-content: center;
  z-index: 100;
  /* Backdrop is inert — no click handler intentionally; mis-click must not dismiss. */
}

/* ── Dialog ── */
.approval-modal {
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: var(--space-4) var(--space-5);
  min-width: 360px;
  max-width: 560px;
  width: 100%;
  max-height: min(80vh, 640px);
  display: flex;
  flex-direction: column;
  gap: var(--space-4);
  /* Prevent the whole dialog from scrolling — only the args section scrolls. */
  overflow: hidden;
}

/* ── Header ── */
.approval-header {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-weight: 500;
  color: var(--text);
  flex-shrink: 0;
}

.approval-tool-id {
  flex: 1;
  font-family: var(--font-mono);
  font-size: 0.875rem;
}

/* ── Risk pill ── */
.approval-pill {
  font-size: 0.6875rem;
  font-weight: 600;
  letter-spacing: 0.05em;
  padding: var(--space-1) var(--space-2);
  border-radius: 999px;
  flex-shrink: 0;
}

.pill-human {
  background: var(--fill-muted);
  color: var(--text-muted);
  border: 1px solid var(--border);
}

.pill-stepup {
  background: var(--fill-accent);
  color: var(--accent);
  border: 1px solid var(--accent);
}

.approval-queue-count {
  font-size: 0.75rem;
  color: var(--text-faint);
  flex-shrink: 0;
}

/* ── Dividers ── */
.approval-divider {
  border: none;
  border-top: 1px solid var(--border);
  margin: 0;
  flex-shrink: 0;
}

/* ── Meta row (agent — defensive, v-if guarded) ── */
.approval-meta {
  flex-shrink: 0;
}

.meta-row {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: 0.875rem;
  color: var(--text-muted);
}

.meta-label {
  font-weight: 500;
  color: var(--text-muted);
}

.meta-value {
  color: var(--text);
}

/* ── Reason ── */
.approval-reason {
  margin: 0;
  font-size: 0.875rem;
  color: var(--text-muted);
  flex-shrink: 0;
}

/* ── Args section: ONLY this scrolls ── */
.approval-args-section {
  flex: 1 1 0;
  min-height: 0;
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}

.section-label {
  margin: 0;
  font-size: 0.75rem;
  font-weight: 600;
  letter-spacing: 0.05em;
  text-transform: uppercase;
  color: var(--text-faint);
}

/* ── Risk gate checkbox ── */
.review-gate {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-size: 0.8125rem;
  color: var(--text-muted);
  cursor: pointer;
  flex-shrink: 0;
}

.review-gate .toggle {
  flex-shrink: 0;
  flex-shrink: 0;
}

/* ── Inline error ── */
.approval-error {
  margin: 0;
  font-size: 0.8125rem;
  color: var(--danger);
  flex-shrink: 0;
}

/* ── Footer ── */
.approval-actions {
  display: flex;
  gap: var(--space-2);
  justify-content: flex-end;
  padding-top: var(--space-4);
  border-top: 1px solid var(--border);
  flex-shrink: 0;
}

/* ── Shared button base ── */
.btn-approve,
.btn-deny {
  display: inline-flex;
  align-items: center;
  gap: var(--space-2);
  padding: var(--space-2) var(--space-4);
  border-radius: var(--radius-sm);
  font-family: var(--font-sans);
  font-size: 0.875rem;
  font-weight: 500;
  cursor: pointer;
  transition: background 0.15s, filter 0.15s, opacity 0.15s;
  line-height: 1;
}

.btn-approve:disabled,
.btn-deny:disabled {
  opacity: 0.5;
  cursor: not-allowed;
  filter: none;
}

/* Approve: solid fill — the deliberate, irreversible act */
.btn-approve {
  background: var(--ok);
  color: var(--text-on-accent);
  border: 1px solid transparent;
}

.btn-approve:not(:disabled):hover {
  filter: brightness(1.1);
}

/* Deny: ghost — the safe, low-friction escape */
.btn-deny {
  background: transparent;
  color: var(--text);
  border: 1px solid var(--border);
}

.btn-deny:not(:disabled):hover {
  background: var(--bg-hover);
}

/* ── Spinner animation ── */
.spin {
  animation: spin 0.8s linear infinite;
}

@keyframes spin {
  to { transform: rotate(360deg); }
}
</style>
