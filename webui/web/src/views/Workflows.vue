<script setup>
import { ref, computed, onMounted } from "vue";
import { useRouter } from "vue-router";
import Icon from "../components/Icon.vue";
import ErrorBanner from "../components/ui/ErrorBanner.vue";
import RunWorkflowModal from "../components/RunWorkflowModal.vue";
import PublishWorkflowDialog from "../components/PublishWorkflowDialog.vue";
import VersionSwitcherModal from "../components/VersionSwitcherModal.vue";
import ForkWorkflowModal from "../components/ForkWorkflowModal.vue";
import { listWorkflows, getWorkflow, deleteWorkflow } from "../api/workflows.js";
import { usePromptStore } from "../store/prompt.js";
import { useAgentsStore } from "../store/agents.js";

const router = useRouter();
const promptStore = usePromptStore();
const agentsStore = useAgentsStore();

// Bound-agent display name: resolved from the agents store, else the bare
// UUID's first 8 chars.
const agentNameById = computed(() => {
  const m = {};
  for (const a of agentsStore.assigned) m[a.id] = a.name;
  return m;
});
function agentLabel(id) {
  return agentNameById.value[id] || String(id).slice(0, 8);
}

const workflows = ref([]);
const error = ref("");
const loading = ref(false);
const nextCursor = ref("");
const sharedUnavailable = ref(false);

// One page per request; post-mutation refreshes reset to page 1 (accumulated
// pages collapse back — acceptable and simple).
const PAGE_SIZE = 50;

// ── Run modal ──────────────────────────────────────────────────────────────────
const runModalVisible = ref(false);
const runModalWorkflow = ref(null);

async function openRunModal(wf) {
  try {
    const data = await getWorkflow(wf.lineageId);
    const definition = data.definitionJson ? JSON.parse(data.definitionJson) : {};
    // Whole row + whole fetch response (data.version wins, matching the prior
    // `data.version ?? wf.version` precedence) + the definition parsed from
    // definitionJson — not a hand-picked field subset (workspace/file-explorer
    // archetype: spread, don't reconstruct).
    runModalWorkflow.value = { ...wf, ...data, definition };
    runModalVisible.value = true;
  } catch (e) {
    error.value = e.message || "Failed to load workflow";
  }
}

function closeRunModal() {
  runModalVisible.value = false;
  runModalWorkflow.value = null;
}

// ── Improve in chat ────────────────────────────────────────────────────────────
// Seeds the prompt store prefill (draft mode — user reviews before sending) with
// the workflow identity + current definition summary + an instruction to use
// workflow_propose when the user agrees on a change, then navigates to /chat.
// WHY prefill not set/auto-submit: improvement is conversational; the user should
// review and adjust the framing before the agent acts.
async function openImproveInChat(wf) {
  try {
    const data = await getWorkflow(wf.lineageId);
    let defSummary = "(no definition)";
    if (data.definitionJson) {
      try {
        defSummary = JSON.stringify(JSON.parse(data.definitionJson), null, 2);
      } catch {
        defSummary = data.definitionJson;
      }
    }
    const message =
      `I want to improve the workflow "${wf.name}" (lineageId: ${wf.lineageId}).\n\n` +
      `Current definition:\n\`\`\`json\n${defSummary}\n\`\`\`\n\n` +
      `Once we agree on what to change, use \`workflow_propose\` to create a proposed version for my approval.`;
    promptStore.setPrefill(message);
    router.push("/chat");
  } catch (e) {
    error.value = e.message || "Failed to load workflow";
  }
}

// ── Publish dialog ─────────────────────────────────────────────────────────────
const publishVisible = ref(false);
const publishWorkflow = ref(null);

function openPublishDialog(wf) {
  publishWorkflow.value = wf;
  publishVisible.value = true;
}

function closePublishDialog() {
  publishVisible.value = false;
  publishWorkflow.value = null;
}

// ── Version switcher ───────────────────────────────────────────────────────────
const versionsModalVisible = ref(false);
const versionsModalWorkflow = ref(null);

function openVersionsModal(wf) {
  versionsModalWorkflow.value = wf;
  versionsModalVisible.value = true;
}

function closeVersionsModal() {
  versionsModalVisible.value = false;
  versionsModalWorkflow.value = null;
}

// ── Fork modal ─────────────────────────────────────────────────────────────────
const forkModalVisible = ref(false);
const forkModalWorkflow = ref(null);

function openForkModal(wf) {
  forkModalWorkflow.value = wf;
  forkModalVisible.value = true;
}

function closeForkModal() {
  forkModalVisible.value = false;
  forkModalWorkflow.value = null;
}

// ── Delete confirm ───────────────────────────────────────────────────────────
// deleteTarget holds the whole workflow row awaiting confirmation; the
// shared-warning condition is derived from visibilityKind at the point of use
// in the template below, not precomputed into a stored field.
const deleteTarget = ref(null);
const deletePending = ref(false);

function openDeleteConfirm(wf) {
  deleteTarget.value = wf;
}

function closeDeleteConfirm() {
  if (deletePending.value) return;
  deleteTarget.value = null;
}

async function confirmDelete() {
  if (!deleteTarget.value) return;
  deletePending.value = true;
  error.value = "";
  try {
    await deleteWorkflow(deleteTarget.value.lineageId);
    deleteTarget.value = null;
    await load();
  } catch (e) {
    error.value = e.message || "Failed to delete workflow";
  } finally {
    deletePending.value = false;
  }
}

// ── Data loading + refresh ─────────────────────────────────────────────────────
async function load() {
  error.value = "";
  loading.value = true;
  try {
    const data = await listWorkflows({ limit: PAGE_SIZE });
    workflows.value = data.workflows ?? [];
    nextCursor.value = data.nextCursor ?? "";
    sharedUnavailable.value = data.sharedUnavailable ?? false;
  } catch (e) {
    error.value = e.message || "Failed to load workflows";
  } finally {
    loading.value = false;
  }
}

async function loadMore() {
  if (!nextCursor.value || loading.value) return;
  loading.value = true;
  try {
    const data = await listWorkflows({ limit: PAGE_SIZE, cursor: nextCursor.value });
    workflows.value = [...workflows.value, ...(data.workflows ?? [])];
    nextCursor.value = data.nextCursor ?? "";
    if (data.sharedUnavailable != null) sharedUnavailable.value = data.sharedUnavailable;
  } catch (e) {
    error.value = e.message || "Failed to load more workflows";
  } finally {
    loading.value = false;
  }
}

// F63: single filter over both lists, case-insensitive plain substring against
// name only — WorkflowSummary carries no description field (no include/-exclude
// grammar — recorded decision). Filters the accumulated loaded pages only; it
// does not fetch further pages to widen the match set.
const nameFilter = ref("");

function matchesNameFilter(w) {
  const q = nameFilter.value.trim().toLowerCase();
  if (!q) return true;
  return (w.name ?? "").toLowerCase().includes(q);
}

// Shared: public (visibilityKind === "shared") and not owned by the viewer.
const sharedWorkflows = computed(() =>
  workflows.value.filter((w) => w.visibilityKind === "shared" && !w.isOwner && matchesNameFilter(w)),
);

// Private: owned by the viewer (includes private + any owned shared).
const privateWorkflows = computed(() =>
  workflows.value.filter((w) => w.isOwner && matchesNameFilter(w)),
);

onMounted(() => {
  load();
  agentsStore.refresh(); // best-effort; store swallows its own errors
});

// Test-only introspection point (Files.vue precedent) — deleteTarget has no
// separate modal component boundary to assert a received prop against, since
// its confirmation UI is inline in this file's own template.
defineExpose({ deleteTarget });
</script>

<template>
  <div class="view">
    <header class="view-header">
      <Icon name="play-circle" :size="20" />
      <h2 class="view-title">Workflows</h2>
    </header>

    <ErrorBanner v-if="error" :message="error" />

    <input
      v-if="workflows.length > 0"
      v-model="nameFilter"
      type="text"
      class="wf-filter-input"
      data-testid="workflow-filter-input"
      placeholder="Filter workflows by name…"
      aria-label="Filter workflows by name"
    />

    <div v-if="loading && workflows.length === 0" class="empty" data-testid="loading">
      Loading…
    </div>

    <!-- Persistent banner: shared workflows unreachable (authorization service
         down). Own workflows still render below. -->
    <ErrorBanner
      v-if="sharedUnavailable"
      message="Shared workflows are temporarily unavailable (authorization service unreachable)."
      data-testid="shared-unavailable"
    >
      <template #action>
        <button class="btn-sm" data-testid="shared-retry" @click="load">Retry</button>
      </template>
    </ErrorBanner>

    <!-- Shared section -->
    <section v-if="sharedWorkflows.length > 0" class="section">
      <h3 class="section-title">Shared</h3>
      <ul class="workflow-list">
        <li
          v-for="wf in sharedWorkflows"
          :key="wf.lineageId"
          class="workflow-card"
          :class="{ 'workflow-card--greyed': wf.accessState === 'greyed_out' }"
          data-testid="workflow-card"
        >
          <div class="card-body">
            <div class="card-name">{{ wf.name }}</div>
            <div class="card-meta">
              <span class="badge badge-muted">v{{ wf.version }}</span>
              <span class="badge badge-muted">{{ wf.visibilityKind }}</span>
              <span v-if="wf.boundAgentId" class="badge badge-muted agent-badge" data-testid="bound-agent">
                agent: {{ agentLabel(wf.boundAgentId) }}
              </span>
              <span v-if="wf.accessState === 'greyed_out'" class="missing-req" data-testid="missing-req">
                needs: {{ (wf.missingRequirements ?? []).join(", ") }}
              </span>
              <span v-if="wf.boundAgentId && wf.boundAgentOk === false" class="missing-req" data-testid="agent-denied">
                no access to this workflow's agent
              </span>
            </div>
          </div>
          <div class="card-actions">
            <button
              class="btn-sm btn-accent"
              :disabled="wf.accessState === 'greyed_out' || (!!wf.boundAgentId && wf.boundAgentOk === false)"
              @click="openRunModal(wf)"
            >Run</button>
            <button class="btn-sm" @click="openVersionsModal(wf)">Versions</button>
            <button class="btn-sm" @click="openForkModal(wf)">Fork</button>
          </div>
        </li>
      </ul>
    </section>

    <!-- Private section -->
    <section v-if="privateWorkflows.length > 0" class="section">
      <h3 class="section-title">Private</h3>
      <ul class="workflow-list">
        <li
          v-for="wf in privateWorkflows"
          :key="wf.lineageId"
          class="workflow-card"
          :class="{ 'workflow-card--greyed': wf.accessState === 'greyed_out' }"
          data-testid="workflow-card"
        >
          <div class="card-body">
            <div class="card-name">{{ wf.name }}</div>
            <div class="card-meta">
              <span class="badge badge-muted">v{{ wf.version }}</span>
              <span class="badge" :class="wf.visibilityKind === 'shared' ? 'badge-ok' : 'badge-muted'">
                {{ wf.visibilityKind }}
              </span>
              <span v-if="wf.boundAgentId" class="badge badge-muted agent-badge" data-testid="bound-agent">
                agent: {{ agentLabel(wf.boundAgentId) }}
              </span>
              <span v-if="wf.accessState === 'greyed_out'" class="missing-req" data-testid="missing-req">
                needs: {{ (wf.missingRequirements ?? []).join(", ") }}
              </span>
              <span v-if="wf.boundAgentId && wf.boundAgentOk === false" class="missing-req" data-testid="agent-denied">
                no access to this workflow's agent
              </span>
            </div>
          </div>
          <div class="card-actions">
            <button
              class="btn-sm btn-accent"
              :disabled="wf.accessState === 'greyed_out' || (!!wf.boundAgentId && wf.boundAgentOk === false)"
              @click="openRunModal(wf)"
            >Run</button>
            <button v-if="wf.isOwner" class="btn-sm" @click="openImproveInChat(wf)">Improve</button>
            <button class="btn-sm" @click="openPublishDialog(wf)">Publish</button>
            <button class="btn-sm" @click="openVersionsModal(wf)">Versions</button>
            <button class="btn-sm" @click="openForkModal(wf)">Fork</button>
            <button class="btn-sm btn-danger" data-testid="btn-delete" @click="openDeleteConfirm(wf)">Delete</button>
          </div>
        </li>
      </ul>
    </section>

    <div
      v-if="workflows.length === 0 && !error && !loading"
      class="empty"
      data-testid="empty-state"
    >
      No workflows yet. Ask the agent to run a task and save it as a workflow.
    </div>

    <button
      v-if="nextCursor"
      class="btn-sm load-more"
      data-testid="load-more"
      :disabled="loading"
      @click="loadMore"
    >{{ loading ? "Loading…" : "Load more" }}</button>

    <div
      v-if="workflows.length > 0 && sharedWorkflows.length === 0 && privateWorkflows.length === 0 && !error"
      class="empty"
      data-testid="no-matches"
    >
      No workflows match "{{ nameFilter }}".
    </div>

    <RunWorkflowModal
      :workflow="runModalWorkflow"
      :visible="runModalVisible"
      @close="closeRunModal"
    />

    <PublishWorkflowDialog
      :workflow="publishWorkflow"
      :visible="publishVisible"
      @close="closePublishDialog"
      @published="load"
    />

    <VersionSwitcherModal
      :workflow="versionsModalWorkflow"
      :visible="versionsModalVisible"
      @close="closeVersionsModal"
      @changed="load"
    />

    <ForkWorkflowModal
      :workflow="forkModalWorkflow"
      :visible="forkModalVisible"
      @close="closeForkModal"
      @forked="load"
    />

    <!-- Delete confirmation -->
    <div v-if="deleteTarget" class="del-backdrop" data-testid="delete-confirm">
      <div class="del-modal" role="dialog" aria-modal="true" aria-labelledby="del-title">
        <header class="del-header">
          <Icon name="trash" :size="16" />
          <span id="del-title" class="del-title">Delete workflow</span>
        </header>
        <p class="del-body">
          Delete <strong>{{ deleteTarget.name }}</strong>? This removes every version and cannot be undone.
        </p>
        <p v-if="deleteTarget.visibilityKind === 'shared'" class="del-warn" data-testid="delete-shared-warn">
          This workflow is shared — deleting it removes it for everyone in its groups.
        </p>
        <footer class="del-footer">
          <button class="btn-cancel" :disabled="deletePending" @click="closeDeleteConfirm">Cancel</button>
          <button
            class="btn-danger-solid"
            :disabled="deletePending"
            data-testid="btn-delete-confirm"
            @click="confirmDelete"
          >{{ deletePending ? "Deleting…" : "Delete" }}</button>
        </footer>
      </div>
    </div>
  </div>
</template>

<style scoped>
.view {
  padding: 2rem;
  max-width: 800px;
  margin: 0 auto;
  display: flex;
  flex-direction: column;
  gap: 1.5rem;
}

.view-header {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  color: var(--text);
}

.view-title {
  font-family: var(--font-sans);
  font-size: 1.25rem;
  font-weight: 500;
  margin: 0;
}

.wf-filter-input {
  padding: 0.4375rem 0.75rem;
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--text);
  font-family: var(--font-sans);
  font-size: 0.875rem;
}

.section {
  display: flex;
  flex-direction: column;
  gap: 0.75rem;
}

.section-title {
  font-size: 0.8125rem;
  font-weight: 500;
  color: var(--text-muted);
  text-transform: uppercase;
  letter-spacing: 0.05em;
  margin: 0;
}

.workflow-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
}

.workflow-card {
  display: flex;
  align-items: flex-start;
  gap: 1rem;
  padding: 0.875rem 1rem;
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
}

/* greyed-out: visually muted, Run button disabled */
.workflow-card--greyed {
  opacity: 0.55;
}

.card-body {
  flex: 1;
  display: flex;
  flex-direction: column;
  gap: 0.25rem;
}

.card-name {
  font-size: 0.9375rem;
  color: var(--text);
}

.card-meta {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  flex-wrap: wrap;
}

.missing-req {
  font-size: 0.75rem;
  color: var(--text-muted);
  font-style: italic;
}

.badge {
  font-size: 0.6875rem;
  padding: 0.125rem 0.5rem;
  border-radius: 999px;
  font-weight: 500;
}

.badge-ok {
  background: var(--fill-accent);
  color: var(--ok);
}

.badge-muted {
  background: var(--fill-muted);
  color: var(--text-muted);
}

.card-actions {
  display: flex;
  align-items: center;
  gap: 0.375rem;
  flex-shrink: 0;
  flex-wrap: wrap;
}

.btn-sm {
  padding: 0.25rem 0.625rem;
  font-size: 0.8125rem;
  font-family: var(--font-sans);
  background: var(--bg);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--text-muted);
  cursor: pointer;
  transition: background 0.15s, color 0.15s;
}

.btn-sm:not(:disabled):hover {
  background: var(--fill-muted);
  color: var(--text);
}

.btn-sm:disabled {
  cursor: not-allowed;
  opacity: 0.5;
}

.btn-sm.btn-accent {
  background: var(--accent);
  border-color: transparent;
  color: var(--text-on-accent);
}

.btn-sm.btn-accent:not(:disabled):hover {
  background: var(--accent-hover);
}

.btn-sm.btn-danger {
  color: var(--danger);
}

.btn-sm.btn-danger:not(:disabled):hover {
  background: var(--fill-danger);
  color: var(--danger);
  border-color: var(--danger);
}

.empty {
  color: var(--text-faint);
  font-size: 0.875rem;
}

.load-more {
  align-self: center;
}

/* ── Delete confirmation modal ──────────────────────────────────────────────── */
.del-backdrop {
  position: fixed;
  inset: 0;
  background: var(--overlay);
  display: flex;
  align-items: center;
  justify-content: center;
  z-index: 100;
}

.del-modal {
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: var(--space-4) var(--space-5);
  min-width: 320px;
  max-width: 420px;
  width: 100%;
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
}

.del-header {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  font-weight: 500;
  color: var(--text);
}

.del-title {
  font-size: 0.9375rem;
}

.del-body {
  margin: 0;
  font-size: 0.875rem;
  color: var(--text);
}

.del-warn {
  margin: 0;
  font-size: 0.8125rem;
  color: var(--danger);
}

.del-footer {
  display: flex;
  gap: var(--space-2);
  justify-content: flex-end;
  margin-top: var(--space-2);
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

.btn-danger-solid {
  padding: var(--space-2) var(--space-4);
  border-radius: var(--radius-sm);
  font-family: var(--font-sans);
  font-size: 0.875rem;
  font-weight: 500;
  cursor: pointer;
  background: var(--danger);
  color: var(--text-on-accent);
  border: 1px solid transparent;
}

.btn-danger-solid:not(:disabled):hover {
  filter: brightness(1.1);
}

.btn-danger-solid:disabled,
.btn-cancel:disabled {
  cursor: not-allowed;
  opacity: 0.5;
}
</style>
