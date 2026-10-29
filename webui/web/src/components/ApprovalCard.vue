<script setup>
// Inline approval card in the chat transcript (AG-07): the one place a chat
// decides a gated call of its run. It comes from the live stream or, after a
// reload or on reopening the session, from the broker's pending list. Approve
// and Deny go to the broker with the args digest shown (slice 2: a live run
// wakes on its broker poll). A step-up gate never asks its requester for a
// vote (n >= 2, separation of duty), so its MFA re-sign-in (AG-13) lives in the
// approvers' Inbox, not here.
import { ref, computed, onMounted, onBeforeUnmount } from "vue";
import { get, post } from "../api/client.js";
import { decideApproval, withdrawApproval } from "../api/approvals.js";
import ArgsView from "./ArgsView.vue";
import Icon from "./Icon.vue";
import ToggleSwitch from "./ui/ToggleSwitch.vue";

const AWAITING_APPROVAL = 4; // broker TaskStatus

const props = defineProps({
  // ApprovalInfo (stream / gateway /approvals) or one built from a broker
  // pending approval: toolCallId, taskId, toolId, args, argsDigest, stepUp,
  // requiresN, approvals, ownerMayApprove, reason.
  request: { type: Object, required: true },
});
const emit = defineEmits(["done"]);

const root = ref(null);
const busy = ref("");          // "approve" | "deny" while a decision is in flight
const error = ref("");
const reviewed = ref(false);
const voted = ref(false);      // our vote is in, more approvers are due
const gone = ref("");          // the run that asked no longer exists
const approvals = ref(null);   // live vote count from the run's /approvals entry

const id = computed(() => props.request.toolCallId);
const highRisk = computed(() => !!(props.request.stepUp || props.request.risk === "high"));
// Under separation of duty (n >= 2) the run's own user may not vote: the card
// waits for the others, who decide from their Inbox, and can only withdraw
// (the broker then closes the request and cancels its task).
const waitingFor = computed(() =>
  props.request.ownerMayApprove === false || voted.value
    ? Math.max(1, (props.request.requiresN ?? 1) - (approvals.value ?? props.request.approvals ?? 0) - (voted.value ? 1 : 0))
    : 0
);

// While waiting on others, re-read the run's approval (the gateway keeps its
// vote count current from the broker) so the count drops as they vote. The
// card goes away when the request resolves, which stops the poll.
const POLL_MS = 5000;
let poll;
async function refreshCount() {
  if (!waitingFor.value || gone.value || busy.value) return;
  try {
    const a = ((await get("/approvals")).approvals ?? []).find((x) => x.toolCallId === id.value);
    if (a && typeof a.approvals === "number") approvals.value = a.approvals;
  } catch {
    // Best effort: the next tick retries.
  }
}

onMounted(() => {
  root.value?.scrollIntoView?.({ block: "nearest" });
  poll = setInterval(refreshCount, POLL_MS);
});
onBeforeUnmount(() => clearInterval(poll));

// The gateway holds an approval only while its run waits on it; a reload or a
// gateway restart ends the run, the broker keeps the request.
async function runIsLive() {
  try {
    return ((await get("/approvals")).approvals ?? []).some((a) => a.toolCallId === id.value);
  } catch {
    return true; // unknown: do not claim the run is gone
  }
}

async function respond(approved) {
  if (busy.value) return;
  busy.value = approved ? "approve" : "deny";
  error.value = "";
  const { taskId, argsDigest = "" } = props.request;
  try {
    if (waitingFor.value && taskId) {
      // Withdraw: the requester has no vote, so the broker closes the request
      // and cancels its task; then the run's own wait is ended (it is gone
      // after a reload, which is fine: nothing is left waiting).
      await withdrawApproval(taskId);
      await post(`/approve/${encodeURIComponent(id.value)}`, { body: { approved: false, argsDigest } }).catch(() => {});
      emit("done", id.value);
      return;
    }
    if (!taskId) {
      // A request that names no broker task: only its run's wait can end it.
      const r = await post(`/approve/${encodeURIComponent(id.value)}`, { body: { approved, argsDigest } });
      if (r?.resolved) emit("done", id.value);
      else gone.value = "The run that asked for this is no longer running (the page was reloaded or the gateway restarted). Nothing was decided.";
      return;
    }
    const live = await runIsLive();
    const r = await decideApproval(taskId, approved, argsDigest);
    if (!live) {
      gone.value = `${approved ? "Approved" : "Denied"}, but the run that asked for it is no longer running (the page was reloaded or the gateway restarted), so it will not continue. Send your request again.`;
    } else if (approved && r?.newStatus === AWAITING_APPROVAL) {
      voted.value = true;
    } else {
      emit("done", id.value);
    }
  } catch (e) {
    error.value = e?.message || "Decision failed. Please try again.";
  } finally {
    busy.value = "";
  }
}
</script>

<template>
  <section
    ref="root"
    class="ac"
    role="group"
    :aria-label="`Approval for ${request.toolId}`"
    :data-testid="`chat-approval-${id}`"
  >
    <header class="ac-head">
      <Icon name="tool" :size="14" />
      <code class="ac-tool">{{ request.toolId }}</code>
      <span class="ac-pill" :class="{ 'ac-pill--stepup': request.stepUp }">
        {{ request.stepUp ? "STEP-UP" : "APPROVAL" }}
      </span>
    </header>

    <p v-if="request.reason" class="ac-reason">{{ request.reason }}</p>

    <details class="ac-args" :open="highRisk">
      <summary>Arguments</summary>
      <ArgsView :args="request.args ?? null" />
    </details>

    <p v-if="gone" class="ac-gone" role="status" :data-testid="`chat-approval-gone-${id}`">{{ gone }}</p>
    <template v-else>
      <p v-if="waitingFor" class="ac-waiting" role="status" :data-testid="`chat-approval-waiting-${id}`">
        Waiting for {{ waitingFor }} more approver{{ waitingFor === 1 ? "" : "s" }}.
        <template v-if="!voted">You cannot approve your own request; they decide from their Inbox.</template>
      </p>

      <label v-if="highRisk && !waitingFor" class="ac-review">
        <ToggleSwitch
          v-model="reviewed"
          aria-label="I have reviewed the arguments"
          :data-testid="`chat-approval-reviewed-${id}`"
        />
        <span>I have reviewed the arguments</span>
      </label>

      <p v-if="error" class="ac-error" role="alert">{{ error }}</p>

      <div class="ac-actions">
        <button
          class="ac-btn"
          :disabled="!!busy"
          :aria-busy="busy === 'deny'"
          :data-testid="`chat-deny-${id}`"
          @click="respond(false)"
        >{{ busy === "deny" ? (waitingFor ? "Withdrawing…" : "Denying…") : waitingFor ? "Withdraw" : "Deny" }}</button>
        <button
          v-if="!waitingFor"
          class="ac-btn ac-approve"
          :disabled="!!busy || (highRisk && !reviewed)"
          :aria-busy="busy === 'approve'"
          :data-testid="`chat-approve-${id}`"
          @click="respond(true)"
        >{{ busy === "approve" ? "Approving…" : "Approve" }}</button>
      </div>
    </template>
  </section>
</template>

<style scoped>
.ac {
  display: flex; flex-direction: column; gap: var(--space-3);
  padding: var(--space-3) var(--space-4);
  background: var(--bg-elevated); border: 1px solid var(--border); border-radius: var(--radius-sm);
}
.ac-head { display: flex; align-items: center; gap: var(--space-2); color: var(--text); }
.ac-tool { flex: 1; font-family: var(--font-mono); font-size: 0.875rem; }
.ac-pill {
  font-size: 0.6875rem; font-weight: 600; letter-spacing: 0.05em;
  padding: var(--space-1) var(--space-2); border-radius: 999px;
  background: var(--fill-muted); color: var(--text-muted); border: 1px solid var(--border);
}
.ac-pill--stepup { background: var(--fill-accent); color: var(--accent-text); border-color: var(--accent-text); }
.ac-reason, .ac-waiting, .ac-gone, .ac-error { margin: 0; font-size: 0.875rem; }
.ac-reason { color: var(--text-muted); }
.ac-waiting, .ac-gone { color: var(--text); }
.ac-error { color: var(--danger); }
.ac-args summary { cursor: pointer; font-size: 0.75rem; color: var(--text-muted); }
.ac-args[open] summary { margin-bottom: var(--space-2); }
.ac-review { display: flex; align-items: center; gap: var(--space-2); font-size: 0.8125rem; color: var(--text-muted); }
.ac-actions { display: flex; justify-content: flex-end; gap: var(--space-2); }
.ac-btn {
  padding: var(--space-1) var(--space-3); border-radius: var(--radius-sm);
  border: 1px solid var(--border); background: transparent; color: var(--text);
  font-family: var(--font-sans); font-size: 0.875rem; cursor: pointer;
}
.ac-approve { background: var(--ok); border-color: transparent; color: var(--text-on-status); }
.ac-btn:disabled { opacity: 0.5; cursor: not-allowed; }
</style>
