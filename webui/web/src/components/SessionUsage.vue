<script setup>
// Per-session LLM usage strip: model, tokens in/out, price. Rendered above the
// composer for every session kind — interactive chat, agent sessions, workflow
// runs and scheduled runs all attribute their usage events to a session id, so
// one read covers all four.
//
// Source of truth is the broker (llm_usage_events via GET /sessions/:id/usage),
// not the SSE stream: a workflow or scheduled run produces its usage with no
// browser attached, and a session reopened later must still show its totals.
import { ref, computed, watch, onUnmounted } from "vue";
import { getSessionUsage } from "../api/usage.js";
import { fmtAmountPrecise } from "../lib/money.js";

const props = defineProps({
  sessionId: { type: String, default: "" },
  // Used only as a refetch trigger: totals are re-read when a run finishes.
  running: { type: Boolean, default: false },
  // True once the session has produced any conversation. Separates a brand-new
  // empty session (show nothing) from one that ran and cost nothing (say so).
  hasContent: { type: Boolean, default: false },
});

const usage = ref(null);

// Usage lands in the DB via a fire-and-forget emit that races the run's own
// completion, so the fetch fired the instant a run ends can miss its last call.
// One delayed re-read closes that window.
// ponytail: fixed delay, not a poll-until-stable loop — worst case the strip is
// one turn behind until the next run ends or the session is reopened. Revisit
// only if the emit path ever becomes slow enough for 2s to miss.
const SETTLE_REFETCH_MS = 2000;
let settleTimer = null;

function clearSettleTimer() {
  if (settleTimer !== null) {
    clearTimeout(settleTimer);
    settleTimer = null;
  }
}

async function load() {
  const id = props.sessionId;
  if (!id) {
    usage.value = null;
    return;
  }
  try {
    const resp = await getSessionUsage(id);
    // Guard against a slow response for a session the user already navigated
    // away from overwriting the current one's totals.
    if (props.sessionId !== id) return;
    usage.value = resp;
  } catch {
    // Deliberately silent: this is a decoration on the chat view. A failed read
    // hides the strip rather than raising a banner over a working conversation.
    if (props.sessionId === id) usage.value = null;
  }
}

watch(() => props.sessionId, () => {
  clearSettleTimer();
  usage.value = null;
  load();
}, { immediate: true });

watch(() => props.running, (isRunning, wasRunning) => {
  if (wasRunning && !isRunning) {
    load();
    clearSettleTimer();
    settleTimer = setTimeout(() => { settleTimer = null; load(); }, SETTLE_REFETCH_MS);
  }
});

onUnmounted(clearSettleTimer);

const calls = computed(() => usage.value?.calls ?? 0);

// A session that ran but called no model is genuinely free — a tool-only
// workflow is the common case, since only kind:"reason" steps invoke an LLM.
// Hiding the strip there is indistinguishable from a broken read, so say it.
const billedNothing = computed(() => usage.value !== null && calls.value === 0);

// An empty session still renders nothing: there is no run to report on yet.
const show = computed(() => calls.value > 0 || (billedNothing.value && props.hasContent));

// Rows arrive most-expensive-first, so the first model is the one worth naming.
// A session that spanned several (provider fallback, a vision or reason call)
// gets a "+N" suffix instead of an unbounded list.
const modelLabel = computed(() => {
  const models = usage.value?.models ?? [];
  const unique = [...new Set(models)];
  if (unique.length === 0) return "—";
  if (unique.length === 1) return unique[0];
  return `${unique[0]} +${unique.length - 1}`;
});

const modelTitle = computed(() => [...new Set(usage.value?.models ?? [])].join(", "));

function fmtTokens(n) {
  return Number(n ?? 0).toLocaleString();
}
</script>

<template>
  <div v-if="show" class="session-usage" data-testid="session-usage">
    <span v-if="billedNothing" class="su-item" data-testid="session-usage-free">
      <span class="su-label">No model calls — no LLM cost</span>
    </span>
    <template v-else>
    <span class="su-item" :title="modelTitle">
      <span class="su-label">Model</span>
      <span class="su-value">{{ modelLabel }}</span>
    </span>
    <span class="su-item">
      <span class="su-label">Tokens In</span>
      <span class="su-value">{{ fmtTokens(usage.tokensIn) }}</span>
    </span>
    <span class="su-item">
      <span class="su-label">Tokens Out</span>
      <span class="su-value">{{ fmtTokens(usage.tokensOut) }}</span>
    </span>
    <span class="su-item">
      <span class="su-label">Price</span>
      <span class="su-value">{{ fmtAmountPrecise(usage.costMicros) }}</span>
    </span>
    </template>
  </div>
</template>

<style scoped>
.session-usage {
  display: flex;
  flex-wrap: wrap;
  gap: 4px 14px;
  padding: 0 2px 6px;
  font-size: 11px;
  line-height: 1.4;
  color: var(--text-muted);
}

.su-item {
  display: inline-flex;
  gap: 5px;
  white-space: nowrap;
}

.su-label { opacity: 0.75; }

.su-value {
  color: var(--text);
  font-variant-numeric: tabular-nums;
}
</style>
