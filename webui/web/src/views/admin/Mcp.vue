<script setup>
import { ref, computed, onMounted } from "vue";
import {
  listAssignments,
  assignRole,
  revokeRole,
  listMcpConnections,
  addMcpConnection,
  updateMcpConnection,
  deleteMcpConnection,
} from "../../api/admin.js";
import Icon from "../../components/Icon.vue";
import DataTable from "../../components/ui/DataTable.vue";
import EmptyState from "../../components/ui/EmptyState.vue";
import { useToast } from "../../components/ui/useToast.js";

const { push: toast } = useToast();

const connections = ref([]);
const forbidden   = ref(false);
const loading     = ref(false);
const error       = ref("");

// form state — also used for editing (editId non-null = edit mode)
const editId      = ref(null);
const name        = ref("");
const url         = ref("");
const transport   = ref("streamable_http");
const authType    = ref("none");
const bearerToken = ref("");

async function load() {
  loading.value   = true;
  error.value     = "";
  forbidden.value = false;
  try {
    const data = await listMcpConnections();
    if (data.forbidden) { forbidden.value = true; return; }
    connections.value = data.connections ?? [];
  } catch (e) {
    error.value = e.message;
  } finally {
    loading.value = false;
  }
}

onMounted(load);

function startEdit(conn) {
  editId.value      = conn.id;
  name.value        = conn.name;
  url.value         = conn.url;
  transport.value   = conn.transport;
  authType.value    = conn.authType;
  bearerToken.value = "";
}

function cancelEdit() {
  editId.value      = null;
  name.value        = "";
  url.value         = "";
  transport.value   = "streamable_http";
  authType.value    = "none";
  bearerToken.value = "";
}

async function submit() {
  if (!name.value.trim() || !url.value.trim()) {
    error.value = "name and URL are required";
    return;
  }
  error.value = "";
  const body = {
    name:      name.value.trim(),
    url:       url.value.trim(),
    transport: transport.value,
    authType:  authType.value,
  };
  // only include bearerToken when authType=bearer and admin typed a value
  if (authType.value === "bearer" && bearerToken.value) {
    body.bearerToken = bearerToken.value;
  }
  try {
    if (editId.value) {
      const resp = await updateMcpConnection(editId.value, body);
      if (resp.forbidden) {
        error.value = resp.error || "You are not a tenant admin.";
        return;
      }
      toast("ok", "Connection saved.");
    } else {
      const resp = await addMcpConnection(body);
      if (resp.forbidden) {
        error.value = resp.error || "You are not a tenant admin.";
        return;
      }
      toast("ok", "Connection added.");
    }
    cancelEdit();
    await load();
  } catch (e) {
    error.value = e.message;
  }
}

async function remove(conn) {
  error.value = "";
  try {
    const resp = await deleteMcpConnection(conn.id);
    if (resp.forbidden) {
      error.value = resp.error || "You are not a tenant admin.";
      return;
    }
    toast("ok", "Connection deleted.");
    await load();
  } catch (e) {
    error.value = e.message;
  }
}

// ── Access (AG-02) ───────────────────────────────────────────────────────────
// A human using an agent needs their own grant on the connector next to the
// agent's: a group (permitted_group) or a direct user grant (permitted_user).
// Agent grants (permitted_agent) are managed on the agent.
const accessConn  = ref(null);
const tuples      = ref([]);
const grantUser   = ref("");
const grantGroup  = ref("");

const accessGrants = computed(() => {
  if (!accessConn.value) return [];
  const obj = `mcp_connector:${accessConn.value.id}`;
  return tuples.value.filter((t) => t.object === obj &&
    ["permitted_agent", "permitted_group", "permitted_user"].includes(t.relation));
});

async function loadAccess() {
  const data = await listAssignments();
  tuples.value = data?.tuples ?? [];
}

async function openAccess(conn) {
  error.value = "";
  accessConn.value = accessConn.value?.id === conn.id ? null : conn;
  if (!accessConn.value) return;
  try { await loadAccess(); } catch (e) { error.value = e.message; }
}

async function mutateAccess(fn, tuple, msg) {
  error.value = "";
  try {
    await fn(tuple);
    toast("ok", msg);
    await loadAccess();
  } catch (e) {
    error.value = e.message;
  }
}

async function grant(relation) {
  const raw = (relation === "permitted_user" ? grantUser.value : grantGroup.value).trim();
  if (!raw) return;
  const user = relation === "permitted_user"
    ? `user:${raw.replace(/^user:/, "")}`
    : `group:${raw.replace(/^group:/, "").replace(/#member$/, "")}#member`;
  await mutateAccess(assignRole, { user, relation, object: `mcp_connector:${accessConn.value.id}` }, "Access granted.");
  if (relation === "permitted_user") grantUser.value = ""; else grantGroup.value = "";
}

function revokeGrant(t) {
  return mutateAccess(revokeRole, { user: t.user, relation: t.relation, object: t.object }, "Access revoked.");
}

const TABLE_COLS = [
  { key: "name",      label: "Name" },
  { key: "url",       label: "URL" },
  { key: "transport", label: "Transport" },
  { key: "authType",  label: "Auth" },
  { key: "_actions",  label: "",           width: "230px" },
];
</script>

<template>
  <div class="view">
    <div class="view-header">
      <Icon name="server" class="view-icon" />
      <h1>MCP Connections</h1>
    </div>

    <!-- not-an-admin empty-state -->
    <EmptyState
      v-if="forbidden"
      data-testid="forbidden"
      icon="connections"
      message="You are not a tenant admin."
    />

    <template v-else>
      <div v-if="error" data-testid="error-banner" class="banner-err">{{ error }}</div>

      <!-- Add / edit form -->
      <div class="form">
        <input
          v-model="name"
          placeholder="name"
          class="f-name"
          data-testid="conn-name"
        />
        <input
          v-model="url"
          placeholder="https://…"
          class="f-url"
          data-testid="conn-url"
        />
        <select v-model="transport" data-testid="conn-transport">
          <option value="streamable_http">streamable_http</option>
          <option value="sse">sse</option>
        </select>
        <select v-model="authType" data-testid="conn-auth-type">
          <option value="none">none</option>
          <option value="bearer">bearer</option>
        </select>
        <input
          v-if="authType === 'bearer'"
          v-model="bearerToken"
          type="password"
          :placeholder="editId ? 'new token (leave blank to keep existing)' : 'bearer token'"
          class="f-bearer"
          data-testid="conn-bearer"
        />
        <button class="btn-primary" data-testid="add-conn-btn" @click="submit">
          <Icon name="plus" /> {{ editId ? "Save" : "Add" }}
        </button>
        <button v-if="editId" class="btn-ghost" @click="cancelEdit">Cancel</button>
      </div>

      <DataTable
        :columns="TABLE_COLS"
        :rows="connections"
        :loading="loading"
        empty-text="No MCP connections yet."
        :row-attrs="{ 'data-testid': 'conn-row' }"
      >
        <template #row="{ row }">
          <td class="mono">{{ row.name }}</td>
          <td class="mono">{{ row.url }}</td>
          <td><span class="badge">{{ row.transport }}</span></td>
          <td><span class="badge">{{ row.authType }}</span></td>
          <td class="right actions">
            <button
              :data-testid="`access-${row.id}`"
              class="btn-secondary-sm"
              @click="openAccess(row)"
            >
              Access
            </button>
            <button
              :data-testid="`edit-${row.id}`"
              class="btn-secondary-sm"
              @click="startEdit(row)"
            >
              Edit
            </button>
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

      <section v-if="accessConn" class="access" data-testid="conn-access-panel">
        <h2>Access to {{ accessConn.name }}</h2>
        <p class="muted">
          An agent reaches this server only if both it and the person it acts for are granted.
          Grant people here, directly or through a group; grant agents on the agent.
        </p>
        <ul class="grants">
          <li v-for="t in accessGrants" :key="t.user + t.relation" data-testid="conn-grant-row">
            <span class="badge">{{ t.relation }}</span>
            <span class="mono">{{ t.user }}</span>
            <button
              v-if="t.relation !== 'permitted_agent'"
              :data-testid="`conn-grant-revoke-${t.user}`"
              class="btn-danger-sm"
              @click="revokeGrant(t)"
            >
              Revoke
            </button>
          </li>
          <li v-if="!accessGrants.length" class="muted">No grants yet.</li>
        </ul>
        <div class="form">
          <input v-model="grantUser" placeholder="user email" data-testid="conn-grant-user-input" />
          <button class="btn-primary" data-testid="conn-grant-user-btn" @click="grant('permitted_user')">Grant user</button>
          <input v-model="grantGroup" placeholder="group name" data-testid="conn-grant-group-input" />
          <button class="btn-primary" data-testid="conn-grant-group-btn" @click="grant('permitted_group')">Grant group</button>
        </div>
      </section>
    </template>
  </div>
</template>

<style scoped>
.view { padding: 24px 32px; max-width: 1000px; }
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
.form .f-name   { width: 140px; }
.form .f-url    { width: 240px; }
.form .f-bearer { width: 180px; }

.btn-primary {
  display: inline-flex; align-items: center; gap: 4px;
  background: var(--accent); color: var(--text-on-accent);
  border: none; border-radius: var(--radius-sm);
  padding: 7px 13px; font-size: 13px; cursor: pointer;
}
.btn-primary:hover { background: var(--accent-hover); }

.btn-ghost {
  background: transparent; color: var(--text); border: 1px solid var(--border);
  border-radius: var(--radius-sm); padding: 7px 13px; font-size: 13px; cursor: pointer;
}
.btn-ghost:hover { background: var(--bg-hover); }

.btn-secondary-sm {
  background: transparent; color: var(--text-muted); border: 1px solid var(--border);
  border-radius: var(--radius-sm); padding: 3px 10px; cursor: pointer; font-size: 12px;
}
.btn-secondary-sm:hover { background: var(--bg-hover); color: var(--text); }

.btn-danger-sm {
  display: inline-flex; align-items: center; gap: 4px;
  background: transparent; color: var(--danger);
  border: 1px solid var(--danger); border-radius: var(--radius-sm);
  padding: 3px 10px; cursor: pointer; font-size: 12px; opacity: 0.8;
}
.btn-danger-sm:hover { background: var(--fill-danger); opacity: 1; }

.mono { font-family: var(--font-mono); word-break: break-all; }
.right { text-align: right; }
.actions { display: flex; gap: 6px; justify-content: flex-end; }

.access { margin-top: 24px; }
.access h2 { margin: 0 0 6px; font-size: 16px; font-weight: 600; }
.grants { list-style: none; padding: 0; margin: 12px 0; display: flex; flex-direction: column; gap: 6px; }
.grants li { display: flex; align-items: center; gap: 8px; }

.badge {
  border-radius: var(--radius-sm); padding: 1px 7px;
  font-size: 12px; background: var(--fill-muted);
  color: var(--text-muted); border: 1px solid var(--border);
}
</style>
