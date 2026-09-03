<script setup>
import { ref, computed, onMounted } from "vue";
import { listAssignments, assignRole, revokeRole, listMcpConnections, listAgents } from "../../api/admin.js";
import { SECTIONS, subjectRef, principalKind } from "../../sections.js";
import { filterTuples, filterPrincipals } from "../../utils/svc-filter.js";
import Icon from "../../components/Icon.vue";
import DataTable from "../../components/ui/DataTable.vue";
import EmptyState from "../../components/ui/EmptyState.vue";
import Modal from "../../components/ui/Modal.vue";
import FormField from "../../components/ui/FormField.vue";
import { useToast } from "../../components/ui/useToast.js";

const { push: toast } = useToast();

const tuples      = ref([]);
const principals  = ref([]);
const fgaEnabled  = ref(true);
const warnings    = ref([]);
const forbidden   = ref(false);
const loading     = ref(false);
const error       = ref("");

// MCP connection lookup: uuid → name
const mcpConnections = ref([]);
const mcpNameByUuid  = computed(() => {
  const m = {};
  for (const c of mcpConnections.value) m[c.id] = c.name;
  return m;
});

// Agent lookup: uuid → name
const agents        = ref([]);
const agentNameByUuid = computed(() => {
  const m = {};
  for (const a of agents.value) m[a.id] = a.name;
  return m;
});

// Principal display-name lookup: full ref (user:<sub>) → displayName
const displayNameById = computed(() => {
  const m = {};
  for (const p of principals.value) if (p.displayName) m[p.id] = p.displayName;
  return m;
});

// dialog state
const dialogSection = ref(null);   // section object or null (closed)
const dlgRelation   = ref("");
const dlgObject     = ref("");
const dlgSubject    = ref("");
const dlgError      = ref("");

async function loadMcpConnections() {
  try {
    const resp = await listMcpConnections();
    mcpConnections.value = resp?.connections ?? [];
  } catch (_) {
    // forbidden or unavailable — display falls back to short uuid
  }
}

async function loadAgents() {
  try {
    const resp = await listAgents();
    if (resp?.forbidden) return;
    agents.value = resp?.agents ?? [];
  } catch (_) {
    // forbidden or unavailable — display falls back to short uuid
  }
}

async function load() {
  loading.value  = true;
  error.value    = "";
  forbidden.value = false;
  try {
    const [data] = await Promise.all([
      listAssignments(),
      loadMcpConnections(),
      loadAgents(),
    ]);
    if (data.forbidden) { forbidden.value = true; return; }
    tuples.value     = data.tuples     ?? [];
    principals.value = data.principals ?? [];
    fgaEnabled.value = data.fgaEnabled ?? true;
    warnings.value   = data.warnings   ?? [];
  } catch (e) {
    error.value = e.message;
  } finally {
    loading.value = false;
  }
}

onMounted(load);

// ── Filtering tuples by section ───────────────────────────────────────────────

function tuplesForSection(sec) {
  const sectionRows = tuples.value.filter((t) =>
    t.object?.startsWith(sec.objectType + ":"),
  );
  // filterTuples removes service-principal rows: a `user:svc-` subject or
  // `group:svc-` object must not be hand-managed by an admin.
  return filterTuples(sectionRows);
}

// ── Assign dialog ─────────────────────────────────────────────────────────────

function openDialog(sec) {
  dialogSection.value = sec;
  dlgRelation.value   = sec.relations[0].name;
  dlgObject.value     = "";
  dlgSubject.value    = "";
  dlgError.value      = "";
}

function closeDialog() { dialogSection.value = null; }

const currentRel = computed(() =>
  dialogSection.value?.relations.find((r) => r.name === dlgRelation.value),
);

const subjectOptions = computed(() => {
  const kindMatched = principals.value.filter(
    (p) => currentRel.value?.subjects.includes(p.kind),
  );
  // filterPrincipals removes service principals — they must not be hand-assigned;
  // their authority derives solely from the API-key trust path.
  return filterPrincipals(kindMatched);
});

async function submitAssign() {
  dlgError.value = "";
  if (!dlgObject.value || !dlgSubject.value) {
    dlgError.value = "object and subject are required";
    return;
  }
  const kind = principalKind(dlgSubject.value);
  if (!currentRel.value?.subjects.includes(kind)) {
    dlgError.value = `${dlgRelation.value} cannot be granted to a ${kind || "?"}`;
    return;
  }
  try {
    await assignRole({
      user:     subjectRef(dlgSubject.value),
      relation: dlgRelation.value,
      object:   dlgObject.value,
      section:  dialogSection.value?.id ?? 0,
    });
    closeDialog();
    toast("ok", "Assignment saved.");
    await load();
  } catch (e) {
    dlgError.value = e.message;
  }
}

async function revoke(tuple) {
  try {
    await revokeRole(tuple);
    toast("ok", "Assignment revoked.");
    await load();
  } catch (e) {
    error.value = e.message;
  }
}

function shortObj(o) {
  const i = o.indexOf(":");
  return i < 0 ? o : o.slice(i + 1);
}

function objectLabel(o) {
  if (o.startsWith("mcp_connector:")) {
    const uuid = o.slice("mcp_connector:".length);
    return mcpNameByUuid.value[uuid] ?? uuid;
  }
  if (o.startsWith("agent:")) {
    const uuid = o.slice("agent:".length);
    return agentNameByUuid.value[uuid] ?? uuid;
  }
  return shortObj(o);
}

// Returns true when objectLabel resolved to a human name different from the raw ref.
function objectLabelDiffers(o) {
  return objectLabel(o) !== shortObj(o);
}

function sectionTableCols(sec) {
  return [
    { key: "user",     label: "Subject" },
    { key: "relation", label: "Relation" },
    { key: "object",   label: sec.objectColumnLabel ?? "Object" },
    { key: "_actions", label: "",        width: "90px" },
  ];
}
</script>

<template>
  <div class="view">
    <div class="view-header">
      <Icon name="admin" class="view-icon" />
      <h1>Roles &amp; Assignments</h1>
    </div>

    <!-- not-an-admin empty-state -->
    <EmptyState
      v-if="forbidden"
      data-testid="forbidden"
      icon="admin"
      message="You are not a tenant admin."
    />

    <template v-else>
      <!-- dev allow-all banner -->
      <div v-if="!fgaEnabled" data-testid="fga-disabled-banner" class="banner-warn">
        OpenFGA is disabled — dev allow-all mode. Role assignments are not enforced.
      </div>

      <!-- warnings -->
      <div v-for="(w, i) in warnings" :key="i" class="banner-warn small">{{ w }}</div>

      <!-- error -->
      <div v-if="error" data-testid="error-banner" class="banner-err">{{ error }}</div>

      <!-- sections -->
      <div v-for="(sec, si) in SECTIONS" :key="sec.id" class="section">
        <div class="section-header">
          <span class="section-title">{{ sec.label }}</span>
          <span class="section-blurb">{{ sec.blurb }}</span>
          <button
            :data-testid="`assign-btn-${si}`"
            class="btn-primary"
            @click="openDialog(sec)"
          >
            <Icon name="plus" /> Assign
          </button>
        </div>

        <DataTable
          :columns="sectionTableCols(sec)"
          :rows="tuplesForSection(sec)"
          :loading="loading && tuplesForSection(sec).length === 0"
          empty-text="No assignments."
          :row-attrs="{ 'data-testid': 'tuple-row' }"
        >
          <template #row="{ row, index }">
            <td
              class="mono"
              :title="displayNameById[row.user] && displayNameById[row.user] !== row.user ? row.user : undefined"
            >{{ displayNameById[row.user] ?? row.user }}</td>
            <td><span class="rel-badge">{{ row.relation }}</span></td>
            <td
              class="mono"
              :title="objectLabelDiffers(row.object) ? row.object : undefined"
            >{{ objectLabel(row.object) }}</td>
            <td class="right">
              <button
                :data-testid="`revoke-${index}`"
                class="btn-danger-sm"
                @click="revoke(row)"
              >
                <Icon name="trash" /> Revoke
              </button>
            </td>
          </template>
        </DataTable>
      </div>
    </template>

    <!-- Assign dialog -->
    <Modal
      :open="!!dialogSection"
      :title="dialogSection ? `Assign — ${dialogSection.label}` : ''"
      @close="closeDialog"
    >
      <div v-if="dialogSection" data-testid="assign-dialog">
        <p class="hint">{{ dialogSection.blurb }}</p>

        <FormField label="Relation">
          <select v-model="dlgRelation">
            <option v-for="r in dialogSection.relations" :key="r.name" :value="r.name">{{ r.name }}</option>
          </select>
        </FormField>

        <FormField :label="`Object (${dialogSection.objectType}:…)`">
          <select
            v-if="dialogSection.objectType === 'mcp_connector'"
            data-testid="dialog-object-select"
            v-model="dlgObject"
          >
            <option value="" disabled>Select a connection…</option>
            <option
              v-for="c in mcpConnections"
              :key="c.id"
              :value="`mcp_connector:${c.id}`"
            >{{ c.name }}</option>
          </select>
          <input
            v-else
            data-testid="dialog-object"
            v-model="dlgObject"
            :placeholder="`${dialogSection.objectType}:…`"
          />
        </FormField>

        <FormField :label="`Subject (${currentRel?.subjects.join(' / ')})`" :error="dlgError">
          <input
            data-testid="dialog-subject"
            v-model="dlgSubject"
            list="subj-list"
            placeholder="user:alice@example.com"
          />
          <datalist id="subj-list">
            <option v-for="p in subjectOptions" :key="p.id" :value="p.id">{{ p.displayName }}</option>
          </datalist>
        </FormField>
      </div>

      <template #footer>
        <button class="btn-ghost" @click="closeDialog">Cancel</button>
        <button data-testid="dialog-submit" class="btn-primary" @click="submitAssign">Assign</button>
      </template>
    </Modal>
  </div>
</template>

<style scoped>
.view { padding: 24px 32px; max-width: 900px; }
.view-header { display: flex; align-items: center; gap: 10px; margin-bottom: 24px; }
.view-header h1 { margin: 0; font-size: 20px; font-weight: 600; }
.view-icon { color: var(--text-muted); width: 22px; height: 22px; }

.banner-warn {
  background: var(--fill-accent); border: 1px solid var(--accent);
  border-radius: var(--radius-sm); padding: 10px 14px;
  color: var(--accent); font-size: 13px; margin-bottom: 16px;
}
.banner-warn.small { font-size: 12px; padding: 6px 10px; }
.banner-err {
  background: var(--fill-danger); border: 1px solid var(--danger);
  border-radius: var(--radius-sm); padding: 10px 14px;
  color: var(--danger); font-size: 13px; margin-bottom: 16px;
}

.section { margin-bottom: 32px; }
.section-header {
  display: flex; align-items: center; gap: 10px; margin-bottom: 8px;
}
.section-title { font-size: 15px; font-weight: 600; }
.section-blurb { color: var(--text-muted); font-size: 12px; flex: 1; }

.mono { font-family: var(--font-mono); word-break: break-all; }
.rel-badge {
  background: var(--fill-muted); color: #a6ffa1;
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 1px 7px; font-size: 12px;
}
.right { text-align: right; }

/* ── buttons ─────────────────────────────────────────────────────────── */
.btn-primary {
  display: inline-flex; align-items: center; gap: 4px;
  background: var(--accent); color: var(--text-on-accent);
  border: none; border-radius: var(--radius-sm);
  padding: 6px 12px; font-size: 13px; cursor: pointer;
}
.btn-primary:hover { background: var(--accent-hover); }
.btn-ghost {
  background: transparent; color: var(--text); border: 1px solid var(--border);
  border-radius: var(--radius-sm); padding: 6px 12px; font-size: 13px; cursor: pointer;
}
.btn-ghost:hover { background: var(--bg-hover); }
.btn-danger-sm {
  display: inline-flex; align-items: center; gap: 4px;
  background: transparent; color: var(--danger);
  border: 1px solid var(--danger); border-radius: var(--radius-sm);
  padding: 3px 10px; cursor: pointer; font-size: 12px;
  opacity: 0.8;
}
.btn-danger-sm:hover { background: var(--fill-danger); opacity: 1; }

/* ── dialog body ─────────────────────────────────────────────────────── */
.hint { color: var(--text-muted); font-size: 12px; margin: 0 0 16px; }
select, input {
  width: 100%; box-sizing: border-box;
  background: var(--bg); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 7px 9px; font-size: 13px; font-family: var(--font-mono);
}
</style>
