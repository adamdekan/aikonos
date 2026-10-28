<script setup>
// Pending approvals held by the broker (AG-07): every approval the signed-in
// user may decide, including the second vote of a 2-of-n gate requested in
// someone else's session. Approve/Deny call the broker's ApproveTask through
// the gateway. A step-up approval refused for want of a recent MFA sign-in
// (AG-13) offers a fresh sign-in, then retries the approval.
import { ref, onMounted, nextTick } from "vue";
import ArgsView from "../../components/ArgsView.vue";
import { listPendingApprovals, decideApproval, isStepUpRequired } from "../../api/approvals.js";
import { stepUpSignin } from "../../auth/oidc.js";

// AG-07: the deep link in an approval notification (/inbox?approval=<taskId>)
// marks and scrolls to that request.
const props = defineProps({ focusTask: { type: String, default: "" } });

const approvals = ref([]);
const error = ref("");
const busy = ref("");
const stepUp = ref(null); // { taskId, acr } while a fresh MFA sign-in is due

async function load() {
  try {
    approvals.value = (await listPendingApprovals()).approvals ?? [];
    if (props.focusTask) {
      await nextTick();
      document.querySelector(`[data-testid="approval-${CSS.escape(props.focusTask)}"]`)?.scrollIntoView?.({ block: "center" });
    }
  } catch (e) {
    error.value = e.message || "Failed to load approvals";
  }
}

async function decide(a, approved) {
  busy.value = a.taskId;
  error.value = "";
  stepUp.value = null;
  try {
    await decideApproval(a.taskId, approved, a.argsDigest);
    approvals.value = approvals.value.filter((x) => x.taskId !== a.taskId);
  } catch (e) {
    if (isStepUpRequired(e)) {
      stepUp.value = { taskId: a.taskId, acr: e.data.acr ?? "" };
      error.value = "This approval needs a recent multi-factor sign-in.";
    } else {
      error.value = e.message || "Decision failed";
    }
  } finally {
    busy.value = "";
  }
}

async function verifyAndApprove(a) {
  try {
    await stepUpSignin(stepUp.value?.acr);
  } catch (e) {
    error.value = e.message || "Sign-in failed";
    return;
  }
  await decide(a, true);
}

onMounted(load);
</script>

<template>
  <section v-if="approvals.length || error" class="section" data-testid="pending-approvals">
    <h3 class="section-title">Pending approvals</h3>
    <p v-if="error" class="pa-error" role="alert">{{ error }}</p>
    <ul class="pa-list">
      <li
        v-for="a in approvals"
        :key="a.taskId"
        class="pa-row"
        :class="{ 'pa-focus': a.taskId === focusTask }"
        :data-testid="`approval-${a.taskId}`"
        :aria-current="a.taskId === focusTask ? 'true' : undefined"
      >
        <div class="pa-meta">
          <span>{{ a.requesterId }}</span>
          <span v-if="a.stepUp" class="pa-pill">STEP-UP</span>
          <span>{{ a.approvals }} of {{ a.requiresN }} approvals</span>
        </div>
        <div v-for="s in a.steps" :key="s.seq" class="pa-step">
          <code>{{ s.toolId }}</code>
          <ArgsView :args="s.args ?? null" />
        </div>
        <div class="pa-actions">
          <button
            v-if="stepUp?.taskId === a.taskId"
            class="pa-btn pa-approve"
            :disabled="busy === a.taskId"
            :data-testid="`approval-step-up-${a.taskId}`"
            @click="verifyAndApprove(a)"
          >Verify sign-in and approve</button>
          <button class="pa-btn" :disabled="busy === a.taskId" data-action="deny" @click="decide(a, false)">Deny</button>
          <button class="pa-btn pa-approve" :disabled="busy === a.taskId" data-action="approve" @click="decide(a, true)">Approve</button>
        </div>
      </li>
    </ul>
  </section>
</template>

<style scoped>
.section { display: flex; flex-direction: column; gap: var(--space-4); }
.section-title {
  font-size: 0.8125rem; font-weight: 500; color: var(--text-muted);
  text-transform: uppercase; letter-spacing: 0.05em; margin: 0;
}
.pa-list { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: var(--space-3); }
.pa-row {
  display: flex; flex-direction: column; gap: var(--space-3);
  padding: var(--space-3) var(--space-4);
  background: var(--bg-elevated); border: 1px solid var(--border); border-radius: var(--radius-sm);
}
.pa-focus { border-color: var(--accent-text); }
.pa-meta { display: flex; flex-wrap: wrap; gap: var(--space-3); font-size: 0.75rem; color: var(--text-muted); }
.pa-pill { color: var(--accent-text); font-weight: 600; }
.pa-step { display: flex; flex-direction: column; gap: var(--space-2); }
.pa-actions { display: flex; justify-content: flex-end; gap: var(--space-2); }
.pa-btn {
  padding: var(--space-1) var(--space-3); border-radius: var(--radius-sm);
  border: 1px solid var(--border); background: transparent; color: var(--text);
  font-family: var(--font-sans); cursor: pointer;
}
.pa-approve { background: var(--ok); border-color: transparent; color: var(--text-on-status); }
.pa-btn:disabled { opacity: 0.5; cursor: not-allowed; }
.pa-error { margin: 0; font-size: 0.8125rem; color: var(--danger); }
</style>
