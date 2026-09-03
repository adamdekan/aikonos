<script setup>
import { ref, computed, inject } from "vue";
import { effectiveSkills } from "./access-derive.js";
import { ACCESS_CTX } from "./useAccessControl.js";

const ctx = inject(ACCESS_CTX);
const {
  loading, tuples, principals, derivedGroups, skills,
  skillScope, isRegisteredSkill, userName,
  doAssign, doRevoke, mutating,
} = ctx;

// ── tab-local state ───────────────────────────────────────────────────────────

const selectedSkillId = ref(null);
const skillSearch     = ref("");

// All skill ids: union of registry + any skill referenced in tuples (stale grants)
const allSkillIds = computed(() => {
  const ids = new Set(skills.value.map((s) => s.toolId));
  for (const t of tuples.value) {
    if (t.object.startsWith("skill:")) ids.add(t.object.slice("skill:".length));
    if (t.user.startsWith("skill:"))   ids.add(t.user.slice("skill:".length));
  }
  return [...ids].sort();
});

const filteredSkills = computed(() => {
  const q = skillSearch.value.toLowerCase();
  if (!q) return allSkillIds.value;
  return allSkillIds.value.filter((id) => id.toLowerCase().includes(q));
});

function selectSkill(toolId) {
  selectedSkillId.value = toolId;
}

// Groups that have been granted this skill (via group#member → permitted_group → skill:<id>)
const selectedSkillGroups = computed(() => {
  if (!selectedSkillId.value) return [];
  const obj = `skill:${selectedSkillId.value}`;
  return tuples.value
    .filter((t) => t.object === obj && t.relation === "permitted_group" && t.user.includes("#member"))
    .map((t) => {
      const raw = t.user; // "group:<name>#member"
      const name = raw.replace(/^group:/, "").replace(/#member$/, "");
      return name;
    })
    .sort();
});

// Users with effective access to this skill, with provenance
const selectedSkillUsers = computed(() => {
  if (!selectedSkillId.value) return [];
  const sid = selectedSkillId.value;
  // For each user principal, check if they have effective access to this skill
  const result = [];
  for (const p of principals.value.filter((p) => p.kind === "user" || p.id.startsWith("user:"))) {
    const eff = effectiveSkills(p.id, tuples.value);
    const match = eff.find((s) => s.skillId === sid);
    if (match) result.push({ userRef: p.id, provenance: match.provenance });
  }
  return result.sort((a, b) => a.userRef < b.userRef ? -1 : 1);
});

// Groups not yet granted this skill (for the grant-to-group dropdown)
const skillGrantDropOptions = computed(() => {
  if (!selectedSkillId.value) return [];
  const granted = new Set(selectedSkillGroups.value);
  return derivedGroups.value.filter((g) => !granted.has(g.name));
});

async function grantSkillToGroup(groupName) {
  if (!selectedSkillId.value) return;
  await doAssign(`group:${groupName}#member`, "permitted_group", `skill:${selectedSkillId.value}`);
}

async function revokeSkillFromGroup(groupName) {
  if (!selectedSkillId.value) return;
  await doRevoke(`group:${groupName}#member`, "permitted_group", `skill:${selectedSkillId.value}`);
}
</script>

<template>
  <div class="master-detail" data-testid="pane-Tools">
    <div class="master-pane">
      <input
        v-model="skillSearch"
        class="search-input"
        placeholder="Search tools…"
        data-testid="skill-search"
      />
      <div v-if="loading" class="list-loading">Loading…</div>
      <div
        v-for="toolId in filteredSkills"
        :key="toolId"
        :class="['list-item', { active: selectedSkillId === toolId }]"
        :data-testid="`skill-row-${toolId}`"
        @click="selectSkill(toolId)"
      >
        <span class="item-name mono">{{ toolId }}</span>
        <span v-if="skillScope(toolId)" class="item-sub">{{ skillScope(toolId) }}</span>
        <span v-if="!isRegisteredSkill(toolId)" class="item-sub warn-text">(unregistered)</span>
      </div>
      <div v-if="!loading && filteredSkills.length === 0" class="list-empty">No tools.</div>
    </div>

    <div class="detail-pane" data-testid="tool-detail-pane">
      <div v-if="!selectedSkillId" class="detail-empty">Select a tool to view details.</div>

      <template v-else>
        <div class="detail-section">
          <div class="detail-title mono">{{ selectedSkillId }}</div>
          <div class="detail-meta">
            <span v-if="skillScope(selectedSkillId)" class="meta-item">
              scope: <span class="mono">{{ skillScope(selectedSkillId) }}</span>
            </span>
            <span v-if="!isRegisteredSkill(selectedSkillId)" class="meta-item warn-text">
              Stale grant — tool not in registry
            </span>
          </div>
        </div>

        <!-- Groups that can invoke -->
        <div class="detail-section">
          <div class="section-label">Groups that can invoke</div>
          <div class="chips-row">
            <span
              v-for="gname in selectedSkillGroups"
              :key="gname"
              class="chip chip-skill"
              :data-testid="`skill-group-chip-${gname}`"
            >
              {{ gname }}
              <button
                class="chip-x"
                :disabled="mutating"
                @click="revokeSkillFromGroup(gname)"
                title="Revoke from group"
                :data-testid="`revoke-skill-group-${gname}`"
              >×</button>
            </span>
            <span v-if="selectedSkillGroups.length === 0" class="no-items">No groups granted.</span>
          </div>
          <div class="add-row">
            <select
              class="dropdown"
              :disabled="mutating"
              @change="(e) => { if (e.target.value) { grantSkillToGroup(e.target.value); e.target.value = ''; } }"
              data-testid="grant-skill-to-group-select"
            >
              <option value="">Grant to group…</option>
              <option v-for="g in skillGrantDropOptions" :key="g.name" :value="g.name" :data-testid="`grant-skill-opt-${g.name}`">
                {{ g.name }}
              </option>
            </select>
          </div>
        </div>

        <!-- Users with access (derived) -->
        <div class="detail-section">
          <div class="section-label">Users with access</div>
          <div class="hint small" style="margin-bottom:8px;">
            Access flows through group membership. Direct user grants are shown if present in the store.
          </div>
          <table v-if="selectedSkillUsers.length > 0" class="skills-table">
            <thead>
              <tr>
                <th>User</th>
                <th>Provenance</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="entry in selectedSkillUsers" :key="entry.userRef">
                <td>{{ userName(entry.userRef) }}</td>
                <td>
                  <span
                    v-for="(prov, pi) in entry.provenance"
                    :key="pi"
                    :class="['prov-badge', prov.kind === 'direct' ? 'prov-direct' : 'prov-group']"
                  >
                    {{ prov.kind === "direct" ? "direct" : `via ${prov.group}` }}
                  </span>
                </td>
              </tr>
            </tbody>
          </table>
          <div v-else class="no-items">No users have access to this tool.</div>
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

.detail-section { margin-bottom: 22px; }
.section-label  { font-size: 12px; font-weight: 600; text-transform: uppercase; letter-spacing: 0.06em; color: var(--text-muted); margin-bottom: 8px; }

.hint       { color: var(--text-muted); font-size: 12px; }
.hint.small { font-size: 11px; }

/* ── chips ─────────────────────────────────────────────────────────── */
.chips-row { display: flex; flex-wrap: wrap; gap: 6px; margin-bottom: 8px; }
.chip {
  display: inline-flex; align-items: center; gap: 4px;
  background: var(--fill-muted); color: var(--text);
  border: 1px solid var(--border); border-radius: 12px;
  padding: 2px 10px; font-size: 12px;
}
.chip-skill  { border-color: #a6ffa1; color: #a6ffa1; background: var(--fill-muted); }
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

/* ── skills table ──────────────────────────────────────────────────── */
.skills-table { width: 100%; border-collapse: collapse; font-size: 13px; }
.skills-table th { text-align: left; font-size: 11px; font-weight: 600; text-transform: uppercase; letter-spacing: 0.05em; color: var(--text-muted); padding: 4px 8px; border-bottom: 1px solid var(--border); }
.skills-table td { padding: 6px 8px; border-bottom: 1px solid var(--border); vertical-align: middle; }
.skills-table tr:last-child td { border-bottom: none; }
.prov-badge {
  display: inline-block; border-radius: var(--radius-sm); padding: 1px 7px;
  font-size: 11px; margin-right: 4px; border: 1px solid transparent;
}
.prov-direct { background: var(--fill-accent); color: var(--accent); border-color: var(--accent); }
.prov-group  { background: var(--fill-muted);  color: var(--text-muted); border-color: var(--border); }

/* ── scope badge ────────────────────────────────────────────────────── */
.scope-badge {
  display: inline-block; background: var(--fill-muted); color: var(--text-muted);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 0 6px; font-size: 11px; margin-left: 4px; font-family: var(--font-mono);
}

/* ── warn text ───────────────────────────────────────────────────────── */
.warn-text { color: var(--accent); font-size: 11px; }

.mono { font-family: var(--font-mono); }
</style>
