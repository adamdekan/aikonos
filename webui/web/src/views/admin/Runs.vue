<script setup>
import { ref, onMounted } from "vue";
import { listAdminRuns } from "../../api/admin.js";
import Icon from "../../components/Icon.vue";
import DataTable from "../../components/ui/DataTable.vue";
import EmptyState from "../../components/ui/EmptyState.vue";

const schedules   = ref([]);
const forbidden   = ref(false);
const loading     = ref(false);
const error       = ref("");
const ownerFilter = ref("");

async function load(owner) {
  loading.value   = true;
  error.value     = "";
  forbidden.value = false;
  try {
    const data = await listAdminRuns(owner || undefined);
    if (data.forbidden) { forbidden.value = true; return; }
    schedules.value = data.schedules ?? [];
  } catch (e) {
    error.value = e.message;
  } finally {
    loading.value = false;
  }
}

onMounted(() => load());

function applyFilter() {
  load(ownerFilter.value.trim() || undefined);
}

function fmt(iso) {
  if (!iso) return "—";
  try { return new Date(iso).toLocaleString(); } catch { return iso; }
}

function badgeClass(state) {
  return { ACTIVE: "s-active", PAUSED: "s-paused", COMPLETED: "s-done", FAILED: "s-fail" }[state] ?? "s-unknown";
}

// Workflow-mode rows carry a non-empty lineage id and always have an empty
// prompt.
function isWorkflowRow(s) {
  return !!(s.workflowLineageId && s.workflowLineageId.trim() !== "");
}

const TABLE_COLS = [
  { key: "owner",      label: "Owner" },
  { key: "schedule",   label: "Schedule" },
  { key: "prompt",     label: "Prompt" },
  { key: "nextFireAt", label: "Next fire" },
  { key: "state",      label: "State",   width: "90px" },
  { key: "last",       label: "Last" },
];
</script>

<template>
  <div class="view">
    <div class="view-header">
      <Icon name="play-circle" class="view-icon" />
      <h1>Scheduled Runs</h1>
    </div>

    <!-- not-an-admin empty-state -->
    <EmptyState
      v-if="forbidden"
      data-testid="forbidden"
      icon="schedules"
      message="You are not a tenant admin."
    />

    <template v-else>
      <div v-if="error" data-testid="error-banner" class="banner-err">{{ error }}</div>

      <!-- owner filter -->
      <div class="filter-bar">
        <input
          v-model="ownerFilter"
          placeholder="Filter by owner email…"
          data-testid="owner-filter"
        />
        <button class="btn-primary" data-testid="filter-btn" @click="applyFilter">
          <Icon name="search" /> Filter
        </button>
      </div>

      <DataTable
        :columns="TABLE_COLS"
        :rows="schedules"
        :loading="loading"
        empty-text="No scheduled runs."
        :row-attrs="{ 'data-testid': 'run-row' }"
      >
        <template #row="{ row }">
          <td class="mono">{{ row.owner }}</td>
          <td class="mono">{{ row.kind === "CRON" ? row.cronExpr : "once" }}</td>
          <td v-if="isWorkflowRow(row)" class="prompt schedule-workflow">
            <span class="badge badge-workflow" data-testid="workflow-badge">Workflow</span>
            {{ row.workflowDisplayName || "(deleted workflow)" }}
          </td>
          <td v-else class="prompt" :title="row.prompt">{{ row.prompt }}</td>
          <td class="muted">{{ fmt(row.nextFireAt) }}</td>
          <td><span class="badge" :class="badgeClass(row.state)">{{ row.state }}</span></td>
          <td class="muted" :title="row.lastSummary || ''">
            {{ row.lastStatus || "—" }}<span v-if="row.runCount"> · {{ row.runCount }}×</span>
          </td>
        </template>
      </DataTable>
    </template>
  </div>
</template>

<style scoped>
.view { padding: 24px 32px; max-width: 1000px; }
.view-header { display: flex; align-items: center; gap: 10px; margin-bottom: 24px; }
.view-header h1 { margin: 0; font-size: 20px; font-weight: 600; }
.view-icon { color: var(--text-muted); width: 22px; height: 22px; }

.banner-err {
  background: var(--fill-danger); border: 1px solid var(--danger);
  border-radius: var(--radius-sm); padding: 10px 14px;
  color: var(--danger); font-size: 13px; margin-bottom: 16px;
}

.muted { color: var(--text-muted); font-size: 13px; }

.filter-bar {
  display: flex; gap: 8px; margin-bottom: 16px; align-items: center;
}
.filter-bar input {
  background: var(--bg-elevated); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 7px 10px; font-size: 13px; width: 300px;
}

.btn-primary {
  display: inline-flex; align-items: center; gap: 4px;
  background: var(--accent); color: var(--text-on-accent);
  border: none; border-radius: var(--radius-sm);
  padding: 7px 13px; font-size: 13px; cursor: pointer;
}
.btn-primary:hover { background: var(--accent-hover); }

.mono    { font-family: var(--font-mono); word-break: break-all; }
.prompt  { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

.badge { border-radius: var(--radius-sm); padding: 1px 7px; font-size: 12px; border: 1px solid transparent; }
.s-active  { background: var(--fill-muted); color: var(--ok);     border-color: var(--ok); }
.s-paused  { background: var(--fill-accent); color: var(--accent); border-color: var(--accent); }
.s-done    { background: var(--fill-muted); color: var(--text-muted); border-color: var(--border); }
.s-fail    { background: var(--fill-danger); color: var(--danger); border-color: var(--danger); }
.s-unknown { background: var(--bg-elevated); color: var(--text-muted); }
.badge-workflow { background: var(--fill-accent); color: var(--accent); border-color: var(--accent); }

.schedule-workflow { display: flex; align-items: center; gap: 6px; }
</style>
