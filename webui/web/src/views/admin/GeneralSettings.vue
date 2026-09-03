<script setup>
import { ref, onMounted } from "vue";
import { get, put } from "../../api/client.js";
import Icon from "../../components/Icon.vue";
import EmptyState from "../../components/ui/EmptyState.vue";
import Spinner from "../../components/ui/Spinner.vue";

const entries  = ref([]);
const loading  = ref(false);
const forbidden = ref(false);

// Per-row state: edited value, save status, error message.
const rowValues  = ref({});
const rowSuccess = ref({});
const rowErrors  = ref({});

onMounted(async () => {
  loading.value = true;
  try {
    const resp = await get("/admin/config");
    if (resp.forbidden) { forbidden.value = true; return; }
    entries.value = resp.entries ?? [];
    for (const e of entries.value) {
      rowValues.value[e.key] = e.value;
    }
  } finally {
    loading.value = false;
  }
});

async function save(key) {
  rowSuccess.value[key] = false;
  rowErrors.value[key]  = "";
  try {
    await put("/admin/config", { body: { key, value: rowValues.value[key] } });
    rowSuccess.value[key] = true;
  } catch (err) {
    rowErrors.value[key] = String(err).replace(/^Error:\s*/, "");
  }
}
</script>

<template>
  <div class="view">
    <div class="view-header">
      <Icon name="settings" class="view-icon" />
      <h1>General configuration</h1>
    </div>

    <!-- not-an-admin empty-state -->
    <EmptyState v-if="forbidden" data-testid="forbidden" icon="admin" message="You are not a tenant admin." />

    <div v-else-if="loading" class="loading-row"><Spinner size="md" /></div>

    <template v-else>
      <div class="config-table">
        <div
          v-for="entry in entries"
          :key="entry.key"
          data-testid="config-row"
          class="config-row"
        >
          <div class="row-meta">
            <span class="row-key mono">{{ entry.key }}</span>
            <span class="row-kind muted">{{ entry.kind }}</span>
            <span class="row-default muted">default: {{ entry.defaultValue }}</span>
          </div>
          <p v-if="entry.doc" class="row-doc muted">{{ entry.doc }}</p>
          <div class="row-edit">
            <input
              data-testid="config-value-input"
              type="text"
              class="form-input"
              v-model="rowValues[entry.key]"
            />
            <button
              data-testid="config-save-btn"
              class="btn-primary"
              @click="save(entry.key)"
            >
              Save
            </button>
            <span
              v-if="rowSuccess[entry.key]"
              data-testid="config-row-success"
              class="row-success"
            >Saved</span>
          </div>
          <p
            v-if="rowErrors[entry.key]"
            data-testid="config-row-error"
            class="row-error"
          >{{ rowErrors[entry.key] }}</p>
        </div>
      </div>
    </template>
  </div>
</template>

<style scoped>
.view { padding: 24px 32px; max-width: 760px; }
.view-header { display: flex; align-items: center; gap: 10px; margin-bottom: 20px; }
.view-header h1 { margin: 0; font-size: 20px; font-weight: 600; }
.view-icon { color: var(--text-muted); width: 22px; height: 22px; }

.empty-state {
  display: flex; flex-direction: column; align-items: center; gap: 12px;
  padding: 60px 0; color: var(--text-muted);
}
.empty-icon { width: 40px; height: 40px; }

.config-table { display: flex; flex-direction: column; gap: 16px; }

.config-row {
  background: var(--bg-elevated); border: 1px solid var(--border);
  border-radius: var(--radius); padding: 16px 20px;
  display: flex; flex-direction: column; gap: 8px;
}

.row-meta { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; }
.row-key  { font-size: 14px; font-weight: 500; }
.row-kind  { font-size: 11px; }
.row-default { font-size: 11px; }
.row-doc  { margin: 0; font-size: 13px; }

.row-edit { display: flex; align-items: center; gap: 10px; }

.form-input {
  background: var(--bg); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 6px 9px; font-size: 13px; min-width: 160px;
}

.btn-primary {
  display: inline-flex; align-items: center;
  background: var(--accent); color: var(--text-on-accent);
  border: none; border-radius: var(--radius-sm);
  padding: 6px 14px; font-size: 13px; cursor: pointer;
}
.btn-primary:disabled { opacity: 0.5; cursor: default; }

.row-success { font-size: 12px; color: var(--ok); }
.row-error   { margin: 0; font-size: 12px; color: var(--danger); }

.mono  { font-family: var(--font-mono); }
.muted { color: var(--text-muted); }
.loading-row { display: flex; padding: 32px 0; }
</style>
