<script setup>
import { ref, onMounted } from "vue";
import { listNetworkRules, addNetworkRule, deleteNetworkRule } from "../../api/admin.js";
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

// form state (inline add-rule form — not a modal, keep the UX identical)
const scopeKind    = ref("TENANT");
const scopeValue   = ref("");
const action       = ref("ALLOW");
const hostPattern  = ref("");
const note         = ref("");

async function load() {
  loading.value   = true;
  error.value     = "";
  forbidden.value = false;
  try {
    const data = await listNetworkRules();
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
  if (!hostPattern.value.trim()) {
    error.value = "host pattern is required";
    return;
  }
  error.value = "";
  try {
    const resp = await addNetworkRule({
      scopeKind:   scopeKind.value,
      scopeValue:  scopeKind.value === "TENANT" ? "" : scopeValue.value.trim(),
      action:      action.value,
      hostPattern: hostPattern.value.trim(),
      note:        note.value.trim(),
    });
    if (resp.forbidden) {
      error.value = resp.error || "You are not a tenant admin.";
      return;
    }
    hostPattern.value = "";
    note.value        = "";
    scopeValue.value  = "";
    toast("ok", "Rule added.");
    await load();
  } catch (e) {
    error.value = e.message;
  }
}

async function remove(rule) {
  error.value = "";
  try {
    const resp = await deleteNetworkRule(rule.id);
    if (resp.forbidden) {
      error.value = resp.error || "You are not a tenant admin.";
      return;
    }
    toast("ok", "Rule deleted.");
    await load();
  } catch (e) {
    error.value = e.message;
  }
}

function actionClass(a) {
  return { ALLOW: "a-allow", DENY: "a-deny", ASK: "a-ask" }[a] ?? "";
}

function scopeLabel(r) {
  if (r.scopeKind === "TENANT") return "tenant";
  return `${r.scopeKind.toLowerCase()}:${r.scopeValue}`;
}

const TABLE_COLS = [
  { key: "scope",        label: "Scope" },
  { key: "action",       label: "Action",  width: "90px" },
  { key: "hostPattern",  label: "Host" },
  { key: "note",         label: "Note" },
  { key: "_actions",     label: "",        width: "90px" },
];
</script>

<template>
  <div class="view">
    <div class="view-header">
      <Icon name="network" class="view-icon" />
      <h1>Network Access</h1>
    </div>

    <!-- not-an-admin empty-state -->
    <EmptyState
      v-if="forbidden"
      data-testid="forbidden"
      icon="network"
      message="You are not a tenant admin."
    />

    <template v-else>
      <div v-if="error" data-testid="error-banner" class="banner-err">{{ error }}</div>

      <!-- Add rule form -->
      <div class="form">
        <select v-model="scopeKind" data-testid="rule-scope-kind">
          <option value="TENANT">Tenant</option>
          <option value="GROUP">Group</option>
          <option value="USER">User</option>
        </select>
        <input
          v-if="scopeKind !== 'TENANT'"
          v-model="scopeValue"
          :placeholder="scopeKind === 'GROUP' ? 'group name' : 'user email'"
          class="sv"
        />
        <select v-model="action" data-testid="rule-action">
          <option value="ALLOW">ALLOW</option>
          <option value="DENY">DENY</option>
          <option value="ASK">ASK</option>
        </select>
        <input
          v-model="hostPattern"
          placeholder="host or *.suffix or *"
          class="host"
          data-testid="rule-host"
        />
        <input
          v-model="note"
          placeholder="note (optional)"
          class="note-input"
        />
        <button class="btn-primary" data-testid="add-rule-btn" @click="submit">
          <Icon name="plus" /> Add rule
        </button>
      </div>

      <DataTable
        :columns="TABLE_COLS"
        :rows="rules"
        :loading="loading"
        empty-text="No rules — the tenant default is ALLOW (all hosts reachable). Add a TENANT * DENY catch-all plus ALLOW entries to build an allow-list."
        :row-attrs="{ 'data-testid': 'rule-row' }"
      >
        <template #row="{ row }">
          <td class="mono">{{ scopeLabel(row) }}</td>
          <td><span class="badge" :class="actionClass(row.action)">{{ row.action }}</span></td>
          <td class="mono">{{ row.hostPattern }}</td>
          <td class="muted">{{ row.note }}</td>
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

.muted { color: var(--text-muted); font-size: 13px; }

.form {
  display: flex; flex-wrap: wrap; gap: 6px;
  margin-bottom: 16px; align-items: center;
}
.form select, .form input {
  background: var(--bg-elevated); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 7px 9px; font-size: 13px;
}
.form .host { width: 220px; }
.form .sv   { width: 150px; }
.form .note-input { width: 160px; }

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

.badge {
  border-radius: var(--radius-sm); padding: 1px 7px;
  font-size: 12px; border: 1px solid transparent;
}
.a-allow { background: var(--fill-muted); color: var(--ok);     border-color: var(--ok); }
.a-deny  { background: var(--fill-danger); color: var(--danger); border-color: var(--danger); }
.a-ask   { background: var(--fill-accent); color: var(--accent); border-color: var(--accent); }
</style>
