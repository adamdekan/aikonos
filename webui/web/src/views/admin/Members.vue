<script setup>
import { ref, computed, onMounted } from "vue";
import { listMembers } from "../../api/admin.js";
import Icon from "../../components/Icon.vue";
import DataTable from "../../components/ui/DataTable.vue";
import EmptyState from "../../components/ui/EmptyState.vue";
import { useToast } from "../../components/ui/useToast.js";

const { push: toast } = useToast();

const members = ref([]);
const forbidden = ref(false);
const loading = ref(false);
const error = ref("");
const filter = ref("");

async function load() {
  loading.value = true;
  error.value = "";
  forbidden.value = false;
  try {
    const data = await listMembers();
    if (data.forbidden) { forbidden.value = true; return; }
    members.value = data.members ?? [];
  } catch (e) {
    error.value = e.message;
  } finally {
    loading.value = false;
  }
}

onMounted(load);

// Case-insensitive substring filter over name/email/subject (matches the
// filter convention used elsewhere in the webui).
const filtered = computed(() => {
  const q = filter.value.trim().toLowerCase();
  if (!q) return members.value;
  return members.value.filter((m) =>
    [m.name, m.email, m.subject].some((v) => (v ?? "").toLowerCase().includes(q)),
  );
});

function fmtDate(iso) {
  if (!iso) return "—";
  const d = new Date(iso);
  return isNaN(d) ? iso : d.toLocaleString();
}

function csvCell(v) {
  const s = String(v ?? "");
  return /[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
}

function exportCsv() {
  const header = ["subject", "email", "name", "role", "origin", "last_seen", "provisioned_at"];
  const lines = [header.join(",")];
  for (const m of filtered.value) {
    lines.push([
      m.subject,
      m.email,
      m.name,
      m.isAdmin ? "admin" : "user",
      m.provisioned ? "provisioned" : "manual",
      m.lastSeen ?? "",
      m.provisionedAt ?? "",
    ].map(csvCell).join(","));
  }
  const blob = new Blob([lines.join("\n")], { type: "text/csv;charset=utf-8" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = "aikonos-members.csv";
  a.click();
  URL.revokeObjectURL(url);
  toast("ok", `Exported ${filtered.value.length} member(s).`);
}

const TABLE_COLS = [
  { key: "name", label: "Name" },
  { key: "role", label: "Role", width: "90px" },
  { key: "origin", label: "Origin", width: "120px" },
  { key: "lastSeen", label: "Last seen", width: "180px" },
];
</script>

<template>
  <div class="view">
    <div class="view-header">
      <Icon name="users" class="view-icon" />
      <h1>Members</h1>
      <div class="spacer"></div>
      <button
        v-if="!forbidden"
        class="btn-secondary"
        data-testid="export-csv"
        :disabled="loading || filtered.length === 0"
        @click="exportCsv"
      >
        <Icon name="download" /> Export CSV
      </button>
    </div>

    <EmptyState
      v-if="forbidden"
      data-testid="forbidden"
      icon="settings"
      message="You are not a tenant admin."
    />

    <template v-else>
      <p class="lede">
        Everyone who has signed in to this organization. Role and group management live in
        Access Control; this roster is the consolidated directory with sign-in recency and
        provisioning origin.
      </p>

      <div v-if="error" data-testid="error-banner" class="banner-err">{{ error }}</div>

      <input
        v-model="filter"
        class="filter"
        placeholder="Filter by name, email, or subject…"
        data-testid="member-filter"
      />

      <DataTable
        :columns="TABLE_COLS"
        :rows="filtered"
        :loading="loading"
        empty-text="No members have signed in yet."
        :row-attrs="{ 'data-testid': 'member-row' }"
      >
        <template #row="{ row }">
          <td>
            <div class="name">{{ row.name || row.email || row.subject }}</div>
            <div class="sub mono">{{ row.email || row.subject }}</div>
          </td>
          <td>
            <span :class="row.isAdmin ? 'badge-admin' : 'badge-user'">
              {{ row.isAdmin ? "admin" : "user" }}
            </span>
          </td>
          <td>
            <span :class="row.provisioned ? 'origin-jit' : 'origin-manual'">
              {{ row.provisioned ? "provisioned" : "manual" }}
            </span>
          </td>
          <td class="muted">{{ fmtDate(row.lastSeen) }}</td>
        </template>
      </DataTable>
    </template>
  </div>
</template>

<style scoped>
.view { padding: 24px 32px; max-width: 940px; }
.view-header { display: flex; align-items: center; gap: 10px; margin-bottom: 16px; }
.view-header h1 { margin: 0; font-size: 20px; font-weight: 600; }
.view-icon { color: var(--text-muted); width: 22px; height: 22px; }
.spacer { flex: 1; }

.lede { color: var(--text-muted); font-size: 13px; line-height: 1.6; margin: 0 0 16px; max-width: 68ch; }

.banner-err {
  background: var(--fill-danger); border: 1px solid var(--danger);
  border-radius: var(--radius-sm); padding: 10px 14px;
  color: var(--danger); font-size: 13px; margin-bottom: 16px;
}

.filter {
  width: 320px; max-width: 100%;
  background: var(--bg-elevated); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 7px 11px; font-size: 13px; margin-bottom: 16px;
}
.filter:focus { outline: none; border-color: var(--accent); }

.name { font-size: 14px; }
.sub { font-size: 12px; color: var(--text-faint); }
.mono { font-family: var(--font-mono); }
.muted { color: var(--text-muted); font-size: 13px; }

.badge-admin, .badge-user, .origin-jit, .origin-manual {
  font-size: 11px; font-weight: 500; border-radius: 4px; padding: 2px 8px;
}
.badge-admin { color: var(--accent); background: var(--fill-accent); }
.badge-user  { color: var(--text-faint); background: var(--fill-muted); }
.origin-jit    { color: var(--ok); background: var(--fill-ok); }
.origin-manual { color: var(--text-faint); background: var(--fill-muted); }

.btn-secondary {
  display: inline-flex; align-items: center; gap: 5px;
  background: transparent; color: var(--text-muted);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 7px 13px; font-size: 13px; cursor: pointer;
}
.btn-secondary:hover:not(:disabled) { background: var(--bg-hover); color: var(--text); }
.btn-secondary:disabled { opacity: 0.5; cursor: not-allowed; }
</style>
