<script setup>
import { ref, computed, onMounted } from "vue";
import { listMembers, setUserBlocked, revokeUserSessions } from "../../api/admin.js";
import Icon from "../../components/Icon.vue";
import DataTable from "../../components/ui/DataTable.vue";
import EmptyState from "../../components/ui/EmptyState.vue";
import Modal from "../../components/ui/Modal.vue";
import { useToast } from "../../components/ui/useToast.js";
import { csvEscape } from "./auditExport.js";

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

function exportCsv() {
  const header = ["subject", "email", "name", "role", "origin", "status", "last_seen", "provisioned_at"];
  const lines = [header.join(",")];
  for (const m of filtered.value) {
    lines.push([
      m.subject,
      m.email,
      m.name,
      m.isAdmin ? "admin" : "user",
      m.provisioned ? "provisioned" : "manual",
      m.blocked ? "blocked" : "active",
      m.lastSeen ?? "",
      m.provisionedAt ?? "",
    ].map(csvEscape).join(","));
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

// ── AG-06: block / unblock / revoke all sessions ─────────────────────────────
// One reason dialog serves all three actions; the broker requires a reason and
// audits it with the acting admin.
const ACTIONS = {
  block:   { title: "Block user", verb: "Block", run: (m, r) => setUserBlocked(m.subject, true, r),
             done: "User blocked. Their sessions stop within a minute." },
  unblock: { title: "Unblock user", verb: "Unblock", run: (m, r) => setUserBlocked(m.subject, false, r),
             done: "User unblocked." },
  revoke:  { title: "Revoke all sessions", verb: "Revoke sessions", run: (m, r) => revokeUserSessions(m.subject, r),
             done: "Sessions revoked. The user must sign in again." },
};
const pending = ref(null); // { kind, member }
const reason = ref("");
const actionError = ref("");
const acting = ref(false);

function ask(kind, member) {
  pending.value = { kind, member };
  reason.value = "";
  actionError.value = "";
}

async function confirmAction() {
  const { kind, member } = pending.value;
  if (!reason.value.trim()) { actionError.value = "A reason is required."; return; }
  acting.value = true;
  try {
    await ACTIONS[kind].run(member, reason.value.trim());
    toast("ok", ACTIONS[kind].done);
    pending.value = null;
    await load();
  } catch (e) {
    actionError.value = e.message;
  } finally {
    acting.value = false;
  }
}

const TABLE_COLS = [
  { key: "name", label: "Name" },
  { key: "role", label: "Role", width: "90px" },
  { key: "origin", label: "Origin", width: "120px" },
  { key: "status", label: "Status", width: "110px" },
  { key: "lastSeen", label: "Last seen", width: "180px" },
  { key: "_actions", label: "", width: "220px" },
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
          <td>
            <span
              :class="row.blocked ? 'status-blocked' : 'status-active'"
              :data-testid="`member-status-${row.subject}`"
              :title="row.blocked ? `Blocked by ${row.blockedBy} on ${fmtDate(row.blockedAt)}: ${row.blockReason}` : ''"
            >{{ row.blocked ? "blocked" : "active" }}</span>
            <div v-if="row.sessionsRevokedAt" class="sub" :data-testid="`member-revoked-at-${row.subject}`">
              sessions revoked {{ fmtDate(row.sessionsRevokedAt) }}
            </div>
          </td>
          <td class="muted">{{ fmtDate(row.lastSeen) }}</td>
          <td class="actions">
            <button
              v-if="!row.blocked"
              class="btn-danger-sm"
              :data-testid="`member-block-${row.subject}`"
              @click="ask('block', row)"
            >Block</button>
            <button
              v-else
              class="btn-secondary-sm"
              :data-testid="`member-unblock-${row.subject}`"
              @click="ask('unblock', row)"
            >Unblock</button>
            <button
              class="btn-secondary-sm"
              :data-testid="`member-revoke-sessions-${row.subject}`"
              @click="ask('revoke', row)"
            >Revoke sessions</button>
          </td>
        </template>
      </DataTable>
    </template>

    <Modal :open="!!pending" :title="pending ? ACTIONS[pending.kind].title : ''" @close="pending = null">
      <template v-if="pending">
        <p class="lede">
          {{ pending.member.name || pending.member.email || pending.member.subject }}
        </p>
        <label class="reason-label" for="revocation-reason">Reason (recorded in the audit log)</label>
        <textarea
          id="revocation-reason"
          v-model="reason"
          class="reason"
          rows="3"
          maxlength="500"
          data-testid="revocation-reason"
        ></textarea>
        <div v-if="actionError" class="banner-err" data-testid="revocation-error">{{ actionError }}</div>
      </template>
      <template #footer>
        <button class="btn-secondary" data-testid="revocation-cancel" @click="pending = null">Cancel</button>
        <button
          class="btn-danger"
          data-testid="revocation-confirm"
          :disabled="acting || !reason.trim()"
          @click="confirmAction"
        >{{ pending ? ACTIONS[pending.kind].verb : "" }}</button>
      </template>
    </Modal>
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
.filter:focus { outline: none; border-color: var(--accent-text); }

.name { font-size: 14px; }
.sub { font-size: 12px; color: var(--text-faint); }
.mono { font-family: var(--font-mono); }
.muted { color: var(--text-muted); font-size: 13px; }

.badge-admin, .badge-user, .origin-jit, .origin-manual {
  font-size: 11px; font-weight: 500; border-radius: 4px; padding: 2px 8px;
}
.badge-admin { color: var(--accent-text); background: var(--fill-accent); }
.badge-user  { color: var(--text-faint); background: var(--fill-muted); }
.origin-jit    { color: var(--ok); background: var(--fill-ok); }
.origin-manual { color: var(--text-faint); background: var(--fill-muted); }

.status-active, .status-blocked { font-size: 11px; font-weight: 500; border-radius: 4px; padding: 2px 8px; }
.status-active  { color: var(--ok); background: var(--fill-ok); }
.status-blocked { color: var(--danger); background: var(--fill-danger); }
.actions { text-align: right; white-space: nowrap; }
.actions button + button { margin-left: 6px; }
.reason-label { display: block; font-size: 12px; color: var(--text-muted); margin-bottom: 6px; }
.reason {
  width: 100%; box-sizing: border-box; resize: vertical;
  background: var(--bg); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 7px 10px; font-size: 13px; font-family: inherit; margin-bottom: 10px;
}
.btn-secondary-sm, .btn-danger-sm {
  background: transparent; border-radius: var(--radius-sm);
  padding: 4px 10px; font-size: 12px; cursor: pointer;
}
.btn-secondary-sm { color: var(--text); border: 1px solid var(--border); }
.btn-secondary-sm:hover { background: var(--bg-hover); }
.btn-danger-sm { color: var(--danger); border: 1px solid var(--danger); }
.btn-danger-sm:hover { background: var(--fill-danger); }
.btn-danger {
  background: var(--fill-danger); color: var(--danger);
  border: 1px solid var(--danger); border-radius: var(--radius-sm);
  padding: 7px 13px; font-size: 13px; cursor: pointer; margin-left: 8px;
}
.btn-danger:disabled { opacity: 0.5; cursor: not-allowed; }

.btn-secondary {
  display: inline-flex; align-items: center; gap: 5px;
  background: transparent; color: var(--text-muted);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 7px 13px; font-size: 13px; cursor: pointer;
}
.btn-secondary:hover:not(:disabled) { background: var(--bg-hover); color: var(--text); }
.btn-secondary:disabled { opacity: 0.5; cursor: not-allowed; }
</style>
