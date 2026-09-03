<script setup>
import { ref, onMounted } from "vue";
import {
  listSpendCaps,
  setSpendCap,
  deleteSpendCap,
  getSpendSummary,
  listMembers,
  listAgents,
} from "../../api/admin.js";
import { toMicros, fmtAmount } from "../../lib/money.js";
import Icon from "../../components/Icon.vue";
import DataTable from "../../components/ui/DataTable.vue";
import EmptyState from "../../components/ui/EmptyState.vue";
import ErrorBanner from "../../components/ui/ErrorBanner.vue";
import { useToast } from "../../components/ui/useToast.js";

const { push: toast } = useToast();

const forbidden = ref(false);
const loading   = ref(false);
// Persistent load failure -> ErrorBanner (F34). Action-level failures (a
// single set/delete call) surface as a toast instead, matching the RateLimits
// convention this view mirrors.
const loadError = ref("");

const summary = ref({ orgSpendMicros: 0, orgCapMicros: 0, users: [], agents: [] });
const caps    = ref([]);   // raw cap rows — carries the `id` needed to delete
const members = ref([]);
const agents  = ref([]);

// org cap form
const orgCapInput = ref("");

// user cap form
const userSubject  = ref("");
const userCapInput = ref("");

// agent cap form
const agentId       = ref("");
const agentCapInput = ref("");

async function load() {
  loading.value   = true;
  loadError.value = "";
  forbidden.value = false;
  try {
    const [sumData, capsData, memberData, agentData] = await Promise.all([
      getSpendSummary(),
      listSpendCaps(),
      listMembers().catch(() => ({ members: [] })),
      listAgents().catch(() => ({ agents: [] })),
    ]);
    if (sumData.forbidden || capsData.forbidden) { forbidden.value = true; return; }
    summary.value = {
      orgSpendMicros: sumData.orgSpendMicros ?? 0,
      orgCapMicros:   sumData.orgCapMicros ?? 0,
      users:          sumData.users ?? [],
      agents:         sumData.agents ?? [],
    };
    caps.value    = capsData.caps ?? [];
    members.value = memberData.members ?? [];
    agents.value  = agentData.agents ?? [];
  } catch (e) {
    loadError.value = e.message;
  } finally {
    loading.value = false;
  }
}

onMounted(load);

function capId(scope, subjectId) {
  const row = caps.value.find((c) => c.scope === scope && c.subjectId === subjectId);
  return row ? row.id : null;
}

// pctUsed returns null when no cap is set (blank in the UI) instead of a
// misleading 0%/divide-by-zero.
function pctUsed(spendMicros, capMicros) {
  if (!capMicros) return null;
  return Math.min(100, Math.round(((spendMicros ?? 0) / capMicros) * 100));
}

async function saveOrgCap() {
  const amount = Number(orgCapInput.value);
  if (orgCapInput.value === "" || isNaN(amount) || amount <= 0) {
    toast("error", "Org cap must be a positive amount.");
    return;
  }
  try {
    const resp = await setSpendCap({ scope: "org", subjectId: "", capMicros: toMicros(amount) });
    if (resp.forbidden) { toast("error", resp.error || "You are not a tenant admin."); return; }
    orgCapInput.value = "";
    toast("ok", "Org cap set.");
    await load();
  } catch (e) {
    toast("error", e.message);
  }
}

async function clearOrgCap() {
  const id = capId("org", "");
  if (!id) return;
  try {
    const resp = await deleteSpendCap(id);
    if (resp.forbidden) { toast("error", resp.error || "You are not a tenant admin."); return; }
    toast("ok", "Org cap cleared.");
    await load();
  } catch (e) {
    toast("error", e.message);
  }
}

async function saveUserCap() {
  const subjectId = userSubject.value.trim();
  const amount = parseFloat(userCapInput.value);
  if (!subjectId) { toast("error", "Pick or enter a user."); return; }
  if (isNaN(amount) || amount <= 0) { toast("error", "Cap must be a positive amount."); return; }
  try {
    const resp = await setSpendCap({ scope: "user", subjectId, capMicros: toMicros(amount) });
    if (resp.forbidden) { toast("error", resp.error || "You are not a tenant admin."); return; }
    userSubject.value  = "";
    userCapInput.value = "";
    toast("ok", "User cap set.");
    await load();
  } catch (e) {
    toast("error", e.message);
  }
}

async function removeUserCap(row) {
  const id = capId("user", row.userId);
  if (!id) return;
  try {
    const resp = await deleteSpendCap(id);
    if (resp.forbidden) { toast("error", resp.error || "You are not a tenant admin."); return; }
    toast("ok", "User cap removed.");
    await load();
  } catch (e) {
    toast("error", e.message);
  }
}

async function saveAgentCap() {
  const subjectId = agentId.value.trim();
  const amount = parseFloat(agentCapInput.value);
  if (!subjectId) { toast("error", "Pick or enter an agent."); return; }
  if (isNaN(amount) || amount <= 0) { toast("error", "Cap must be a positive amount."); return; }
  try {
    const resp = await setSpendCap({ scope: "agent", subjectId, capMicros: toMicros(amount) });
    if (resp.forbidden) { toast("error", resp.error || "You are not a tenant admin."); return; }
    agentId.value       = "";
    agentCapInput.value = "";
    toast("ok", "Agent cap set.");
    await load();
  } catch (e) {
    toast("error", e.message);
  }
}

async function removeAgentCap(row) {
  const id = capId("agent", row.agentId);
  if (!id) return;
  try {
    const resp = await deleteSpendCap(id);
    if (resp.forbidden) { toast("error", resp.error || "You are not a tenant admin."); return; }
    toast("ok", "Agent cap removed.");
    await load();
  } catch (e) {
    toast("error", e.message);
  }
}

const USER_COLS = [
  { key: "userId",      label: "User" },
  { key: "spendMicros", label: "Spend" },
  { key: "capMicros",   label: "Cap" },
  { key: "_pct",         label: "% Used", width: "120px" },
  { key: "_actions",     label: "",       width: "90px" },
];

const AGENT_COLS = [
  { key: "agentId",     label: "Agent" },
  { key: "spendMicros", label: "Spend" },
  { key: "capMicros",   label: "Cap" },
  { key: "_pct",         label: "% Used", width: "120px" },
  { key: "_actions",     label: "",       width: "90px" },
];
</script>

<template>
  <div class="view">
    <div class="view-header">
      <Icon name="gauge" class="view-icon" />
      <h1>Spend Caps</h1>
    </div>

    <EmptyState
      v-if="forbidden"
      data-testid="forbidden"
      icon="settings"
      message="You are not a tenant admin."
    />

    <template v-else>
      <ErrorBanner v-if="loadError" :message="loadError" />

      <!-- Org section -->
      <section class="section">
        <h2>Organization</h2>
        <div class="org-row">
          <div class="org-stat">
            <span class="muted small">Current-period spend</span>
            <span class="mono">{{ fmtAmount(summary.orgSpendMicros) }}</span>
          </div>
          <div class="org-stat">
            <span class="muted small">Cap</span>
            <span class="mono">{{ summary.orgCapMicros ? fmtAmount(summary.orgCapMicros) : "no cap set" }}</span>
          </div>
          <div class="util-bar" data-testid="org-utilization">
            <div
              v-if="summary.orgCapMicros"
              class="util-fill"
              :style="{ width: `${pctUsed(summary.orgSpendMicros, summary.orgCapMicros)}%` }"
            />
          </div>
          <span class="muted small" data-testid="org-pct">
            {{ summary.orgCapMicros ? `${pctUsed(summary.orgSpendMicros, summary.orgCapMicros)}%` : "—" }}
          </span>
        </div>
        <div class="form">
          <input
            v-model="orgCapInput"
            type="number"
            step="0.01"
            min="0"
            placeholder="org cap amount"
            data-testid="org-cap-input"
          />
          <button class="btn-primary" data-testid="org-cap-save-btn" @click="saveOrgCap">
            <Icon name="plus" /> Set org cap
          </button>
          <button
            v-if="summary.orgCapMicros"
            class="btn-danger-sm"
            data-testid="org-cap-clear-btn"
            @click="clearOrgCap"
          >
            <Icon name="trash" /> Clear
          </button>
        </div>
      </section>

      <!-- Per-user section -->
      <section class="section">
        <h2>Per-user</h2>
        <div class="form">
          <input
            v-model="userSubject"
            list="spend-caps-members"
            placeholder="user (pick or type email)"
            class="field-wide"
            data-testid="user-pick"
          />
          <datalist id="spend-caps-members">
            <option v-for="m in members" :key="m.subject" :value="m.subject">
              {{ m.name || m.email || m.subject }}
            </option>
          </datalist>
          <input
            v-model="userCapInput"
            type="number"
            step="0.01"
            min="0"
            placeholder="cap amount"
            data-testid="user-cap-input"
          />
          <button class="btn-primary" data-testid="user-cap-save-btn" @click="saveUserCap">
            <Icon name="plus" /> Set cap
          </button>
        </div>

        <DataTable
          :columns="USER_COLS"
          :rows="summary.users"
          :loading="loading"
          empty-text="No per-user spend recorded this period."
          :row-attrs="{ 'data-testid': 'user-row' }"
        >
          <template #row="{ row }">
            <td class="mono">{{ row.userId }}</td>
            <td class="mono">{{ fmtAmount(row.spendMicros) }}</td>
            <td class="mono">{{ row.capMicros ? fmtAmount(row.capMicros) : "—" }}</td>
            <td>{{ row.capMicros ? `${pctUsed(row.spendMicros, row.capMicros)}%` : "—" }}</td>
            <td class="right">
              <button
                v-if="capId('user', row.userId)"
                :data-testid="`delete-user-${row.userId}`"
                class="btn-danger-sm"
                @click="removeUserCap(row)"
              >
                <Icon name="trash" /> Delete
              </button>
            </td>
          </template>
        </DataTable>
      </section>

      <!-- Per-agent section -->
      <section class="section">
        <h2>Per-agent</h2>
        <div class="form">
          <input
            v-model="agentId"
            list="spend-caps-agents"
            placeholder="agent (pick or type id)"
            class="field-wide"
            data-testid="agent-pick"
          />
          <datalist id="spend-caps-agents">
            <option v-for="a in agents" :key="a.id" :value="a.id">{{ a.name || a.id }}</option>
          </datalist>
          <input
            v-model="agentCapInput"
            type="number"
            step="0.01"
            min="0"
            placeholder="cap amount"
            data-testid="agent-cap-input"
          />
          <button class="btn-primary" data-testid="agent-cap-save-btn" @click="saveAgentCap">
            <Icon name="plus" /> Set cap
          </button>
        </div>

        <DataTable
          :columns="AGENT_COLS"
          :rows="summary.agents"
          :loading="loading"
          empty-text="No per-agent spend recorded this period."
          :row-attrs="{ 'data-testid': 'agent-row' }"
        >
          <template #row="{ row }">
            <td class="mono">{{ row.agentId }}</td>
            <td class="mono">{{ fmtAmount(row.spendMicros) }}</td>
            <td class="mono">{{ row.capMicros ? fmtAmount(row.capMicros) : "—" }}</td>
            <td>{{ row.capMicros ? `${pctUsed(row.spendMicros, row.capMicros)}%` : "—" }}</td>
            <td class="right">
              <button
                v-if="capId('agent', row.agentId)"
                :data-testid="`delete-agent-${row.agentId}`"
                class="btn-danger-sm"
                @click="removeAgentCap(row)"
              >
                <Icon name="trash" /> Delete
              </button>
            </td>
          </template>
        </DataTable>
      </section>
    </template>
  </div>
</template>

<style scoped>
.view { padding: 24px 32px; max-width: 900px; }
.view-header { display: flex; align-items: center; gap: 10px; margin-bottom: 24px; }
.view-header h1 { margin: 0; font-size: 20px; font-weight: 600; }
.view-icon { color: var(--text-muted); width: 22px; height: 22px; }

.section { margin-bottom: 32px; }
.section h2 { font-size: 14px; font-weight: 600; margin: 0 0 12px; color: var(--text-muted); }

.org-row {
  display: flex; align-items: center; gap: 20px;
  margin-bottom: 12px;
}
.org-stat { display: flex; flex-direction: column; gap: 2px; }

.util-bar {
  flex: 1; height: 8px; border-radius: 4px;
  background: var(--fill-muted); overflow: hidden;
}
.util-fill { height: 100%; background: var(--accent); }

.form {
  display: flex; flex-wrap: wrap; gap: 6px;
  margin-bottom: 16px; align-items: center;
}
.form input {
  background: var(--bg-elevated); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 7px 9px; font-size: 13px; width: 160px;
}
.form .field-wide { width: 220px; }

.btn-primary {
  display: inline-flex; align-items: center; gap: 4px;
  background: var(--accent); color: var(--text-on-accent);
  border: none; border-radius: var(--radius-sm);
  padding: 7px 13px; font-size: 13px; cursor: pointer;
}
.btn-primary:hover { background: var(--accent-hover); }

.btn-danger-sm {
  display: inline-flex; align-items: center; gap: 4px;
  background: transparent; color: var(--danger);
  border: 1px solid var(--danger); border-radius: var(--radius-sm);
  padding: 3px 10px; cursor: pointer; font-size: 12px; opacity: 0.8;
}
.btn-danger-sm:hover { background: var(--fill-danger); opacity: 1; }

.muted { color: var(--text-muted); }
.small { font-size: 12px; }
.mono { font-family: var(--font-mono); word-break: break-all; }
.right { text-align: right; }
</style>
