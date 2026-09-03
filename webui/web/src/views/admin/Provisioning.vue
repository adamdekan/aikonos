<script setup>
import { ref, onMounted } from "vue";
import { listProvisioningRules, addProvisioningRule, deleteProvisioningRule } from "../../api/admin.js";
import Icon from "../../components/Icon.vue";
import DataTable from "../../components/ui/DataTable.vue";
import EmptyState from "../../components/ui/EmptyState.vue";
import FormField from "../../components/ui/FormField.vue";
import { useToast } from "../../components/ui/useToast.js";

const { push: toast } = useToast();

const rules     = ref([]);
const forbidden = ref(false);
const loading   = ref(false);
const error     = ref("");
const addNotice = ref("");

// form state
const matcher    = ref("");
const groupsRaw  = ref("");

async function load() {
  loading.value   = true;
  error.value     = "";
  forbidden.value = false;
  try {
    const data = await listProvisioningRules();
    if (data.forbidden) { forbidden.value = true; return; }
    rules.value = data.rules ?? [];
  } catch (e) {
    error.value = e.message;
  } finally {
    loading.value = false;
  }
}

onMounted(load);

async function submit() {
  if (!matcher.value.trim()) {
    error.value = "matcher is required";
    return;
  }
  const groups = groupsRaw.value
    .split(",")
    .map((g) => g.trim())
    .filter(Boolean);
  if (groups.length === 0) {
    error.value = "at least one group is required";
    return;
  }
  error.value  = "";
  addNotice.value = "";
  try {
    const resp = await addProvisioningRule(matcher.value.trim(), groups);
    if (resp.forbidden) {
      error.value = resp.error || "You are not a tenant admin.";
      return;
    }
    if (resp.error) {
      error.value = resp.error;
      return;
    }
    const count = resp.appliedCount ?? 0;
    matcher.value   = "";
    groupsRaw.value = "";
    toast("ok", "Rule added.");
    if (count > 0) {
      addNotice.value = `Applied to ${count} existing user${count === 1 ? "" : "s"}.`;
    }
    await load();
  } catch (e) {
    error.value = e.message;
  }
}

async function remove(rule) {
  if (!confirm(`Delete rule for "${rule.matcher}"? Granted roles are not revoked.`)) return;
  error.value = "";
  try {
    const resp = await deleteProvisioningRule(rule.id);
    if (resp && resp.forbidden) {
      error.value = resp.error || "You are not a tenant admin.";
      return;
    }
    toast("ok", "Rule deleted.");
    await load();
  } catch (e) {
    error.value = e.message;
  }
}

function formatGroups(groups) {
  return groups ?? [];
}

function formatDate(iso) {
  if (!iso) return "—";
  return new Date(iso).toLocaleString();
}

const TABLE_COLS = [
  { key: "matcher",   label: "Matcher" },
  { key: "groups",    label: "Groups" },
  { key: "createdBy", label: "Created by", width: "160px" },
  { key: "createdAt", label: "Created at", width: "160px" },
  { key: "_actions",  label: "",           width: "90px" },
];
</script>

<template>
  <div class="view">
    <div class="view-header">
      <Icon name="admin" class="view-icon" />
      <h1>Provisioning</h1>
    </div>

    <!-- semantic banner explaining rule behaviour -->
    <div class="banner-info">
      Wildcard rules (<code>*@domain</code>) apply once at a user's first login. Exact-email rules
      also apply immediately to users who have already signed in. Deleting a rule does
      <strong>not</strong> revoke granted roles — use the Roles tab.
    </div>

    <!-- not-an-admin empty-state -->
    <EmptyState
      v-if="forbidden"
      data-testid="forbidden"
      icon="admin"
      message="You are not a tenant admin."
    />

    <template v-else>
      <div v-if="error" data-testid="error-banner" class="banner-err">{{ error }}</div>
      <div v-if="addNotice" data-testid="add-notice" class="banner-ok">{{ addNotice }}</div>

      <!-- Add rule form -->
      <div class="form">
        <input
          v-model="matcher"
          placeholder="alice@example.com or *@example.com"
          class="matcher-input"
          data-testid="rule-matcher"
        />
        <input
          v-model="groupsRaw"
          placeholder="group1, group2"
          class="groups-input"
          data-testid="rule-groups"
        />
        <button class="btn-primary" data-testid="add-rule-btn" @click="submit">
          <Icon name="plus" /> Add rule
        </button>
      </div>

      <DataTable
        :columns="TABLE_COLS"
        :rows="rules"
        :loading="loading"
        empty-text="No provisioning rules. Add a wildcard rule (*@yourdomain.com) to auto-grant groups on first login."
        :row-attrs="{ 'data-testid': 'rule-row' }"
      >
        <template #row="{ row }">
          <td class="mono">{{ row.matcher }}</td>
          <td>
            <span
              v-for="g in formatGroups(row.groups)"
              :key="g"
              class="group-chip"
            >{{ g }}</span>
            <span v-if="!row.groups || row.groups.length === 0" class="muted">—</span>
          </td>
          <td class="muted">{{ row.createdBy || "—" }}</td>
          <td class="muted">{{ formatDate(row.createdAt) }}</td>
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
.view-header { display: flex; align-items: center; gap: 10px; margin-bottom: 16px; }
.view-header h1 { margin: 0; font-size: 20px; font-weight: 600; }
.view-icon { color: var(--text-muted); width: 22px; height: 22px; }

.banner-info {
  background: var(--fill-muted); border: 1px solid var(--border);
  border-radius: var(--radius-sm); padding: 10px 14px;
  color: var(--text-muted); font-size: 13px; margin-bottom: 16px;
  line-height: 1.5;
}
.banner-info code { font-family: var(--font-mono); }

.banner-err {
  background: var(--fill-danger); border: 1px solid var(--danger);
  border-radius: var(--radius-sm); padding: 10px 14px;
  color: var(--danger); font-size: 13px; margin-bottom: 16px;
}

.banner-ok {
  background: var(--fill-muted); border: 1px solid var(--ok);
  border-radius: var(--radius-sm); padding: 10px 14px;
  color: var(--ok); font-size: 13px; margin-bottom: 16px;
}

.muted { color: var(--text-muted); font-size: 13px; }

.form {
  display: flex; flex-wrap: wrap; gap: 6px;
  margin-bottom: 16px; align-items: center;
}
.form input {
  background: var(--bg-elevated); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 7px 9px; font-size: 13px;
}
.form .matcher-input { width: 240px; }
.form .groups-input  { width: 220px; }

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

.group-chip {
  display: inline-block;
  background: var(--fill-muted); border: 1px solid var(--border);
  border-radius: var(--radius-sm); padding: 1px 7px;
  font-size: 12px; margin: 1px 3px 1px 0;
}
</style>
