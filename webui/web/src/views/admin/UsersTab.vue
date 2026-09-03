<script setup>
import { ref, computed, inject } from "vue";
import { deriveUser, effectiveSkills, effectiveSkillBundles } from "./access-derive.js";
import { assignRole, revokeRole } from "../../api/admin.js";
import { subjectRef } from "../../sections.js";
import { ACCESS_CTX } from "./useAccessControl.js";
import { useToast } from "../../components/ui/useToast.js";

const { push: toast } = useToast();

const ctx = inject(ACCESS_CTX);
const {
  loading, tuples, principals, derivedGroups, tRef,
  userPrincipals, userName, agentName, bundleName,
  doAssign, doRevoke, mutating, mutError,
  selectedUser,
} = ctx;

// ── tab-local state ───────────────────────────────────────────────────────────

const search = ref("");

const filteredUsers = computed(() => {
  const q = search.value.toLowerCase();
  if (!q) return userPrincipals.value;
  return userPrincipals.value.filter((p) => {
    const dn = (p.displayName ?? "").toLowerCase();
    const em = (p.email ?? "").toLowerCase();
    const id = (p.id ?? "").toLowerCase();
    return dn.includes(q) || em.includes(q) || id.includes(q);
  });
});

function selectUser(p) {
  selectedUser.value = p;
}

const selUserData = computed(() => {
  if (!selectedUser.value) return null;
  return deriveUser(selectedUser.value.id, tuples.value);
});

const selUserSkills = computed(() => {
  if (!selectedUser.value) return [];
  return effectiveSkills(selectedUser.value.id, tuples.value);
});

const selUserBundles = computed(() => {
  if (!selectedUser.value) return [];
  return effectiveSkillBundles(selectedUser.value.id, tuples.value);
});

// ── add-to-group ──────────────────────────────────────────────────────────────

const GROUP_NAME_RE     = /^[a-z0-9][a-z0-9._-]{0,63}$/;
const groupDropSearch   = ref("");
const newGroupName      = ref("");
const showNewGroupInput = ref(false);
const newGroupNameError = ref("");

const groupDropOptions = computed(() => {
  const q = groupDropSearch.value.toLowerCase();
  const opts = derivedGroups.value
    .filter((g) => !g.members.includes(selectedUser.value?.id))
    .filter((g) => !q || g.name.includes(q));
  return opts;
});

function toggleNewGroupInput() {
  showNewGroupInput.value = !showNewGroupInput.value;
  newGroupName.value      = "";
  newGroupNameError.value = "";
}

async function addUserToGroup(groupNameVal) {
  if (!selectedUser.value) return;
  await doAssign(selectedUser.value.id, "member", `group:${groupNameVal}`);
}

async function createGroupAndAdd() {
  newGroupNameError.value = "";
  const n = newGroupName.value.trim();
  if (!GROUP_NAME_RE.test(n)) {
    newGroupNameError.value = "Name must match ^[a-z0-9][a-z0-9._-]{0,63}$";
    return;
  }
  showNewGroupInput.value = false;
  newGroupName.value      = "";
  await addUserToGroup(n);
}

async function removeUserFromGroup(groupNameVal) {
  if (!selectedUser.value) return;
  await doRevoke(selectedUser.value.id, "member", `group:${groupNameVal}`);
}

// ── tenant role ───────────────────────────────────────────────────────────────

const tenantRoleLoading = ref(false);

async function setTenantRole(relation) {
  if (!selectedUser.value || !tRef.value) return;
  tenantRoleLoading.value = true;
  const other = relation === "admin" ? "member" : "admin";
  const existing = selUserData.value?.tenantRoles ?? [];
  try {
    for (const r of existing) {
      if (r.relation === other || r.relation === relation) {
        await revokeRole({ user: subjectRef(selectedUser.value.id), relation: r.relation, object: tRef.value });
      }
    }
    await assignRole({ user: subjectRef(selectedUser.value.id), relation, object: tRef.value });
    toast("ok", `Tenant role set to ${relation}.`);
    await ctx.load();
    const refreshed = principals.value.find((p) => p.id === selectedUser.value?.id);
    if (refreshed) selectedUser.value = refreshed;
  } catch (e) {
    mutError.value = e.message;
  } finally {
    tenantRoleLoading.value = false;
  }
}

async function removeTenantRole(relation) {
  if (!selectedUser.value || !tRef.value) return;
  await doRevoke(selectedUser.value.id, relation, tRef.value);
}

// ── direct skill revoke ───────────────────────────────────────────────────────

async function revokeDirectSkill(skillIdVal) {
  if (!selectedUser.value) return;
  await doRevoke(selectedUser.value.id, "permitted_group", `skill:${skillIdVal}`);
}
</script>

<template>
  <div class="master-detail" data-testid="pane-Users">
    <div class="master-pane">
      <input
        v-model="search"
        class="search-input"
        placeholder="Search users…"
        data-testid="user-search"
      />
      <div v-if="loading" class="list-loading">Loading…</div>
      <div
        v-for="p in filteredUsers"
        :key="p.id"
        :class="['list-item', { active: selectedUser?.id === p.id }]"
        :data-testid="`user-row-${p.id}`"
        @click="selectUser(p)"
      >
        <span class="item-name">{{ p.displayName ?? p.email ?? p.id.replace(/^user:/, '') }}</span>
        <span class="item-sub">{{ p.email }}</span>
      </div>
      <div v-if="!loading && filteredUsers.length === 0" class="list-empty">No users.</div>
    </div>

    <div class="detail-pane" data-testid="user-detail-pane">
      <div v-if="!selectedUser" class="detail-empty">Select a user to view details.</div>

      <template v-else>
        <!-- Identity header -->
        <div class="detail-section">
          <div class="detail-title">{{ selectedUser.displayName ?? selectedUser.email ?? selectedUser.id }}</div>
          <div class="detail-meta">
            <span v-if="selectedUser.email" class="meta-item">{{ selectedUser.email }}</span>
            <span class="meta-item mono small" :title="selectedUser.id">{{ selectedUser.id }}</span>
          </div>
        </div>

        <!-- Groups -->
        <div class="detail-section">
          <div class="section-label">Groups</div>
          <div class="chips-row">
            <span
              v-for="gname in selUserData?.groups ?? []"
              :key="gname"
              class="chip"
              :data-testid="`user-group-chip-${gname}`"
            >
              {{ gname }}
              <button class="chip-x" :disabled="mutating" @click="removeUserFromGroup(gname)" title="Remove from group" :data-testid="`user-remove-group-${gname}`">×</button>
            </span>
            <span v-if="(selUserData?.groups ?? []).length === 0" class="no-items">No groups.</span>
          </div>

          <!-- Add to group -->
          <div class="add-row">
            <select
              class="dropdown"
              :disabled="mutating"
              @change="(e) => { if (e.target.value) { addUserToGroup(e.target.value); e.target.value = ''; } }"
              data-testid="add-to-group-select"
            >
              <option value="">Add to group…</option>
              <option v-for="g in groupDropOptions" :key="g.name" :value="g.name" :data-testid="`add-to-group-opt-${g.name}`">{{ g.name }}</option>
            </select>
            <button class="btn-ghost-sm" data-testid="toggle-new-group-input" @click="toggleNewGroupInput">+ New group</button>
          </div>
          <div v-if="showNewGroupInput" class="inline-form">
            <input
              v-model="newGroupName"
              class="inline-input"
              placeholder="new-group-name"
              data-testid="new-group-name-input"
              @keydown.enter="createGroupAndAdd"
            />
            <button class="btn-primary-sm" :disabled="mutating" @click="createGroupAndAdd">Create &amp; Add</button>
            <button class="btn-ghost-sm" @click="toggleNewGroupInput">Cancel</button>
            <span v-if="newGroupNameError" class="inline-err" data-testid="new-group-name-error">{{ newGroupNameError }}</span>
          </div>
        </div>

        <!-- Tenant role -->
        <div class="detail-section">
          <div class="section-label">
            Tenant role
            <span v-if="!tRef" class="hint">(no tenant tuple — controls disabled)</span>
          </div>
          <div class="chips-row">
            <span
              v-for="tr in selUserData?.tenantRoles ?? []"
              :key="tr.relation + tr.object"
              class="chip chip-tenant"
            >
              {{ tr.relation }}
              <button class="chip-x" :disabled="mutating || !tRef" @click="removeTenantRole(tr.relation)" title="Revoke">×</button>
            </span>
            <span v-if="(selUserData?.tenantRoles ?? []).length === 0" class="no-items">No tenant role.</span>
          </div>
          <div v-if="tRef" class="add-row">
            <button
              class="btn-ghost-sm"
              :disabled="mutating || tenantRoleLoading"
              data-testid="set-role-member"
              @click="setTenantRole('member')"
            >Set member</button>
            <button
              class="btn-ghost-sm"
              :disabled="mutating || tenantRoleLoading"
              data-testid="set-role-admin"
              @click="setTenantRole('admin')"
            >Set admin</button>
          </div>
        </div>

        <!-- Effective tools -->
        <div class="detail-section">
          <div class="section-label">Effective Tools</div>
          <table v-if="selUserSkills.length > 0" class="skills-table">
            <thead>
              <tr>
                <th>Tool</th>
                <th>Provenance</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="sk in selUserSkills" :key="sk.skillId">
                <td class="mono">{{ sk.skillId }}</td>
                <td>
                  <span
                    v-for="(prov, pi) in sk.provenance"
                    :key="pi"
                    :class="['prov-badge', prov.kind === 'direct' ? 'prov-direct' : 'prov-group']"
                  >
                    {{ prov.kind === "direct" ? "direct" : `via ${prov.group}` }}
                  </span>
                </td>
                <td class="right">
                  <button
                    v-if="sk.provenance.some((p) => p.kind === 'direct')"
                    class="btn-danger-sm"
                    :disabled="mutating"
                    :data-testid="`revoke-direct-skill-${sk.skillId}`"
                    @click="revokeDirectSkill(sk.skillId)"
                  >Revoke</button>
                </td>
              </tr>
            </tbody>
          </table>
          <div v-else class="no-items">No tools.</div>
        </div>

        <!-- Assigned skills (bundles) -->
        <div class="detail-section">
          <div class="section-label">Assigned Skills</div>
          <table v-if="selUserBundles.length > 0" class="skills-table">
            <thead>
              <tr>
                <th>Skill</th>
                <th>Provenance</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="b in selUserBundles" :key="b.bundleId">
                <td>{{ bundleName(b.bundleId) }}</td>
                <td>
                  <span
                    v-for="(prov, pi) in b.provenance"
                    :key="pi"
                    :class="['prov-badge', prov.kind === 'direct' ? 'prov-direct' : 'prov-group']"
                  >
                    {{ prov.kind === "direct" ? "direct" : `via ${prov.group}` }}
                  </span>
                </td>
              </tr>
            </tbody>
          </table>
          <div v-else class="no-items">No skills.</div>
        </div>

        <!-- Agents -->
        <div class="detail-section">
          <div class="section-label">Agents</div>
          <div class="agents-row">
            <div class="agents-col">
              <div class="col-label">Owned Agents</div>
              <div v-for="ref in selUserData?.agentsOwned ?? []" :key="ref" class="agent-item">
                {{ agentName(ref) }}
              </div>
              <div v-if="(selUserData?.agentsOwned ?? []).length === 0" class="no-items">None.</div>
            </div>
            <div class="agents-col">
              <div class="col-label">Assigned Agents</div>
              <div v-for="ref in selUserData?.agentsUsable ?? []" :key="ref" class="agent-item">
                {{ agentName(ref) }}
              </div>
              <div v-if="(selUserData?.agentsUsable ?? []).length === 0" class="no-items">None.</div>
            </div>
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
.chip-tenant { border-color: var(--accent); color: var(--accent); background: var(--fill-accent); }
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

/* ── agents ────────────────────────────────────────────────────────── */
.agents-row { display: flex; gap: 24px; }
.agents-col { flex: 1; }
.col-label  { font-size: 11px; font-weight: 600; text-transform: uppercase; letter-spacing: 0.05em; color: var(--text-muted); margin-bottom: 6px; }
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

.btn-danger-sm {
  display: inline-flex; align-items: center; gap: 4px;
  background: transparent; color: var(--danger);
  border: 1px solid var(--danger); border-radius: var(--radius-sm);
  padding: 3px 10px; cursor: pointer; font-size: 12px; opacity: 0.8;
}
.btn-danger-sm:hover:not(:disabled) { background: var(--fill-danger); opacity: 1; }
.btn-danger-sm:disabled { opacity: 0.3; cursor: default; }

.mono { font-family: var(--font-mono); }
.right { text-align: right; }
</style>
