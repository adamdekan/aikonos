<script setup>
import { ref, computed, inject } from "vue";
import { assignRole } from "../../api/admin.js";
import { subjectRef } from "../../sections.js";
import { ACCESS_CTX } from "./useAccessControl.js";
import { useToast } from "../../components/ui/useToast.js";
import ToggleSwitch from "../../components/ui/ToggleSwitch.vue";

const { push: toast } = useToast();

const ctx = inject(ACCESS_CTX);
const {
  loading, derivedGroups, skills, skillBundles,
  userPrincipals, userName, agentName, bundleName,
  doAssign, doRevoke, mutating,
  load, selectedGroup,
} = ctx;

// ── tab-local state ───────────────────────────────────────────────────────────

const GROUP_NAME_RE        = /^[a-z0-9][a-z0-9._-]{0,63}$/;

const search               = ref("");
const bulkMemberSelected   = ref(new Set());
const bulkMemberError      = ref("");
const bulkSkillSelected    = ref(new Set());
const bulkSkillError       = ref("");

const memberDropSearch     = ref("");
const newGroupNameGrp      = ref("");
const showNewGroupGrp      = ref(false);
const newGroupNameGrpError = ref("");
const skillDropSearch      = ref("");
const bundleDropSearch     = ref("");

// ── filtered group list ───────────────────────────────────────────────────────

const filteredGroups = computed(() => {
  const q = search.value.toLowerCase();
  if (!q) return derivedGroups.value;
  return derivedGroups.value.filter((g) => g.name.toLowerCase().includes(q));
});

function selectGroup(g) {
  selectedGroup.value = g;
}

// ── member drop ───────────────────────────────────────────────────────────────

const memberDropOptions = computed(() => {
  const q = memberDropSearch.value.toLowerCase();
  const existing = new Set(selectedGroup.value?.members ?? []);
  return userPrincipals.value
    .filter((p) => !existing.has(p.id))
    .filter((p) => !q || userName(p.id).toLowerCase().includes(q) || p.id.toLowerCase().includes(q));
});

async function addMemberToGroup(userRef) {
  if (!selectedGroup.value) return;
  await doAssign(userRef, "member", `group:${selectedGroup.value.name}`);
}

async function removeMemberFromGroup(userRef) {
  if (!selectedGroup.value) return;
  await doRevoke(userRef, "member", `group:${selectedGroup.value.name}`);
}

// ── skill drop ────────────────────────────────────────────────────────────────

const skillDropOptions = computed(() => {
  if (!selectedGroup.value) return [];
  const q = skillDropSearch.value.toLowerCase();
  const existing = new Set(selectedGroup.value.skills);
  return skills.value
    .filter((s) => !existing.has(s.toolId))
    .filter((s) => !q || s.toolId.toLowerCase().includes(q));
});

async function addSkillToGroup(toolId) {
  if (!selectedGroup.value) return;
  await doAssign(`group:${selectedGroup.value.name}#member`, "permitted_group", `skill:${toolId}`);
}

async function removeSkillFromGroup(toolId) {
  if (!selectedGroup.value) return;
  await doRevoke(`group:${selectedGroup.value.name}#member`, "permitted_group", `skill:${toolId}`);
}

// ── bundle drop ───────────────────────────────────────────────────────────────

const bundleDropOptions = computed(() => {
  if (!selectedGroup.value) return [];
  const q = bundleDropSearch.value.toLowerCase();
  const existing = new Set(selectedGroup.value.skillBundles ?? []);
  return skillBundles.value
    .filter((b) => !existing.has(b.id))
    .filter((b) => !q || (b.name ?? "").toLowerCase().includes(q) || b.id.toLowerCase().includes(q));
});

async function addBundleToGroup(bundleId) {
  if (!selectedGroup.value) return;
  await doAssign(`group:${selectedGroup.value.name}#member`, "can_use", `agentskill:${bundleId}`);
}

async function removeBundleFromGroup(bundleId) {
  if (!selectedGroup.value) return;
  await doRevoke(`group:${selectedGroup.value.name}#member`, "can_use", `agentskill:${bundleId}`);
}

// ── delegation toggle ─────────────────────────────────────────────────────────

async function setDelegatable(enabled) {
  if (!selectedGroup.value) return;
  const name = selectedGroup.value.name;
  const user = `group:${name}#member`;
  const object = `group:${name}`;
  if (enabled) {
    await doAssign(user, "delegatable", object);
  } else {
    await doRevoke(user, "delegatable", object);
  }
}

// ── manager drop ──────────────────────────────────────────────────────────────

const managerDropOptions = computed(() => {
  if (!selectedGroup.value) return [];
  const existing = new Set(selectedGroup.value.managers ?? []);
  return userPrincipals.value.filter((p) => !existing.has(p.id));
});

async function addManagerToGroup(userRef) {
  if (!selectedGroup.value) return;
  await doAssign(userRef, "manager", `group:${selectedGroup.value.name}`);
}

async function removeManagerFromGroup(userRef) {
  if (!selectedGroup.value) return;
  await doRevoke(userRef, "manager", `group:${selectedGroup.value.name}`);
}

// ── new-group form ────────────────────────────────────────────────────────────

function toggleNewGroupGrp() {
  showNewGroupGrp.value       = !showNewGroupGrp.value;
  newGroupNameGrp.value       = "";
  newGroupNameGrpError.value  = "";
}

async function createNewGroup() {
  newGroupNameGrpError.value = "";
  const n = newGroupNameGrp.value.trim();
  if (!GROUP_NAME_RE.test(n)) {
    newGroupNameGrpError.value = "Name must match ^[a-z0-9][a-z0-9._-]{0,63}$";
    return;
  }
  // Groups are materialized on first assignment (FGA has no empty groups).
  // Set a synthetic selectedGroup so the UI shows the member-add controls immediately.
  // Once the first member is assigned, load() reruns and the name-match lookup below
  // replaces this synthetic object with the real derived entry.
  showNewGroupGrp.value = false;
  // Reuse existing entry if the name already exists in the derived list
  const existing = derivedGroups.value.find((g) => g.name === n);
  if (!existing) {
    selectedGroup.value = { name: n, members: [], managers: [], skills: [], skillBundles: [], agentsUsable: [], delegatable: false };
  } else {
    selectedGroup.value = existing;
  }
}

// ── bulk add members ──────────────────────────────────────────────────────────

function toggleBulkMember(userRef) {
  const s = new Set(bulkMemberSelected.value);
  if (s.has(userRef)) s.delete(userRef); else s.add(userRef);
  bulkMemberSelected.value = s;
}

async function bulkAddMembers() {
  if (!selectedGroup.value || bulkMemberSelected.value.size === 0) return;
  const items = [...bulkMemberSelected.value];
  mutating.value = true;
  bulkMemberError.value = "";
  const failures = [];
  try {
    for (const uRef of items) {
      try {
        await assignRole({ user: subjectRef(uRef), relation: "member", object: `group:${selectedGroup.value.name}` });
      } catch (e) {
        failures.push(`${uRef.replace(/^user:/, "")}: ${e.message}`);
      }
    }
    const added = items.length - failures.length;
    bulkMemberSelected.value = new Set();
    if (failures.length) {
      bulkMemberError.value = `${added} added, ${failures.length} failed: ${failures.join("; ")}`;
    } else {
      toast("ok", `${added} member${added !== 1 ? "s" : ""} added.`);
    }
    await load();
    const refreshed = derivedGroups.value.find((g) => g.name === selectedGroup.value?.name);
    if (refreshed) selectedGroup.value = refreshed;
  } finally {
    mutating.value = false;
  }
}

// ── bulk add skills ───────────────────────────────────────────────────────────

function toggleBulkSkill(toolId) {
  const s = new Set(bulkSkillSelected.value);
  if (s.has(toolId)) s.delete(toolId); else s.add(toolId);
  bulkSkillSelected.value = s;
}

async function bulkAddSkills() {
  if (!selectedGroup.value || bulkSkillSelected.value.size === 0) return;
  const items = [...bulkSkillSelected.value];
  mutating.value = true;
  bulkSkillError.value = "";
  const failures = [];
  try {
    for (const toolId of items) {
      try {
        await assignRole({
          user: subjectRef(`group:${selectedGroup.value.name}#member`),
          relation: "permitted_group",
          object: `skill:${toolId}`,
        });
      } catch (e) {
        failures.push(`${toolId}: ${e.message}`);
      }
    }
    const added = items.length - failures.length;
    bulkSkillSelected.value = new Set();
    if (failures.length) {
      bulkSkillError.value = `${added} added, ${failures.length} failed: ${failures.join("; ")}`;
    } else {
      toast("ok", `${added} tool${added !== 1 ? "s" : ""} granted.`);
    }
    await load();
    const refreshed = derivedGroups.value.find((g) => g.name === selectedGroup.value?.name);
    if (refreshed) selectedGroup.value = refreshed;
  } finally {
    mutating.value = false;
  }
}
</script>

<template>
  <div class="master-detail" data-testid="pane-Groups">
    <div class="master-pane">
      <div class="master-header-row stacked">
        <button class="btn-primary-sm" data-testid="toggle-new-group-grp" @click="toggleNewGroupGrp">+ New group</button>
        <input
          v-model="search"
          class="search-input"
          placeholder="Search groups…"
          data-testid="group-search"
        />
      </div>
      <div v-if="showNewGroupGrp" class="inline-form" style="padding: 6px 8px;">
        <input
          v-model="newGroupNameGrp"
          class="inline-input"
          placeholder="group-name"
          data-testid="new-group-name-grp"
          @keydown.enter="createNewGroup"
        />
        <button class="btn-primary-sm" :disabled="mutating" @click="createNewGroup">Create</button>
        <button class="btn-ghost-sm" @click="toggleNewGroupGrp">Cancel</button>
        <span v-if="newGroupNameGrpError" class="inline-err" data-testid="new-group-name-grp-error">{{ newGroupNameGrpError }}</span>
      </div>
      <div v-if="loading" class="list-loading">Loading…</div>
      <div
        v-for="g in filteredGroups"
        :key="g.name"
        :class="['list-item', { active: selectedGroup?.name === g.name }]"
        :data-testid="`group-row-${g.name}`"
        @click="selectGroup(g)"
      >
        <span class="item-name">{{ g.name }}</span>
        <span class="item-sub">{{ g.members.length }} member{{ g.members.length !== 1 ? 's' : '' }}</span>
      </div>
      <div v-if="!loading && filteredGroups.length === 0" class="list-empty">No groups.</div>
    </div>

    <div class="detail-pane" data-testid="group-detail-pane">
      <div v-if="!selectedGroup" class="detail-empty">Select a group to view details.</div>

      <template v-else>
        <div class="detail-section">
          <div class="detail-title">group:{{ selectedGroup.name }}</div>
          <div class="hint small">FGA has no empty groups — a group materializes on its first assignment.</div>
        </div>

        <!-- Delegation toggle -->
        <div class="detail-section">
          <div class="section-label">Delegation group</div>
          <label class="toggle-row">
            <ToggleSwitch
              :model-value="selectedGroup.delegatable"
              :disabled="mutating"
              data-testid="delegation-toggle"
              aria-label="Delegation group"
              @update:model-value="setDelegatable"
            />
            <span class="hint small">Members may delegate tasks to each other.</span>
          </label>
        </div>

        <!-- Members -->
        <div class="detail-section">
          <div class="section-label">Members</div>
          <div class="chips-row">
            <span
              v-for="ref in selectedGroup.members"
              :key="ref"
              class="chip"
              :data-testid="`group-member-${ref}`"
            >
              {{ userName(ref) }}
              <button class="chip-x" :disabled="mutating" @click="removeMemberFromGroup(ref)" title="Remove member" :data-testid="`group-remove-member-${ref}`">×</button>
            </span>
            <span v-if="selectedGroup.members.length === 0" class="no-items">No members.</span>
          </div>
          <!-- Bulk add members (checkbox list) -->
          <div v-if="memberDropOptions.length > 0" class="bulk-add-block">
            <div class="section-label-sm">Add members</div>
            <div class="bulk-checkbox-list">
              <label
                v-for="p in memberDropOptions"
                :key="p.id"
                class="bulk-check-item"
              >
                <ToggleSwitch
                  :model-value="bulkMemberSelected.has(p.id)"
                  :disabled="mutating"
                  :data-testid="`bulk-member-cb-${p.id}`"
                  :aria-label="p.displayName ?? p.email ?? p.id"
                  @update:model-value="toggleBulkMember(p.id)"
                />
                {{ p.displayName ?? p.email ?? p.id }}
              </label>
            </div>
            <div class="add-row">
              <button
                class="btn-primary-sm"
                :disabled="mutating || bulkMemberSelected.size === 0"
                @click="bulkAddMembers"
                data-testid="bulk-add-members-btn"
              >Add selected ({{ bulkMemberSelected.size }})</button>
            </div>
            <div v-if="bulkMemberError" class="inline-err">{{ bulkMemberError }}</div>
          </div>
          <div v-else class="add-row">
            <select
              class="dropdown"
              :disabled="mutating"
              @change="(e) => { if (e.target.value) { addMemberToGroup(e.target.value); e.target.value = ''; } }"
              data-testid="add-member-select"
            >
              <option value="">Add member…</option>
              <option v-for="p in memberDropOptions" :key="p.id" :value="p.id" :data-testid="`group-add-member-${p.id}`">
                {{ p.displayName ?? p.email ?? p.id }}
              </option>
            </select>
          </div>
        </div>

        <!-- Managers -->
        <div class="detail-section">
          <div class="section-label">Managers</div>
          <div class="chips-row">
            <span
              v-for="ref in selectedGroup.managers"
              :key="ref"
              class="chip chip-mgr"
            >
              {{ userName(ref) }}
              <button class="chip-x" :disabled="mutating" @click="removeManagerFromGroup(ref)" title="Remove manager">×</button>
            </span>
            <span v-if="selectedGroup.managers.length === 0" class="no-items">No managers.</span>
          </div>
          <div class="add-row">
            <select
              class="dropdown"
              :disabled="mutating"
              @change="(e) => { if (e.target.value) { addManagerToGroup(e.target.value); e.target.value = ''; } }"
              data-testid="add-manager-select"
            >
              <option value="">Add manager…</option>
              <option v-for="p in managerDropOptions" :key="p.id" :value="p.id" :data-testid="`group-manager-opt-${p.id}`">
                {{ p.displayName ?? p.email ?? p.id }}
              </option>
            </select>
          </div>
        </div>

        <!-- Tools granted -->
        <div class="detail-section">
          <div class="section-label">Tools Granted</div>
          <div class="chips-row">
            <span
              v-for="sid in selectedGroup.skills"
              :key="sid"
              class="chip chip-skill"
            >
              {{ sid }}
              <button class="chip-x" :disabled="mutating" @click="removeSkillFromGroup(sid)" title="Revoke tool">×</button>
            </span>
            <span v-if="selectedGroup.skills.length === 0" class="no-items">No tools.</span>
          </div>
          <!-- Bulk add tools (checkbox list) -->
          <div v-if="skillDropOptions.length > 0" class="bulk-add-block">
            <div class="section-label-sm">Grant tools</div>
            <div class="bulk-checkbox-list">
              <label
                v-for="sk in skillDropOptions"
                :key="sk.toolId"
                class="bulk-check-item"
              >
                <ToggleSwitch
                  :model-value="bulkSkillSelected.has(sk.toolId)"
                  :disabled="mutating"
                  :data-testid="`bulk-skill-cb-${sk.toolId}`"
                  :aria-label="sk.toolId"
                  @update:model-value="toggleBulkSkill(sk.toolId)"
                />
                {{ sk.toolId }}
                <span v-if="sk.scope" class="scope-badge">{{ sk.scope }}</span>
              </label>
            </div>
            <div class="add-row">
              <button
                class="btn-primary-sm"
                :disabled="mutating || bulkSkillSelected.size === 0"
                @click="bulkAddSkills"
                data-testid="bulk-add-skills-btn"
              >Grant selected ({{ bulkSkillSelected.size }})</button>
            </div>
            <div v-if="bulkSkillError" class="inline-err">{{ bulkSkillError }}</div>
          </div>
          <div v-else class="add-row">
            <select
              class="dropdown"
              :disabled="mutating"
              @change="(e) => { if (e.target.value) { addSkillToGroup(e.target.value); e.target.value = ''; } }"
              data-testid="add-skill-select"
            >
              <option value="">Grant tool…</option>
              <option v-for="sk in skillDropOptions" :key="sk.toolId" :value="sk.toolId" :data-testid="`group-skill-opt-${sk.toolId}`">
                {{ sk.toolId }}
                <template v-if="sk.scope"> ({{ sk.scope }})</template>
              </option>
            </select>
          </div>
        </div>

        <!-- Skills granted (bundles) -->
        <div class="detail-section">
          <div class="section-label">Skills Granted</div>
          <div class="chips-row">
            <span
              v-for="bid in selectedGroup.skillBundles ?? []"
              :key="bid"
              class="chip chip-skill"
              :title="bid"
            >
              {{ bundleName(bid) }}
              <button class="chip-x" :disabled="mutating" @click="removeBundleFromGroup(bid)" title="Revoke skill">×</button>
            </span>
            <span v-if="(selectedGroup.skillBundles ?? []).length === 0" class="no-items">No skills.</span>
          </div>
          <div class="add-row">
            <select
              class="dropdown"
              :disabled="mutating"
              @change="(e) => { if (e.target.value) { addBundleToGroup(e.target.value); e.target.value = ''; } }"
              data-testid="add-bundle-select"
            >
              <option value="">Grant skill…</option>
              <option v-for="b in bundleDropOptions" :key="b.id" :value="b.id" :data-testid="`group-bundle-opt-${b.id}`">
                {{ b.name ?? b.id }}
              </option>
            </select>
          </div>
        </div>

        <!-- Agents usable by this group -->
        <div class="detail-section">
          <div class="section-label">Agents usable</div>
          <div
            v-for="ref in selectedGroup.agentsUsable"
            :key="ref"
            class="agent-item"
          >{{ agentName(ref) }}</div>
          <div v-if="selectedGroup.agentsUsable.length === 0" class="no-items">None.</div>
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
.master-header-row {
  display: flex; gap: 6px; padding: 8px;
}
.master-header-row.stacked { flex-direction: column; align-items: stretch; }
.master-header-row .search-input { flex: 1; }
.search-input {
  width: 100%; box-sizing: border-box;
  background: var(--bg); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 6px 9px; font-size: 13px; margin: 8px;
  width: calc(100% - 16px);
}
.master-header-row .search-input { margin: 0; width: auto; }
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
.chip-mgr    { border-color: var(--border); color: var(--text-muted); }
.chip-skill  { border-color: #a6ffa1; color: #a6ffa1; background: var(--fill-muted); }
.chip-x {
  background: none; border: none; color: inherit; cursor: pointer;
  padding: 0 2px; font-size: 14px; line-height: 1; opacity: 0.7;
}
.chip-x:hover:not(:disabled) { opacity: 1; }
.chip-x:disabled { cursor: default; opacity: 0.3; }

.no-items { color: var(--text-muted); font-size: 12px; font-style: italic; }

/* ── add-row / inline-form ──────────────────────────────────────────── */
.add-row {
  display: flex; gap: 6px; align-items: center; flex-wrap: wrap; margin-top: 6px;
}
.dropdown {
  background: var(--bg); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 5px 9px; font-size: 13px; min-width: 180px; cursor: pointer;
}
.inline-form {
  display: flex; gap: 6px; align-items: center; flex-wrap: wrap; margin-top: 6px;
}
.inline-input {
  background: var(--bg); color: var(--text);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 5px 9px; font-size: 13px; font-family: var(--font-mono); min-width: 180px;
}
.inline-err { color: var(--danger); font-size: 12px; }

/* ── agents ────────────────────────────────────────────────────────── */
.agent-item { font-size: 13px; padding: 3px 0; }

/* ── buttons ─────────────────────────────────────────────────────────── */
.btn-primary-sm {
  display: inline-flex; align-items: center; gap: 4px;
  background: var(--accent); color: var(--text-on-accent);
  border: none; border-radius: var(--radius-sm);
  padding: 5px 10px; font-size: 12px; cursor: pointer; white-space: nowrap;
}
.btn-primary-sm:hover:not(:disabled) { background: var(--accent-hover); }
.btn-primary-sm:disabled { opacity: 0.5; cursor: default; }

.btn-ghost-sm {
  background: transparent; color: var(--text); border: 1px solid var(--border);
  border-radius: var(--radius-sm); padding: 5px 10px; font-size: 12px; cursor: pointer; white-space: nowrap;
}
.btn-ghost-sm:hover:not(:disabled) { background: var(--bg-hover); }
.btn-ghost-sm:disabled { opacity: 0.5; cursor: default; }

/* ── bulk add ────────────────────────────────────────────────────────── */
.bulk-add-block {
  margin-top: 8px; border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 10px 12px;
}
.section-label-sm {
  font-size: 11px; font-weight: 600; text-transform: uppercase;
  letter-spacing: 0.06em; color: var(--text-muted); margin-bottom: 8px;
}
.bulk-checkbox-list {
  display: flex; flex-direction: column; gap: 4px; max-height: 180px;
  overflow-y: auto; margin-bottom: 10px;
}
.bulk-check-item {
  display: flex; align-items: center; gap: 8px;
  font-size: 13px; cursor: pointer; padding: 2px 0;
}

/* ── scope badge ────────────────────────────────────────────────────── */
.scope-badge {
  display: inline-block; background: var(--fill-muted); color: var(--text-muted);
  border: 1px solid var(--border); border-radius: var(--radius-sm);
  padding: 0 6px; font-size: 11px; margin-left: 4px; font-family: var(--font-mono);
}
</style>
