<script setup>
import { ref, onMounted } from "vue";
import { listAlerts } from "../../api/admin.js";
import Icon from "../../components/Icon.vue";
import DataTable from "../../components/ui/DataTable.vue";
import EmptyState from "../../components/ui/EmptyState.vue";
import { useToast } from "../../components/ui/useToast.js";

const { push: toast } = useToast();

const alerts    = ref([]);
const forbidden = ref(false);
const loading   = ref(false);

async function load() {
  loading.value   = true;
  forbidden.value = false;
  try {
    const data = await listAlerts();
    if (data.forbidden) { forbidden.value = true; return; }
    alerts.value = data.alerts ?? [];
  } catch (e) {
    toast("err", e.message);
  } finally {
    loading.value = false;
  }
}

onMounted(load);

function severityClass(s) {
  return { critical: "s-crit", warning: "s-warn", info: "s-info" }[String(s).toLowerCase()] ?? "s-info";
}

function fmtTime(t) {
  if (!t) return "";
  const d = new Date(t);
  return Number.isNaN(d.getTime()) ? String(t) : d.toLocaleString();
}

function fmtSummary(s) {
  if (s == null) return "";
  if (typeof s === "string") return s;
  return JSON.stringify(s);
}

const TABLE_COLS = [
  { key: "firedAt",  label: "Time",     width: "180px" },
  { key: "rule",     label: "Rule" },
  { key: "severity", label: "Severity", width: "100px" },
  { key: "summary",  label: "Detail" },
];
</script>

<template>
  <div class="view">
    <div class="view-header">
      <Icon name="audit" class="view-icon" />
      <h1>Alerts</h1>
    </div>

    <EmptyState
      v-if="forbidden"
      data-testid="forbidden"
      icon="audit"
      message="You are not a tenant admin."
    />

    <template v-else>
      <DataTable
        :columns="TABLE_COLS"
        :rows="alerts"
        :loading="loading"
        empty-text="No alerts. Alerting rules fire only when configured (escalation, unusual actor/host, off-hours destructive, deny-rate)."
        :row-attrs="{ 'data-testid': 'alert-row' }"
      >
        <template #row="{ row }">
          <td class="mono muted">{{ fmtTime(row.firedAt) }}</td>
          <td class="mono">{{ row.rule }}</td>
          <td><span class="badge" :class="severityClass(row.severity)">{{ row.severity }}</span></td>
          <td class="muted detail">{{ fmtSummary(row.summary) }}</td>
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

.muted { color: var(--text-muted); font-size: 13px; }
.mono { font-family: var(--font-mono); word-break: break-all; }
.detail { word-break: break-word; }

.badge {
  border-radius: var(--radius-sm); padding: 1px 7px;
  font-size: 12px; border: 1px solid transparent;
}
.s-crit { background: var(--fill-danger); color: var(--danger); border-color: var(--danger); }
.s-warn { background: var(--fill-accent); color: var(--accent); border-color: var(--accent); }
.s-info { background: var(--fill-muted); color: var(--text-muted); border-color: var(--border); }
</style>
