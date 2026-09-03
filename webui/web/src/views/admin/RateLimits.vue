<script setup>
import { ref, onMounted } from "vue";
import { listRateLimitPolicies, setRateLimitPolicy, deleteRateLimitPolicy } from "../../api/admin.js";
import Icon from "../../components/Icon.vue";
import DataTable from "../../components/ui/DataTable.vue";
import EmptyState from "../../components/ui/EmptyState.vue";
import FormField from "../../components/ui/FormField.vue";
import { useToast } from "../../components/ui/useToast.js";

const { push: toast } = useToast();

const policies  = ref([]);
const forbidden = ref(false);
const loading   = ref(false);
const error     = ref("");

// form state
const agentId  = ref("");
const provider = ref("");
const rpmLimit = ref("");
const tpmLimit = ref("");

async function load() {
  loading.value   = true;
  error.value     = "";
  forbidden.value = false;
  try {
    const data = await listRateLimitPolicies();
    if (data.forbidden) { forbidden.value = true; return; }
    policies.value = data.policies ?? [];
  } catch (e) {
    error.value = e.message;
  } finally {
    loading.value = false;
  }
}

onMounted(load);

async function submit() {
  error.value = "";
  const rpm = rpmLimit.value.trim() === "" ? undefined : parseInt(rpmLimit.value, 10);
  const tpm = tpmLimit.value.trim() === "" ? undefined : parseInt(tpmLimit.value, 10);
  if (rpm !== undefined && (isNaN(rpm) || rpm < 0)) { error.value = "RPM must be a non-negative integer."; return; }
  if (tpm !== undefined && (isNaN(tpm) || tpm < 0)) { error.value = "TPM must be a non-negative integer."; return; }
  try {
    const resp = await setRateLimitPolicy({
      agentId:  agentId.value.trim()  || undefined,
      provider: provider.value.trim() || undefined,
      rpmLimit: rpm,
      tpmLimit: tpm,
    });
    if (resp.forbidden) {
      error.value = resp.error || "You are not a tenant admin.";
      return;
    }
    agentId.value  = "";
    provider.value = "";
    rpmLimit.value = "";
    tpmLimit.value = "";
    toast("ok", "Policy added.");
    await load();
  } catch (e) {
    error.value = e.message;
  }
}

async function remove(policy) {
  error.value = "";
  try {
    const resp = await deleteRateLimitPolicy(policy.id);
    if (resp.forbidden) {
      error.value = resp.error || "You are not a tenant admin.";
      return;
    }
    toast("ok", "Policy deleted.");
    await load();
  } catch (e) {
    error.value = e.message;
  }
}

function fmtLimit(val) {
  if (val === null || val === undefined) return "∞";
  if (val === -1) return "∞";
  if (val === 0) return "deny-all";
  return val;
}

const TABLE_COLS = [
  { key: "agentId",   label: "Agent ID" },
  { key: "provider",  label: "Provider" },
  { key: "rpmLimit",  label: "RPM Limit", width: "110px" },
  { key: "tpmLimit",  label: "TPM Limit", width: "110px" },
  { key: "_actions",  label: "",          width: "90px" },
];
</script>

<template>
  <div class="view">
    <div class="view-header">
      <Icon name="gauge" class="view-icon" />
      <h1>Rate Limits</h1>
    </div>

    <EmptyState
      v-if="forbidden"
      data-testid="forbidden"
      icon="settings"
      message="You are not a tenant admin."
    />

    <template v-else>
      <div v-if="error" data-testid="error-banner" class="banner-err">{{ error }}</div>

      <!-- Add policy form -->
      <div class="form">
        <input
          v-model="agentId"
          placeholder="leave blank for all agents"
          class="field-agent"
          data-testid="policy-agent-id"
        />
        <input
          v-model="provider"
          placeholder="leave blank for all providers"
          class="field-provider"
          data-testid="policy-provider"
        />
        <input
          v-model="rpmLimit"
          type="number"
          min="0"
          placeholder="leave blank for unlimited"
          class="field-limit"
          data-testid="policy-rpm-limit"
        />
        <input
          v-model="tpmLimit"
          type="number"
          min="0"
          placeholder="leave blank for unlimited"
          class="field-limit"
          data-testid="policy-tpm-limit"
        />
        <button class="btn-primary" data-testid="add-policy-btn" @click="submit">
          <Icon name="plus" /> Add Policy
        </button>
      </div>

      <DataTable
        :columns="TABLE_COLS"
        :rows="policies"
        :loading="loading"
        empty-text="No rate limit policies — all agents and providers are unrestricted."
        :row-attrs="{ 'data-testid': 'policy-row' }"
      >
        <template #row="{ row }">
          <td class="mono">{{ row.agentId ?? "—" }}</td>
          <td class="mono">{{ row.provider ?? "—" }}</td>
          <td>{{ fmtLimit(row.rpmLimit) }}</td>
          <td>{{ fmtLimit(row.tpmLimit) }}</td>
          <td class="right">
            <button
              :data-testid="`delete-${row.id}`"
              class="btn-danger-sm"
              @click="remove(row)"
            >
              <Icon name="trash" /> Delete
            </button>
          </td>
        </template>
      </DataTable>
    </template>
  </div>
</template>

<style scoped>
.view { padding: 24px 32px; max-width: 900px; }
.view-header { display: flex; align-items: center; gap: 10px; margin-bottom: 24px; }
.view-header h1 { margin: 0; font-size: 20px; font-weight: 600; }
.view-icon { color: var(--text-muted); width: 22px; height: 22px; }

.banner-err {
  background: var(--fill-danger); border: 1px solid var(--danger);
  border-radius: var(--radius-sm); padding: 10px 14px;
  color: var(--danger); font-size: 13px; margin-bottom: 16px;
}

.form {
  display: flex; flex-wrap: wrap; gap: 6px;
  margin-bottom: 16px; align-items: center;
}
.form input {
  background: var(--bg-elevated); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 7px 9px; font-size: 13px;
}
.form .field-agent    { width: 200px; }
.form .field-provider { width: 180px; }
.form .field-limit    { width: 180px; }

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

.mono { font-family: var(--font-mono); word-break: break-all; }
.right { text-align: right; }
</style>
