<script setup>
import { ref, onMounted } from "vue";
import Icon from "../components/Icon.vue";
import RecurrenceSelector from "../components/RecurrenceSelector.vue";
import { listSchedules, createSchedule, updateSchedule, pauseSchedule, resumeSchedule, deleteSchedule } from "../api/schedules.js";
import { describeCron } from "../lib/cron.js";

const schedules = ref([]);
const error = ref("");

// New-schedule form state
const newPrompt   = ref("");
const newKind     = ref("CRON");
const newCron     = ref("0 9 * * *"); // daily 09:00 — valid default if submitted untouched
const newRunAt    = ref("");

// Inline-edit state (one row at a time)
const editingId   = ref(null);
const editPrompt  = ref("");
const editKind    = ref("CRON");
const editCron    = ref("0 9 * * *");
const editRunAt   = ref("");
let   editTools   = [];

// ISO → the local "YYYY-MM-DDTHH:mm" that <input type="datetime-local"> wants.
function isoToLocalInput(iso) {
  if (!iso) return "";
  const d = new Date(iso);
  const pad = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

function startEdit(s) {
  editingId.value  = s.id;
  editPrompt.value = s.prompt ?? "";
  editKind.value   = s.kind === "ONCE" ? "ONCE" : "CRON";
  editCron.value   = s.cronExpr || "0 9 * * *";
  editRunAt.value  = s.kind === "ONCE" ? isoToLocalInput(s.nextFireAt) : "";
  editTools        = s.approvedTools ?? []; // resend verbatim so the broker doesn't clear them
}

function cancelEdit() {
  editingId.value = null;
}

// Workflow-mode rows carry a non-empty lineage id; prompt/approvedTools are
// always empty on them (the workflow binding + inputs are immutable after
// create — ).
function isWorkflowRow(s) {
  return !!(s.workflowLineageId && s.workflowLineageId.trim() !== "");
}

async function saveEdit(id) {
  error.value = "";
  try {
    const result = await updateSchedule(id, {
      prompt:        editPrompt.value.trim(),
      kind:          editKind.value,
      cronExpr:      editKind.value === "CRON" ? editCron.value.trim() : "",
      runAt:         editKind.value === "ONCE" ? (editRunAt.value || undefined) : undefined,
      approvedTools: editTools,
    });
    if (result.schedule) updateRow(result.schedule);
    editingId.value = null;
  } catch (e) {
    error.value = e.message || "Failed to update schedule";
  }
}

async function load() {
  error.value = "";
  try {
    const data = await listSchedules();
    schedules.value = data.schedules ?? [];
  } catch (e) {
    error.value = e.message || "Failed to load schedules";
  }
}

async function submit() {
  error.value = "";
  try {
    const result = await createSchedule({
      prompt:        newPrompt.value.trim(),
      kind:          newKind.value,
      cronExpr:      newKind.value === "CRON" ? newCron.value.trim() : "",
      runAt:         newKind.value === "ONCE" ? (newRunAt.value || undefined) : undefined,
      approvedTools: ["web.fetch", "doc.read", "doc.write"],
    });
    if (result.schedule) {
      schedules.value.push(result.schedule);
    }
    newPrompt.value = "";
    newCron.value   = "0 9 * * *";
    newRunAt.value  = "";
  } catch (e) {
    error.value = e.message || "Failed to create schedule";
  }
}

async function pause(id) {
  error.value = "";
  try {
    const result = await pauseSchedule(id);
    if (result.schedule) updateRow(result.schedule);
  } catch (e) {
    error.value = e.message || "Failed to pause";
  }
}

async function resume(id) {
  error.value = "";
  try {
    const result = await resumeSchedule(id);
    if (result.schedule) updateRow(result.schedule);
  } catch (e) {
    error.value = e.message || "Failed to resume";
  }
}

async function remove(id) {
  error.value = "";
  try {
    await deleteSchedule(id);
    schedules.value = schedules.value.filter(s => s.id !== id);
  } catch (e) {
    error.value = e.message || "Failed to delete";
  }
}

function updateRow(updated) {
  const idx = schedules.value.findIndex(s => s.id === updated.id);
  if (idx !== -1) schedules.value[idx] = updated;
}

onMounted(load);
</script>

<template>
  <div class="view">
    <header class="view-header">
      <Icon name="schedules" :size="20" />
      <h2 class="view-title">Schedules</h2>
    </header>

    <div v-if="error" class="error-banner" data-testid="error-banner">
      <Icon name="close" :size="14" />
      {{ error }}
    </div>

    <!-- Create form -->
    <form class="create-form" data-testid="create-schedule-form" @submit.prevent="submit">
      <h3 class="section-title">New schedule</h3>
      <div class="form-row">
        <textarea
          v-model="newPrompt"
          class="field field-prompt"
          placeholder="Agent prompt"
          rows="2"
          data-testid="new-prompt"
          required
        />
      </div>
      <div class="form-row form-inline">
        <select v-model="newKind" class="field" data-testid="new-kind">
          <option value="CRON">Recurring (cron)</option>
          <option value="ONCE">One-off</option>
        </select>
        <RecurrenceSelector v-if="newKind === 'CRON'" v-model="newCron" />
        <input
          v-else
          v-model="newRunAt"
          class="field"
          type="datetime-local"
          data-testid="new-run-at"
        />
      </div>
      <div class="form-actions">
        <button type="submit" class="btn-accent">
          <Icon name="plus" :size="15" />
          Create
        </button>
      </div>
    </form>

    <!-- Schedule list -->
    <div v-if="schedules.length === 0 && !error" class="empty">No schedules yet.</div>
    <ul class="schedule-list">
      <li
        v-for="s in schedules"
        :key="s.id"
        class="schedule-row"
        data-testid="schedule-row"
      >
        <!-- Inline edit form (same controls as the create form) -->
        <div v-if="editingId === s.id" class="schedule-main schedule-edit" :data-testid="`edit-form-${s.id}`">
          <textarea
            v-if="!isWorkflowRow(s)"
            v-model="editPrompt"
            class="field field-prompt"
            rows="2"
            :data-testid="`edit-prompt-${s.id}`"
          />
          <div class="form-inline">
            <select v-model="editKind" class="field" :data-testid="`edit-kind-${s.id}`">
              <option value="CRON">Recurring (cron)</option>
              <option value="ONCE">One-off</option>
            </select>
            <RecurrenceSelector v-if="editKind === 'CRON'" v-model="editCron" />
            <input
              v-else
              v-model="editRunAt"
              class="field"
              type="datetime-local"
              :data-testid="`edit-run-at-${s.id}`"
            />
          </div>
        </div>
        <div v-else class="schedule-main">
          <span v-if="isWorkflowRow(s)" class="schedule-prompt schedule-workflow">
            <span class="badge badge-workflow" data-testid="workflow-badge">Workflow</span>
            {{ s.workflowDisplayName || "(deleted workflow)" }}
          </span>
          <span v-else class="schedule-prompt">{{ s.prompt }}</span>
          <span class="schedule-meta">
            <span class="badge" :class="s.state === 'ACTIVE' ? 'badge-ok' : 'badge-muted'">{{ s.state }}</span>
            <span class="badge badge-muted">{{ s.kind }}</span>
            <span v-if="s.cronExpr" class="mono" :title="s.cronExpr">{{ describeCron(s.cronExpr) }}</span>
          </span>
          <span v-if="s.nextFireAt" class="schedule-next">
            Next: {{ new Date(s.nextFireAt).toLocaleString() }}
          </span>
        </div>
        <div v-if="editingId === s.id" class="schedule-actions">
          <button
            class="btn-icon"
            :data-testid="`save-${s.id}`"
            :aria-label="`Save ${s.id}`"
            @click="saveEdit(s.id)"
          >
            <Icon name="check" :size="15" />
          </button>
          <button
            class="btn-icon"
            :data-testid="`cancel-edit-${s.id}`"
            :aria-label="`Cancel editing ${s.id}`"
            @click="cancelEdit"
          >
            <Icon name="close" :size="15" />
          </button>
        </div>
        <div v-else class="schedule-actions">
          <button
            class="btn-icon"
            :data-testid="`edit-${s.id}`"
            :aria-label="`Edit ${s.id}`"
            @click="startEdit(s)"
          >
            <Icon name="edit" :size="15" />
          </button>
          <button
            v-if="s.state === 'ACTIVE'"
            class="btn-icon"
            :data-testid="`pause-${s.id}`"
            :aria-label="`Pause ${s.id}`"
            @click="pause(s.id)"
          >
            <Icon name="pause" :size="15" />
          </button>
          <button
            v-else-if="s.state === 'PAUSED'"
            class="btn-icon"
            :data-testid="`resume-${s.id}`"
            :aria-label="`Resume ${s.id}`"
            @click="resume(s.id)"
          >
            <Icon name="play" :size="15" />
          </button>
          <button
            class="btn-icon btn-icon-danger"
            :data-testid="`delete-${s.id}`"
            :aria-label="`Delete ${s.id}`"
            @click="remove(s.id)"
          >
            <Icon name="trash" :size="15" />
          </button>
        </div>
      </li>
    </ul>
  </div>
</template>

<style scoped>
.view {
  padding: 2rem;
  max-width: 720px;
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

.error-banner {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  padding: 0.75rem 1rem;
  background: var(--fill-danger);
  border: 1px solid var(--danger);
  border-radius: var(--radius-sm);
  color: var(--danger);
  font-size: 0.875rem;
}

.section-title {
  font-size: 0.8125rem;
  font-weight: 500;
  color: var(--text-muted);
  text-transform: uppercase;
  letter-spacing: 0.05em;
  margin: 0 0 0.5rem;
}

.create-form {
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: 1.25rem;
  display: flex;
  flex-direction: column;
  gap: 0.75rem;
}

.form-row {
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
}

.form-inline {
  flex-direction: row;
  align-items: flex-start;
  gap: 0.75rem;
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

.field-prompt {
  resize: vertical;
}

.form-actions {
  display: flex;
  justify-content: flex-end;
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

.empty {
  color: var(--text-faint);
  font-size: 0.875rem;
}

.schedule-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
}

.schedule-row {
  display: flex;
  align-items: flex-start;
  gap: 1rem;
  padding: 0.875rem 1rem;
  background: var(--bg-elevated);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
}

.schedule-main {
  flex: 1;
  display: flex;
  flex-direction: column;
  gap: 0.25rem;
  min-width: 0;
}

.schedule-edit {
  gap: 0.5rem;
}

.schedule-prompt {
  font-size: 0.9375rem;
  color: var(--text);
}

.schedule-meta {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  flex-wrap: wrap;
}

.schedule-next {
  font-size: 0.75rem;
  color: var(--text-muted);
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

.badge-workflow {
  background: var(--fill-accent);
  color: var(--accent);
}

.schedule-workflow {
  display: flex;
  align-items: center;
  gap: 0.5rem;
}

.mono {
  font-family: var(--font-mono);
  font-size: 0.75rem;
  color: var(--text-muted);
}

.schedule-actions {
  display: flex;
  align-items: center;
  gap: 0.25rem;
  flex-shrink: 0;
}

.btn-icon {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 1.75rem;
  height: 1.75rem;
  background: transparent;
  border: none;
  border-radius: var(--radius-sm);
  color: var(--text-muted);
  cursor: pointer;
  transition: background 0.15s, color 0.15s;
}

.btn-icon:hover {
  background: var(--fill-muted);
  color: var(--text);
}

.btn-icon-danger:hover {
  background: var(--fill-danger);
  color: var(--danger);
}
</style>
