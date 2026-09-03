<script setup>
import { ref, computed, inject } from "vue";
import { deriveAgent } from "./access-derive.js";
import { ACCESS_CTX } from "./useAccessControl.js";
import { assignRole, revokeRole } from "../../api/admin.js";
import { subjectRef } from "../../sections.js";
import { useToast } from "../../components/ui/useToast.js";

const ctx = inject(ACCESS_CTX);
const {
  loading, tuples, agents, mcpConnections,
  userPrincipals, derivedGroups,
  userName, mutating, mutError,
  doAssign, doRevoke, load,
  selectedAgent,
} = ctx;

const { push: toast } = useToast();

// ── tab-local state ───────────────────────────────────────────────────────────

const agentSearch = ref("");

// ── filtered list ─────────────────────────────────────────────────────────────

const filteredAgents = computed(() => {
  const q = agentSearch.value.toLowerCase();
  if (!q) return agents.value;
  return agents.value.filter(
    (a) => a.name?.toLowerCase().includes(q) || a.id?.toLowerCase().includes(q),
  );
});

function selectAgent(a) {
  selectedAgent.value = a;
}

const selAgentData = computed(() => {
  if (!selectedAgent.value) return null;
  return deriveAgent(`agent:${selectedAgent.value.id}`, tuples.value);
});

// ── owner reassign ────────────────────────────────────────────────────────────

const agentOwnerDropSearch = ref("");
const agentOwnerMutating   = ref(false);

const agentOwnerOptions = computed(() => {
  const q = agentOwnerDropSearch.value.toLowerCase();
  const current = selAgentData.value?.owner;
  return userPrincipals.value
    .filter((p) => p.id !== current)
    .filter((p) => !q || userName(p.id).toLowerCase().includes(q) || p.id.toLowerCase().includes(q));
});

async function reassignAgentOwner(newOwnerRef) {
  if (!selectedAgent.value) return;
  agentOwnerMutating.value = true;
  const agentRef = `agent:${selectedAgent.value.id}`;
  const current  = selAgentData.value?.owner;
  try {
    // Revoke old owner first; abort before assigning if revoke fails (mirrors setTenantRole pattern)
    if (current) {
      await revokeRole({ user: subjectRef(current), relation: "owner_user", object: agentRef });
    }
    await assignRole({ user: subjectRef(newOwnerRef), relation: "owner_user", object: agentRef });
    toast("ok", "Agent owner updated.");
    await load();
    const refreshed = agents.value.find((a) => a.id === selectedAgent.value?.id);
    if (refreshed) selectedAgent.value = refreshed;
  } catch (e) {
    mutError.value = e.message;
  } finally {
    agentOwnerMutating.value = false;
  }
}

// ── usable_by: combined user+group dropdown ───────────────────────────────────

const agentUsableDropSearch = ref("");

const agentUsableDropOptions = computed(() => {
  if (!selectedAgent.value) return [];
  const q = agentUsableDropSearch.value.toLowerCase();
  const existing = new Set(selAgentData.value?.usableBy ?? []);
  // Users
  const users = userPrincipals.value
    .filter((p) => !existing.has(p.id))
    .filter((p) => !q || userName(p.id).toLowerCase().includes(q) || p.id.toLowerCase().includes(q))
    .map((p) => ({ ref: p.id, label: userName(p.id), kind: "user" }));
  // Groups (member-set refs)
  const groups = derivedGroups.value
    .filter((g) => !existing.has(`group:${g.name}#member`))
    .filter((g) => !q || g.name.toLowerCase().includes(q))
    .map((g) => ({ ref: `group:${g.name}#member`, label: `group:${g.name}`, kind: "group" }));
  return [...users, ...groups].sort((a, b) => a.label < b.label ? -1 : 1);
});

async function addAgentUsableBy(subjectRefVal) {
  if (!selectedAgent.value) return;
  await doAssign(subjectRefVal, "usable_by", `agent:${selectedAgent.value.id}`);
}

async function removeAgentUsableBy(subjectRefVal) {
  if (!selectedAgent.value) return;
  await doRevoke(subjectRefVal, "usable_by", `agent:${selectedAgent.value.id}`);
}

// ── MCP connectors permitted ──────────────────────────────────────────────────

const agentMcpDropSearch = ref("");

const agentMcpDropOptions = computed(() => {
  if (!selectedAgent.value) return [];
  const q = agentMcpDropSearch.value.toLowerCase();
  const existing = new Set(selAgentData.value?.mcpConnectors ?? []);
  return mcpConnections.value
    .filter((c) => !existing.has(c.id ?? c.name))
    .filter((c) => !q || (c.name ?? c.id ?? "").toLowerCase().includes(q))
    .map((c) => ({ id: c.id ?? c.name, label: c.name ?? c.id }));
});

async function addAgentMcp(connId) {
  if (!selectedAgent.value) return;
  await doAssign(`agent:${selectedAgent.value.id}`, "permitted_agent", `mcp_connector:${connId}`);
}

async function removeAgentMcp(connId) {
  if (!selectedAgent.value) return;
  await doRevoke(`agent:${selectedAgent.value.id}`, "permitted_agent", `mcp_connector:${connId}`);
}
</script>

<template>
  <div class="master-detail" data-testid="pane-Agents">
    <div class="master-pane">
      <input
        v-model="agentSearch"
        class="search-input"
        placeholder="Search agents…"
        data-testid="agent-search"
      />
      <div v-if="loading" class="list-loading">Loading…</div>
      <div
        v-for="a in filteredAgents"
        :key="a.id"
        :class="['list-item', { active: selectedAgent?.id === a.id }]"
        :data-testid="`agent-row-${a.id}`"
        @click="selectAgent(a)"
      >
        <span class="item-name">{{ a.name ?? a.id }}</span>
        <span class="item-sub mono">{{ a.id }}</span>
      </div>
      <div v-if="!loading && filteredAgents.length === 0" class="list-empty">No agents.</div>
    </div>

    <div class="detail-pane" data-testid="agent-detail-pane">
      <div v-if="!selectedAgent" class="detail-empty">Select an agent to view details.</div>

      <template v-else>
        <div class="detail-section">
          <div class="detail-title">{{ selectedAgent.name ?? selectedAgent.id }}</div>
          <div class="detail-meta">
            <span class="meta-item mono small" :title="selectedAgent.id">{{ selectedAgent.id }}</span>
          </div>
        </div>

        <!-- Owner -->
        <div class="detail-section">
          <div class="section-label">Owner</div>
          <div class="chips-row">
            <span v-if="selAgentData?.owner" class="chip chip-tenant">
              {{ userName(selAgentData.owner) }}
            </span>
            <span v-else class="no-items">No owner assigned.</span>
          </div>
          <div class="add-row">
            <select
              class="dropdown"
              :disabled="mutating || agentOwnerMutating"
              @change="(e) => { if (e.target.value) { reassignAgentOwner(e.target.value); e.target.value = ''; } }"
              data-testid="agent-owner-select"
            >
              <option value="">{{ selAgentData?.owner ? 'Reassign owner…' : 'Assign owner…' }}</option>
              <option v-for="p in agentOwnerOptions" :key="p.id" :value="p.id" :data-testid="`agent-owner-opt-${p.id}`">
                {{ p.displayName ?? p.email ?? p.id }}
              </option>
            </select>
          </div>
        </div>

        <!-- usable_by -->
        <div class="detail-section">
          <div class="section-label">Usable by</div>
          <div class="chips-row">
            <span
              v-for="subj in selAgentData?.usableBy ?? []"
              :key="subj"
              class="chip"
              :title="subj"
            >
              {{ subj.startsWith("user:") ? userName(subj) : subj.replace(/^group:/, "").replace(/#member$/, "") }}
              <button
                class="chip-x"
                :disabled="mutating"
                @click="removeAgentUsableBy(subj)"
                title="Revoke"
              >×</button>
            </span>
            <span v-if="(selAgentData?.usableBy ?? []).length === 0" class="no-items">No subjects.</span>
          </div>
          <div class="add-row">
            <select
              class="dropdown"
              :disabled="mutating"
              @change="(e) => { if (e.target.value) { addAgentUsableBy(e.target.value); e.target.value = ''; } }"
              data-testid="agent-usable-select"
            >
              <option value="">Add user or group…</option>
              <option v-for="opt in agentUsableDropOptions" :key="opt.ref" :value="opt.ref" :data-testid="`agent-usable-opt-${opt.ref}`">
                {{ opt.label }}
              </option>
            </select>
          </div>
        </div>

        <!-- MCP connectors permitted -->
        <div class="detail-section">
          <div class="section-label">MCP connectors permitted</div>
          <div class="chips-row">
            <span
              v-for="connId in selAgentData?.mcpConnectors ?? []"
              :key="connId"
              class="chip"
            >
              {{ connId }}
              <button
                class="chip-x"
                :disabled="mutating"
                @click="removeAgentMcp(connId)"
                title="Revoke"
              >×</button>
            </span>
            <span v-if="(selAgentData?.mcpConnectors ?? []).length === 0" class="no-items">No connectors.</span>
          </div>
          <div class="add-row">
            <select
              class="dropdown"
              :disabled="mutating"
              @change="(e) => { if (e.target.value) { addAgentMcp(e.target.value); e.target.value = ''; } }"
              data-testid="agent-mcp-select"
            >
              <option value="">Add MCP connector…</option>
              <option v-for="opt in agentMcpDropOptions" :key="opt.id" :value="opt.id" :data-testid="`agent-mcp-opt-${opt.id}`">
                {{ opt.label }}
              </option>
            </select>
          </div>
        </div>
      </template>
    </div>
  </div>
</template>

<style scoped>
/* ── master-detail ─────────────────────────────────────────────────── */
.master-detail {
  display: flex; gap: 16px; min-height: 420px;
}
.master-pane {
  width: 240px; flex-shrink: 0;
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  overflow-y: auto; display: flex; flex-direction: column;
}
.search-input {
  width: 100%; box-sizing: border-box;
  background: var(--bg); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 6px 9px; font-size: 13px; margin: 8px;
  width: calc(100% - 16px);
}
.list-loading { padding: 12px; color: var(--text-muted); font-size: 13px; }
.list-empty   { padding: 12px; color: var(--text-muted); font-size: 13px; font-style: italic; }
.list-item {
  padding: 10px 12px; cursor: pointer; border-bottom: 1px solid var(--border);
  transition: background 0.1s;
}
.list-item:last-child { border-bottom: none; }
.list-item:hover { background: var(--bg-hover); }
.list-item.active { background: var(--bg-active); }
.item-name { display: block; font-size: 13px; font-weight: 500; }
.item-sub  { display: block; font-size: 11px; color: var(--text-muted); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; max-width: 200px; }

.detail-pane {
  flex: 1; min-width: 0; border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 20px; overflow-y: auto;
}

@media (max-width: 720px) {
  .master-detail { flex-direction: column; min-height: 0; }
  .master-pane { width: 100%; max-height: 260px; }
  .item-sub { max-width: none; }
  .detail-pane { padding: 14px; }
  .detail-empty { margin-top: 24px; }
}
.detail-empty { color: var(--text-muted); font-size: 14px; font-style: italic; margin-top: 60px; text-align: center; }

.detail-title { font-size: 17px; font-weight: 600; margin-bottom: 4px; }
.detail-meta  { display: flex; flex-wrap: wrap; gap: 8px; margin-bottom: 4px; }
.meta-item { font-size: 12px; color: var(--text-muted); }
.meta-item.mono { font-family: var(--font-mono); font-size: 11px; }
.meta-item.small { font-size: 11px; }

.detail-section { margin-bottom: 22px; }
.section-label  { font-size: 12px; font-weight: 600; text-transform: uppercase; letter-spacing: 0.06em; color: var(--text-muted); margin-bottom: 8px; }

/* ── chips ─────────────────────────────────────────────────────────── */
.chips-row { display: flex; flex-wrap: wrap; gap: 6px; margin-bottom: 8px; }
.chip {
  display: inline-flex; align-items: center; gap: 4px;
  background: var(--fill-muted); color: var(--text);
  border: 1px solid var(--border); border-radius: 12px;
  padding: 2px 10px; font-size: 12px;
}
.chip-tenant { border-color: var(--accent); color: var(--accent); background: var(--fill-accent); }
.chip-x {
  background: none; border: none; color: inherit; cursor: pointer;
  padding: 0 2px; font-size: 14px; line-height: 1; opacity: 0.7;
}
.chip-x:hover:not(:disabled) { opacity: 1; }
.chip-x:disabled { cursor: default; opacity: 0.3; }

.no-items { color: var(--text-muted); font-size: 12px; font-style: italic; }

/* ── add-row ──────────────────────────────────────────────────────── */
.add-row {
  display: flex; gap: 6px; align-items: center; flex-wrap: wrap; margin-top: 6px;
}
.dropdown {
  background: var(--bg); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 5px 9px; font-size: 13px; min-width: 180px; cursor: pointer;
}

.mono { font-family: var(--font-mono); }
</style>
