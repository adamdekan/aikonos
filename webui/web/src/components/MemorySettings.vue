<script setup>
// Settings-modal Memory pane. Three scope tabs over one list→detail→actions flow.
//
// Authorization is the broker's: every route 403s a caller outside the matrix.
// `canManage` only decides whether to *offer* an action — the `manager` flag on
// a group is exactly what ListMemoryGroups returns it for — and a 403 that slips
// through anyway flips manageDenied so the buttons stop being offered.
import { computed, onMounted, ref, watch } from "vue";
import ErrorBanner from "./ui/ErrorBanner.vue";
import { useToast } from "./ui/useToast.js";
import { useUserStore } from "../store/user.js";
import { listAgents } from "../api/admin.js";
import {
  listMemoryGroups,
  listMemoryConcepts,
  getMemoryConcept,
  verifyMemoryConcept,
  deprecateMemoryConcept,
  deleteMemoryConcept,
} from "../api/memory.js";

const { push: toast } = useToast();
const userStore = useUserStore();

const scope   = ref("user");
const groups  = ref([]);
const agents  = ref([]);
const groupId = ref("");
const agentId = ref("");

const concepts  = ref([]);
const loading   = ref(false);
const loadError = ref("");
const forbidden = ref(false);

const selected     = ref(null); // { meta, body }
const busy         = ref(false);
const manageDenied = ref(false);

function scopeArgs() {
  return {
    scope: scope.value,
    ...(scope.value === "group" ? { groupId: groupId.value } : {}),
    ...(scope.value === "agent" ? { agentId: agentId.value } : {}),
  };
}

// String key so the watcher fires on a real scope-instance change, not on every
// recomputation of an object literal.
const scopeKey = computed(() =>
  [scope.value, scope.value === "group" ? groupId.value : "", scope.value === "agent" ? agentId.value : ""].join("|"),
);

const activeGroup = computed(() => groups.value.find((g) => g.groupId === groupId.value));

const canManage = computed(() => {
  if (manageDenied.value) return false;
  if (scope.value === "group") return !!activeGroup.value?.manager || userStore.isAdmin;
  return true;
});

async function loadGroups() {
  try {
    const data = await listMemoryGroups();
    groups.value = data.groups ?? [];
    if (!groupId.value && groups.value.length > 0) groupId.value = groups.value[0].groupId;
  } catch {
    // Discovery only — a groups-listing failure hides the tab, it is not a
    // reason to blank the Personal pane the user came for.
    groups.value = [];
  }
}

async function loadAgents() {
  try {
    const data = await listAgents();
    agents.value = data.agents ?? [];
    if (!agentId.value && agents.value.length > 0) agentId.value = agents.value[0].id;
  } catch {
    agents.value = [];
  }
}

async function loadConcepts() {
  if (scope.value === "group" && !groupId.value) { concepts.value = []; return; }
  if (scope.value === "agent" && !agentId.value) { concepts.value = []; return; }

  loading.value   = true;
  loadError.value = "";
  forbidden.value = false;
  try {
    const data = await listMemoryConcepts(scopeArgs());
    if (data.forbidden) { forbidden.value = true; concepts.value = []; return; }
    concepts.value = data.concepts ?? [];
  } catch (e) {
    loadError.value = e.message;
    concepts.value  = [];
  } finally {
    loading.value = false;
  }
}

async function openConcept(id) {
  try {
    const data = await getMemoryConcept({ ...scopeArgs(), id });
    if (data.forbidden) { forbidden.value = true; return; }
    selected.value = { meta: data.meta ?? { id }, body: data.body ?? "" };
  } catch (e) {
    toast("error", e.message);
  }
}

// One action wrapper: mutate, then re-read what the mutation changed.
async function runAction(apiFn, okMsg, { keepOpen }) {
  if (!selected.value || busy.value) return;
  const id = selected.value.meta.id;
  busy.value = true;
  try {
    const res = await apiFn({ ...scopeArgs(), id });
    if (res?.forbidden) {
      manageDenied.value = true;
      toast("error", res.error || "You are not permitted to manage this bundle.");
      return;
    }
    toast("ok", okMsg);
    if (keepOpen) await openConcept(id);
    else selected.value = null;
    await loadConcepts();
  } catch (e) {
    toast("error", e.message);
  } finally {
    busy.value = false;
  }
}

const verify    = () => runAction(verifyMemoryConcept, "Concept verified.", { keepOpen: true });
const deprecate = () => runAction(deprecateMemoryConcept, "Concept deprecated.", { keepOpen: true });

function remove() {
  if (!selected.value) return;
  // Hard delete is the management-only escape hatch (OKF prefers deprecation).
  if (!window.confirm(`Delete memory concept "${selected.value.meta.id}"? This cannot be undone.`)) return;
  return runAction(deleteMemoryConcept, "Concept deleted.", { keepOpen: false });
}

function setScope(next) {
  scope.value = next;
}

watch(scopeKey, () => {
  selected.value     = null;
  manageDenied.value = false;
  loadConcepts();
});

onMounted(async () => {
  await loadGroups();
  if (userStore.isAdmin) await loadAgents();
  await loadConcepts();
});
</script>

<template>
  <div class="memory-pane">
    <nav class="scope-tabs" aria-label="Memory scopes">
      <button
        type="button"
        class="scope-tab"
        :class="{ active: scope === 'user' }"
        data-testid="memory-scope-user"
        @click="setScope('user')"
      >
        Personal
      </button>
      <button
        v-if="groups.length > 0"
        type="button"
        class="scope-tab"
        :class="{ active: scope === 'group' }"
        data-testid="memory-scope-group"
        @click="setScope('group')"
      >
        Groups
      </button>
      <button
        v-if="userStore.isAdmin"
        type="button"
        class="scope-tab"
        :class="{ active: scope === 'agent' }"
        data-testid="memory-scope-agent"
        @click="setScope('agent')"
      >
        Agents
      </button>
    </nav>

    <div v-if="scope === 'group'" class="instance-row">
      <select v-model="groupId" data-testid="memory-group-select">
        <option v-for="g in groups" :key="g.groupId" :value="g.groupId">{{ g.groupId }}</option>
      </select>
      <span v-if="activeGroup?.manager" class="badge badge-manager">manager</span>
    </div>

    <div v-else-if="scope === 'agent'" class="instance-row">
      <select v-model="agentId" data-testid="memory-agent-select">
        <option v-for="a in agents" :key="a.id" :value="a.id">{{ a.name || a.id }}</option>
      </select>
    </div>

    <ErrorBanner v-if="loadError" :message="loadError">
      <template #action>
        <button class="btn-ghost btn-sm" data-testid="memory-retry" @click="loadConcepts">Retry</button>
      </template>
    </ErrorBanner>

    <p v-else-if="forbidden" class="hint" data-testid="memory-forbidden">
      You do not have access to this memory bundle.
    </p>

    <p v-else-if="loading" class="hint">Loading…</p>

    <p v-else-if="concepts.length === 0" class="hint">
      No memory concepts in this bundle yet. Agents write them with the memory tool.
    </p>

    <ul v-else class="concept-list">
      <li v-for="c in concepts" :key="c.id">
        <button
          type="button"
          class="concept-row"
          :class="{ deprecated: c.status === 'deprecated', active: selected?.meta?.id === c.id }"
          data-testid="memory-concept-row"
          @click="openConcept(c.id)"
        >
          <span class="concept-title">{{ c.title || c.id }}</span>
          <span class="concept-badges">
            <span class="badge">{{ c.type }}</span>
            <span class="badge">{{ c.status }}</span>
            <span class="badge">{{ c.trustTier }}</span>
            <span v-if="c.stale" class="badge badge-stale">stale</span>
          </span>
        </button>
      </li>
    </ul>

    <div v-if="selected" class="concept-detail" data-testid="memory-detail">
      <dl class="detail-fields">
        <dt>id</dt><dd class="mono">{{ selected.meta.id }}</dd>
        <dt>type</dt><dd>{{ selected.meta.type }}</dd>
        <dt>status</dt><dd>{{ selected.meta.status }}</dd>
        <dt>trust</dt><dd>{{ selected.meta.trustTier }}</dd>
        <dt v-if="selected.meta.description">description</dt>
        <dd v-if="selected.meta.description">{{ selected.meta.description }}</dd>
        <dt v-if="selected.meta.tags?.length">tags</dt>
        <dd v-if="selected.meta.tags?.length">{{ selected.meta.tags.join(", ") }}</dd>
        <dt v-if="selected.meta.staleAfter">stale after</dt>
        <dd v-if="selected.meta.staleAfter">{{ selected.meta.staleAfter }}</dd>
        <dt v-if="selected.meta.generatedBy">generated by</dt>
        <dd v-if="selected.meta.generatedBy" class="mono">{{ selected.meta.generatedBy }}</dd>
      </dl>

      <pre class="detail-body" data-testid="memory-detail-body">{{ selected.body }}</pre>

      <div v-if="canManage" class="detail-actions">
        <button class="btn-ghost btn-sm" :disabled="busy" data-testid="memory-verify-btn" @click="verify">Verify</button>
        <button class="btn-ghost btn-sm" :disabled="busy" data-testid="memory-deprecate-btn" @click="deprecate">Deprecate</button>
        <button class="btn-ghost btn-sm danger" :disabled="busy" data-testid="memory-delete-btn" @click="remove">Delete</button>
      </div>
      <p v-else class="hint" data-testid="memory-manage-hint">
        Only a group manager or a tenant admin can verify, deprecate, or delete
        concepts in this bundle.
      </p>
    </div>
  </div>
</template>

<style scoped>
.memory-pane {
  display: flex;
  flex-direction: column;
  gap: var(--space-3);
  max-height: 420px;
  overflow-y: auto;
}

.scope-tabs {
  display: flex;
  gap: 4px;
  border-bottom: 1px solid var(--border);
}

.scope-tab {
  background: none;
  border: none;
  border-bottom: 2px solid transparent;
  color: var(--text-muted);
  font-size: 13px;
  padding: 6px 10px;
  cursor: pointer;
}

.scope-tab.active {
  color: var(--text);
  border-bottom-color: var(--accent);
}

.instance-row {
  display: flex;
  align-items: center;
  gap: 8px;
}

.instance-row select {
  background: var(--bg-input, var(--bg));
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  color: var(--text);
  font-size: 13px;
  padding: 5px 8px;
}

.concept-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.concept-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  width: 100%;
  text-align: left;
  background: none;
  border: 1px solid transparent;
  border-radius: var(--radius-sm);
  color: var(--text);
  font-size: 13px;
  padding: 6px 8px;
  cursor: pointer;
}

.concept-row:hover {
  background: var(--bg-hover);
}

.concept-row.active {
  background: var(--bg-active);
  border-color: var(--border);
}

.concept-row.deprecated {
  opacity: 0.55;
}

.concept-title {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.concept-badges {
  display: flex;
  gap: 4px;
  flex-shrink: 0;
}

.badge {
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--fill-muted);
  color: var(--text-muted);
  font-size: 11px;
  padding: 1px 6px;
}

.badge-stale {
  border-color: var(--accent);
  color: var(--accent);
}

.badge-manager {
  border-color: var(--ok, var(--accent));
  color: var(--ok, var(--accent));
}

.concept-detail {
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  padding: 10px 12px;
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.detail-fields {
  display: grid;
  grid-template-columns: 110px 1fr;
  gap: 2px 8px;
  margin: 0;
  font-size: 12px;
}

.detail-fields dt {
  color: var(--text-muted);
}

.detail-fields dd {
  margin: 0;
  color: var(--text);
  word-break: break-word;
}

.detail-body {
  margin: 0;
  max-height: 180px;
  overflow: auto;
  background: var(--fill-muted);
  border-radius: var(--radius-sm);
  padding: 8px;
  font-family: var(--font-mono);
  font-size: 12px;
  white-space: pre-wrap;
  word-break: break-word;
}

.detail-actions {
  display: flex;
  gap: 6px;
}

.mono {
  font-family: var(--font-mono);
}

.hint {
  margin: 0;
  font-size: 12px;
  color: var(--text-muted);
}

.btn-ghost {
  background: transparent;
  color: var(--text);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  padding: 5px 10px;
  font-size: 12px;
  cursor: pointer;
}

.btn-ghost:hover:not(:disabled) {
  background: var(--bg-hover);
}

.btn-ghost:disabled {
  opacity: 0.5;
  cursor: default;
}

.btn-ghost.danger {
  color: var(--danger);
  border-color: var(--danger);
}
</style>
