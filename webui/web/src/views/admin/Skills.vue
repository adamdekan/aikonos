<script setup>
import { ref, onMounted } from "vue";
import { listSkills, upsertSkill, deleteSkill } from "../../api/admin.js";
import Icon from "../../components/Icon.vue";
import DataTable from "../../components/ui/DataTable.vue";
import EmptyState from "../../components/ui/EmptyState.vue";
import ToggleSwitch from "../../components/ui/ToggleSwitch.vue";
import { useToast } from "../../components/ui/useToast.js";

const { push: toast } = useToast();

const skills    = ref([]);
const forbidden = ref(false);
const loading   = ref(false);
const error     = ref("");

// edit state: keyed by toolId
const edits = ref({});

// add-custom form state
const customToolId     = ref("");
const customEffectClass = ref("write_external");
const customDescription = ref("");

const EFFECT_CLASSES = [
  "read_internal",
  "write_internal",
  "read_external",
  "write_external",
  "read_pii",
  "write_pii",
  "destructive",
  "send_message",
];

async function load() {
  loading.value   = true;
  error.value     = "";
  forbidden.value = false;
  try {
    const data = await listSkills();
    if (data.forbidden) { forbidden.value = true; return; }
    skills.value = data.skills ?? [];
    // seed edit state from loaded rows
    const map = {};
    for (const s of skills.value) {
      map[s.toolId] = {
        effectClass:  s.effectClass  ?? "",
        description:  s.description  ?? "",
        enabled:      s.enabled      ?? true,
      };
    }
    edits.value = map;
  } catch (e) {
    error.value = e.message;
  } finally {
    loading.value = false;
  }
}

onMounted(load);

function editFor(toolId) {
  if (!edits.value[toolId]) {
    edits.value[toolId] = { effectClass: "", description: "", enabled: true };
  }
  return edits.value[toolId];
}

async function save(skill) {
  error.value = "";
  const e = editFor(skill.toolId);
  try {
    const resp = await upsertSkill(skill.toolId, {
      effectClass:  e.effectClass,
      description:  e.description,
      enabled:      e.enabled,
      displayName:  skill.displayName,
    });
    if (resp.forbidden) {
      error.value = resp.error || "You are not a tenant admin.";
      return;
    }
    if (resp.error) {
      error.value = resp.error;
      return;
    }
    toast("ok", "Skill saved.");
    await load();
  } catch (err) {
    error.value = err.message;
  }
}

async function remove(skill) {
  if (!confirm(`Delete overlay for "${skill.toolId}"? Built-in tools revert to defaults.`)) return;
  error.value = "";
  try {
    const resp = await deleteSkill(skill.toolId);
    if (resp && resp.forbidden) {
      error.value = resp.error || "You are not a tenant admin.";
      return;
    }
    if (resp && resp.error) {
      error.value = resp.error;
      return;
    }
    toast("ok", "Skill overlay deleted.");
    await load();
  } catch (err) {
    error.value = err.message;
  }
}

async function addCustom() {
  const id = customToolId.value.trim();
  if (!id) { error.value = "tool id is required"; return; }
  error.value = "";
  try {
    const resp = await upsertSkill(id, {
      effectClass: customEffectClass.value,
      description: customDescription.value,
      enabled:     true,
    });
    if (resp.forbidden) {
      error.value = resp.error || "You are not a tenant admin.";
      return;
    }
    if (resp.error) {
      error.value = resp.error;
      return;
    }
    customToolId.value      = "";
    customEffectClass.value  = "write_external";
    customDescription.value  = "";
    toast("ok", "Skill registered.");
    await load();
  } catch (err) {
    error.value = err.message;
  }
}

const TABLE_COLS = [
  { key: "toolId",       label: "Skill" },
  { key: "executorKind", label: "Type",         width: "80px" },
  { key: "scope",        label: "Scope",        width: "140px" },
  { key: "effectClass",  label: "Effect class", width: "170px" },
  { key: "enabled",      label: "Enabled",      width: "80px" },
  { key: "description",  label: "Description" },
  { key: "_actions",     label: "",             width: "130px" },
];
</script>

<template>
  <div class="view">
    <div class="view-header">
      <Icon name="tool" class="view-icon" />
      <h1>Tools</h1>
    </div>

    <div class="banner-info">
      Adding or editing a tool changes the vocabulary only. Invocation still requires an
      <strong>Access Control</strong> grant (<code>can_invoke</code>). Scope is derived by the
      broker and cannot be edited. Built-in effect classes may only be made stricter, not weaker.
    </div>

    <EmptyState
      v-if="forbidden"
      data-testid="forbidden"
      icon="admin"
      message="You are not a tenant admin."
    />

    <template v-else>
      <div v-if="error" data-testid="error-banner" class="banner-err">{{ error }}</div>

      <!-- Add custom (MCP) skill form -->
      <div class="form">
        <input
          v-model="customToolId"
          placeholder="mcp:<conn>:<tool>"
          class="tool-id-input"
          data-testid="custom-tool-id"
        />
        <select
          v-model="customEffectClass"
          class="class-select"
          data-testid="custom-effect-class"
        >
          <option v-for="c in EFFECT_CLASSES" :key="c" :value="c">{{ c }}</option>
        </select>
        <input
          v-model="customDescription"
          placeholder="Description (optional)"
          class="desc-input"
          data-testid="custom-description"
        />
        <button class="btn-primary" data-testid="custom-add-btn" @click="addCustom">
          <Icon name="plus" /> Register skill
        </button>
      </div>

      <DataTable
        :columns="TABLE_COLS"
        :rows="skills"
        :loading="loading"
        empty-text="No skills configured. Built-in skills are available by default. Register MCP-backed skills above."
        :row-attrs="{ 'data-testid': 'skill-row' }"
      >
        <template #row="{ row }">
          <td class="mono">{{ row.toolId }}</td>
          <td class="muted">{{ row.executorKind || "—" }}</td>
          <!-- scope: display-only, never an input (I3) -->
          <td class="muted scope-cell">{{ row.scope || "—" }}</td>
          <td>
            <select
              v-model="editFor(row.toolId).effectClass"
              class="class-select"
              :data-testid="`effect-${row.toolId}`"
            >
              <option v-for="c in EFFECT_CLASSES" :key="c" :value="c">{{ c }}</option>
            </select>
          </td>
          <td>
            <ToggleSwitch
              v-model="editFor(row.toolId).enabled"
              :data-testid="`enabled-${row.toolId}`"
              :aria-label="`Enable ${row.toolId}`"
            />
          </td>
          <td>
            <input
              v-model="editFor(row.toolId).description"
              class="desc-input"
              placeholder="Description"
              :data-testid="`desc-${row.toolId}`"
            />
          </td>
          <td class="right">
            <button
              :data-testid="`save-${row.toolId}`"
              class="btn-primary-sm"
              @click="save(row)"
            >
              Save
            </button>
            <button
              :data-testid="`delete-${row.toolId}`"
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
.view { padding: 24px 32px; max-width: 1100px; }
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

.muted { color: var(--text-muted); font-size: 13px; }

.form {
  display: flex; flex-wrap: wrap; gap: 6px;
  margin-bottom: 16px; align-items: center;
}
.form input, .form select {
  background: var(--bg-elevated); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 7px 9px; font-size: 13px;
}
.tool-id-input { width: 220px; }
.class-select  { width: 160px; }
.desc-input    { width: 200px; }

.btn-primary {
  display: inline-flex; align-items: center; gap: 4px;
  background: var(--accent); color: var(--text-on-accent);
  border: none; border-radius: var(--radius-sm);
  padding: 7px 13px; font-size: 13px; cursor: pointer;
}
.btn-primary:hover { background: var(--accent-hover); }

.btn-primary-sm {
  display: inline-flex; align-items: center; gap: 4px;
  background: var(--accent); color: var(--text-on-accent);
  border: none; border-radius: var(--radius-sm);
  padding: 3px 10px; font-size: 12px; cursor: pointer; margin-right: 4px;
}
.btn-primary-sm:hover { background: var(--accent-hover); }

.btn-danger-sm {
  display: inline-flex; align-items: center; gap: 4px;
  background: transparent; color: var(--danger);
  border: 1px solid var(--danger); border-radius: var(--radius-sm);
  padding: 3px 10px; cursor: pointer; font-size: 12px; opacity: 0.8;
}
.btn-danger-sm:hover { background: var(--fill-danger); opacity: 1; }

.mono { font-family: var(--font-mono); word-break: break-all; }
.right { text-align: right; white-space: nowrap; }
.scope-cell { font-family: var(--font-mono); }
</style>
